// Package main 是 sysmanifest：把一棵已构建好的系统包目录树补齐成 nervud 能
// 装载的形态——计算 digest、填 manifest.json、产出 Ed25519 签名的 manifest.sig。
//
// # 为什么用 Go 而不是复用 nervus-packaging 的 Kotlin signing-lib
//
// 本仓库是纯 Go，而 manifest.sig 只是一份 JSON 加一个 Ed25519 签名，标准库
// crypto/ed25519 直接能产。为签名往构建里拖一条 JVM 工具链不划算。
//
// 代价是这里的结构定义与内核的 pkgregistry 是**两份**，有漂移风险。缓解手段是
// manifest_test.go 里的 golden：任何字段改动都会让它失败，逼人同时看两边。
//
// # 它不产出 .nspkg
//
// 系统包在镜像里是**解包后的目录树**（内核 scan.go glob 的是 */manifest.json），
// 不是 zstd+tar 的 .nspkg。后者是动态安装用的格式，两条路不能混。
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// manifestSchemaV1 必须与内核 pkgregistry.manifestSchemaV1 一致。
// 内核在两段解析的第一段就用它挡下不认识的 schema。
const manifestSchemaV1 = 1

// manifestSigDomain 是签名的域分隔前缀，必须与内核 sigblock.go 逐字节一致。
//
// 域分隔的意义：同一把私钥可能在别处签别的东西（血统节点、trust bundle），
// 没有前缀的话一个上下文里的签名可以被搬到另一个上下文里冒用。
const manifestSigDomain = "nervus-pkg-manifest-v1\x00"

// rolePlatformRelease 是核心系统服务的签名角色。
// 内核 sigblock.go 的注释：「平台：nervud + 核心系统服务」。
const rolePlatformRelease = "platform-release"

// Component 对应内核 pkgregistry.Component。
//
// 字段名与 json tag 必须完全一致：内核用 DisallowUnknownFields 解析，
// 多一个字段、拼错一个 tag，装载时就是一句含义模糊的 decode 失败。
type Component struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`    // app | service
	Runtime      string          `json:"runtime"` // native | jvm
	Entry        string          `json:"entry"`
	NativeLibDir string          `json:"native_lib_dir,omitempty"`
	LaunchMode   string          `json:"launch_mode"` // always-on | on-demand | manual
	Criticality  string          `json:"criticality,omitempty"`
	Disableable  bool            `json:"disableable,omitempty"`
	Exports      []Export        `json:"exports,omitempty"`
	Interfaces   []string        `json:"interfaces,omitempty"`
	IdleTimeout  int             `json:"idle_timeout_sec,omitempty"`
	Limits       ComponentLimits `json:"limits,omitempty"`

	// Privileged / Capabilities / AddressFamilies 是组件请求的 Linux 层特权。
	//
	// 本工具【只透传不裁决】：合法性由内核的白名单判定（pkgregistry/
	// privilege.go），能不能真拿到由内核按包来源判定（只有系统镜像包拿得到）。
	// 这里加字段只是为了让严格 JSON 解码认识它们——不加的话，一个填了
	// privileged 的 manifest 在打包这一步就以 "unknown field" 失败，
	// 而那个错误看起来像模板写错了。
	Privileged      bool     `json:"privileged,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	AddressFamilies []string `json:"address_families,omitempty"`
}

// Export 声明本组件对外提供的接口。
type Export struct {
	Interface  string `json:"interface"`
	Visibility string `json:"visibility,omitempty"`
}

// ProviderArtifactsRef 指向包内的数据驱动 Provider 契约，对应内核
// pkgregistry.ProviderArtifactsRef。
//
// Descriptor 是 ProviderDescriptor 的确定性 protobuf bytes，Schemas 是
// InterfaceSchemaBundleSet 的确定性 protobuf bytes。两个文件由服务自己的
// providergen 产出（见 <服务目录>/providergen/），本工具只负责把它们算进
// digests 并写进 manifest。
//
// 【导出接口的包必须有它】：内核 loadRequiredProviderArtifacts 对
// 「有 exports 却没有 provider」直接返回 ErrProviderArtifactsRequired。
// 唯一的例外是 nervus.pkgmanagerd 的兼容桥，而那条桥正在被拆掉。
type ProviderArtifactsRef struct {
	Descriptor string `json:"descriptor"`
	Schemas    string `json:"schemas"`
}

// ComponentLimits 是组件的资源上限，会被翻成 systemd 的
// MemoryMax / CPUQuotaPerSecUSec / TasksMax。零值表示不设该项。
type ComponentLimits struct {
	MemoryMaxMB     uint64 `json:"memory_max_mb,omitempty"`
	CPUQuotaPercent uint32 `json:"cpu_quota_percent,omitempty"`
	TasksMax        uint32 `json:"tasks_max,omitempty"`
}

// Manifest 对应内核 pkgregistry.Manifest。
type Manifest struct {
	Schema          int               `json:"schema"`
	PackageID       string            `json:"package_id"`
	Label           string            `json:"label"`
	Labels          map[string]string `json:"labels,omitempty"`
	Icon            string            `json:"icon,omitempty"`
	Version         string            `json:"version"`
	VersionCode     uint64            `json:"version_code"`
	MinNervusAPI    uint32            `json:"min_nervus_api"`
	TargetNervusAPI uint32            `json:"target_nervus_api"`
	SupportedABIs   []string          `json:"supported_abis"`
	Permissions     []string          `json:"permissions,omitempty"`
	Components      []Component       `json:"components"`
	// Provider 的 json tag 必须与内核 pkgregistry.Manifest 的 `provider,omitempty`
	// 完全一致。内核用 DisallowUnknownFields 解析，拼错一个字母就是一句含义
	// 模糊的 decode 失败
	Provider *ProviderArtifactsRef `json:"provider,omitempty"`
	Digests  map[string]string     `json:"digests"`
}

// Signature 与 SignatureBlock 对应内核 pkgregistry 的同名类型。
type Signature struct {
	Role  string `json:"role"`
	Alg   string `json:"alg"`
	KeyID string `json:"key_id"`
	Key   string `json:"key,omitempty"`
	Sig   string `json:"sig"`
}

// SignatureBlock 是 manifest.sig 的内容。
//
// 不带 lineage：系统包随镜像发布，签名密钥的轮换跟随整镜像 OTA，不需要
// 包级的血统链。lineage 是给动态安装的第三方包做「同一个开发者换了密钥」
// 用的，那个场景在这里不存在。
type SignatureBlock struct {
	Format     int         `json:"format"`
	Signatures []Signature `json:"signatures"`
}

// keyIDOf 计算公钥的 key_id，格式必须与内核 keyIDOf 一致："sha256:" + hex。
func keyIDOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// computeDigests 遍历 root 下的全部普通文件，产出 相对路径 -> sha256 hex。
//
// 刻意排除 manifest.json 与 manifest.sig：它们不能自散列（写进去就改变了自己的
// 哈希）。内核的 VerifyDigests 同样豁免这两个文件——但那个豁免留下了「验签 A、
// 落盘 B」的缺口，内核在 install.go 里用 verifyStagingMetadata 逐字节比对补上了。
//
// 拒绝符号链接：digest 覆盖的必须是真实内容。一个指向 /etc/shadow 的符号链接
// 算出来的 digest 是「链接目标的内容」还是「链接本身」，取决于实现细节，
// 这种歧义不该出现在完整性校验里。
func computeDigests(root string) (map[string]string, error) {
	digests := make(map[string]string)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		if rel == manifestFileName || rel == signatureFileName {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: 只允许普通文件，发现 %s（符号链接/设备等一律拒绝）", rel, d.Type())
		}

		sum, err := sha256File(path)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		digests[rel] = sum
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(digests) == 0 {
		// 内核的 validate 会以 ErrNoDigests 拒绝，这里提前给出更清楚的信息
		return nil, fmt.Errorf("%s 下没有任何载荷文件", root)
	}
	return digests, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

const (
	manifestFileName  = "manifest.json"
	signatureFileName = "manifest.sig"
)

// encodeManifest 序列化 manifest。
//
// 【这份字节就是被签名的对象】，因此必须稳定：用 MarshalIndent 固定缩进，
// map 的 key 由 encoding/json 保证按字典序输出。任何重新序列化都必须得到
// 同样的字节，否则签名验不过。
//
// 落盘的也是这同一份字节，不是「再 marshal 一次」的结果——那正是内核
// verifyStagingMetadata 在防的「验签 A、落盘 B」。
func encodeManifest(m Manifest) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return append(b, '\n'), nil
}

// signManifest 用 priv 对 manifestBytes 产出 platform-release 角色的签名块。
func signManifest(manifestBytes []byte, priv ed25519.PrivateKey) SignatureBlock {
	pub := priv.Public().(ed25519.PublicKey)

	msg := append([]byte(manifestSigDomain), manifestBytes...)
	sig := ed25519.Sign(priv, msg)

	return SignatureBlock{
		Format: 1,
		Signatures: []Signature{{
			Role:  rolePlatformRelease,
			Alg:   "ed25519",
			KeyID: keyIDOf(pub),
			// 内嵌公钥：内核对非 developer 角色允许内嵌，且会与信任库里 key_id
			// 对应的公钥逐字节核对。带上它让开发期（无信任库）也能自查，
			// 生产期则多一道一致性检查。
			Key: b64(pub),
			Sig: b64(sig),
		}},
	}
}

// validateManifest 做一遍与内核 validate 等价的检查。
//
// 为什么要在这里重做一遍：内核发现问题是在【装载时】，日志里只有一句
// decode/validate 失败，而那时镜像已经烧好了。构建期挡下来，错误离原因最近。
func validateManifest(m Manifest) error {
	if m.Schema != manifestSchemaV1 {
		return fmt.Errorf("schema 必须是 %d", manifestSchemaV1)
	}
	if m.PackageID == "" {
		return fmt.Errorf("package_id 不能为空")
	}
	if m.Version == "" {
		return fmt.Errorf("version 不能为空")
	}
	if m.VersionCode == 0 {
		// 升级裁决只看 version_code，缺它就无法做防降级判断
		return fmt.Errorf("version_code 不能为 0（升级防降级裁决依赖它）")
	}
	if len(m.SupportedABIs) == 0 {
		return fmt.Errorf("supported_abis 不能为空")
	}
	for _, abi := range m.SupportedABIs {
		switch abi {
		case "linux-arm64", "linux-armv7", "linux-x86_64":
		default:
			// 内核只认这三个 canonical token，不接受 Android NDK 名（arm64-v8a）
			// 或裸 CPU 名（aarch64），也不做归一化
			return fmt.Errorf("supported_abis 含非法值 %q（只接受 linux-arm64 / linux-armv7 / linux-x86_64）", abi)
		}
	}
	if len(m.Components) == 0 {
		return fmt.Errorf("至少要声明一个 component")
	}

	seen := make(map[string]struct{}, len(m.Components))
	for _, c := range m.Components {
		if c.ID == "" {
			return fmt.Errorf("component id 不能为空")
		}
		if _, dup := seen[c.ID]; dup {
			return fmt.Errorf("component id %q 重复", c.ID)
		}
		seen[c.ID] = struct{}{}

		if c.Type != "app" && c.Type != "service" {
			return fmt.Errorf("component %q: type 必须是 app 或 service", c.ID)
		}
		if c.Runtime != "native" && c.Runtime != "jvm" {
			return fmt.Errorf("component %q: runtime 必须是 native 或 jvm", c.ID)
		}
		switch c.LaunchMode {
		case "always-on", "on-demand", "manual":
		default:
			return fmt.Errorf("component %q: launch_mode 非法 %q", c.ID, c.LaunchMode)
		}
		// 内核有同样的硬校验（ErrLaunchModeTypeMismatch）
		if c.Type == "app" && c.LaunchMode == "always-on" {
			return fmt.Errorf("component %q: app 不能 always-on", c.ID)
		}
		if c.Type == "service" && c.LaunchMode == "manual" {
			return fmt.Errorf("component %q: service 不能 manual", c.ID)
		}
		if c.Entry == "" {
			return fmt.Errorf("component %q: entry 不能为空", c.ID)
		}
		if strings.HasPrefix(c.Entry, "/") || strings.Contains(c.Entry, "..") {
			return fmt.Errorf("component %q: entry 必须是不含 .. 的包内相对路径", c.ID)
		}
		// 入口必须被完整性校验覆盖，否则等于允许运行一个未经校验的可执行文件
		if _, ok := m.Digests[c.Entry]; !ok {
			return fmt.Errorf("component %q: entry %q 未被 digests 覆盖", c.ID, c.Entry)
		}
	}

	return validateProvider(m)
}

// validateProvider 复核 Provider 契约的形状，与内核 pkgregistry 的两处校验对齐：
// manifest.go 的路径/digest 检查，以及 provider_artifacts.go 的
// 「有 exports 就必须有 provider」。
//
// 在这里挡下来的意义与本文件其它校验相同：内核发现同样的问题是在装载时，
// 那时镜像已经烧好，而错误信息（ErrProviderArtifactsRequired）离「模板漏了一段」
// 这个真实原因很远。
func validateProvider(m Manifest) error {
	if m.Provider == nil {
		for _, c := range m.Components {
			if len(c.Exports) != 0 {
				return fmt.Errorf(
					"component %q 导出了接口，但 manifest 没有 provider 段"+
						"（内核会以 ErrProviderArtifactsRequired 拒绝装载）", c.ID)
			}
		}
		return nil
	}

	for _, ref := range []struct {
		label string
		rel   string
	}{
		{"provider.descriptor", m.Provider.Descriptor},
		{"provider.schemas", m.Provider.Schemas},
	} {
		if ref.rel == "" {
			return fmt.Errorf("%s 不能为空", ref.label)
		}
		if strings.HasPrefix(ref.rel, "/") || strings.Contains(ref.rel, "..") {
			return fmt.Errorf("%s %q 必须是不含 .. 的包内相对路径", ref.label, ref.rel)
		}
		// 与 component entry 同理：Provider 契约决定内核 Catalog 的内容，
		// 不被 digest 覆盖等于允许一份未经校验的契约进内核
		if _, ok := m.Digests[ref.rel]; !ok {
			return fmt.Errorf("%s %q 未被 digests 覆盖", ref.label, ref.rel)
		}
	}
	if m.Provider.Descriptor == m.Provider.Schemas {
		return fmt.Errorf("provider.descriptor 与 provider.schemas 不能是同一个文件")
	}
	return nil
}

// sortedKeys 仅用于诊断输出的稳定排序。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
