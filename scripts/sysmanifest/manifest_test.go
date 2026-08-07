package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newPkgDir 造一棵最小的系统包目录树。
func newPkgDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "svc"), []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return dir
}

func minimalManifest() Manifest {
	return Manifest{
		Schema:          manifestSchemaV1,
		PackageID:       "nervus.example",
		Label:           "Example",
		Version:         "1.0.0",
		VersionCode:     1,
		MinNervusAPI:    1,
		TargetNervusAPI: 1,
		SupportedABIs:   []string{"linux-arm64"},
		Components: []Component{{
			ID:         "main",
			Type:       "service",
			Runtime:    "native",
			Entry:      "bin/svc",
			LaunchMode: "always-on",
		}},
	}
}

func TestComputeDigests_ExcludesSelfDescribingFiles(t *testing.T) {
	dir := newPkgDir(t)
	// manifest.json 与 manifest.sig 不能自散列——写进 digests 就改变了自己的哈希。
	// 内核的 VerifyDigests 同样豁免这两个文件。
	for _, name := range []string{manifestFileName, signatureFileName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	d, err := computeDigests(dir)
	if err != nil {
		t.Fatalf("computeDigests: %v", err)
	}
	if _, ok := d[manifestFileName]; ok {
		t.Error("manifest.json 不应出现在 digests 里")
	}
	if _, ok := d[signatureFileName]; ok {
		t.Error("manifest.sig 不应出现在 digests 里")
	}
	if _, ok := d["bin/svc"]; !ok {
		t.Error("载荷文件应被覆盖")
	}
}

func TestComputeDigests_MatchesSHA256(t *testing.T) {
	dir := newPkgDir(t)
	d, err := computeDigests(dir)
	if err != nil {
		t.Fatalf("computeDigests: %v", err)
	}
	want := sha256.Sum256([]byte("#!/bin/true\n"))
	if got := d["bin/svc"]; got != hex.EncodeToString(want[:]) {
		t.Fatalf("digest = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestComputeDigests_UsesForwardSlashes(t *testing.T) {
	// 内核用这些 key 直接拼包内路径。Windows 上构建时 filepath.Rel 会给出
	// 反斜杠，不归一化就会产出一份在 Linux 上永远对不上的 digest 清单。
	dir := newPkgDir(t)
	d, err := computeDigests(dir)
	if err != nil {
		t.Fatalf("computeDigests: %v", err)
	}
	for k := range d {
		if strings.Contains(k, "\\") {
			t.Fatalf("digest key %q 含反斜杠，必须归一化为 /", k)
		}
	}
}

func TestComputeDigests_RejectsSymlink(t *testing.T) {
	// digest 覆盖的必须是真实内容。一个符号链接的 digest 到底是「目标内容」
	// 还是「链接本身」取决于实现细节，这种歧义不该出现在完整性校验里。
	dir := newPkgDir(t)
	target := filepath.Join(dir, "bin", "svc")
	link := filepath.Join(dir, "bin", "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("本平台不支持符号链接: %v", err)
	}
	if _, err := computeDigests(dir); err == nil {
		t.Fatal("符号链接必须被拒绝")
	}
}

func TestEncodeManifest_IsStable(t *testing.T) {
	// 这份字节【就是被签名的对象】。两次编码必须完全一致，否则重新序列化后
	// 签名就验不过了。
	m := minimalManifest()
	m.Digests = map[string]string{"b": "2", "a": "1", "c": "3"}

	first, err := encodeManifest(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := encodeManifest(m)
		if err != nil {
			t.Fatalf("encode #%d: %v", i, err)
		}
		if string(again) != string(first) {
			t.Fatal("重复编码产出了不同字节，签名将无法复现")
		}
	}
	// map 必须按字典序输出（encoding/json 的保证），否则「稳定」只是碰巧
	if !strings.Contains(string(first), `"a": "1"`) ||
		strings.Index(string(first), `"a"`) > strings.Index(string(first), `"b"`) {
		t.Fatalf("digests 未按字典序输出:\n%s", first)
	}
}

func TestSignManifest_VerifiesWithKernelAlgorithm(t *testing.T) {
	// 完整复现内核 signature.go 的验签路径：
	//   msg = manifestSigDomain || manifestBytes
	// 域分隔前缀错一个字节，内核就验不过——而错误信息只会是「签名无效」，
	// 完全指不出是域分隔的问题。
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	manifestBytes := []byte(`{"schema":1,"package_id":"nervus.example"}`)

	sb := signManifest(manifestBytes, priv)
	if len(sb.Signatures) != 1 {
		t.Fatalf("签名条目数 = %d, want 1", len(sb.Signatures))
	}
	s := sb.Signatures[0]

	if s.Role != rolePlatformRelease {
		t.Errorf("role = %q, want %q", s.Role, rolePlatformRelease)
	}
	if s.Alg != "ed25519" {
		t.Errorf("alg = %q, want ed25519", s.Alg)
	}

	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil {
		t.Fatalf("sig 不是合法 base64: %v", err)
	}
	msg := append([]byte(manifestSigDomain), manifestBytes...)
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatal("按内核算法验签失败")
	}

	// 少了域分隔前缀必须验不过——否则这个前缀形同虚设
	if ed25519.Verify(pub, manifestBytes, sig) {
		t.Fatal("裸 manifest 字节竟然也能验过，域分隔没有生效")
	}
}

func TestSignManifest_EmbeddedKeyMatchesKeyID(t *testing.T) {
	// 内核会把内嵌公钥与信任库里 key_id 对应的公钥逐字节核对。
	// 这里断言我们自己产出的这两者是自洽的。
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sb := signManifest([]byte("{}"), priv)
	s := sb.Signatures[0]

	gotKey, err := base64.StdEncoding.DecodeString(s.Key)
	if err != nil {
		t.Fatalf("key 不是合法 base64: %v", err)
	}
	if string(gotKey) != string(pub) {
		t.Error("内嵌公钥与私钥对应的公钥不一致")
	}
	if s.KeyID != keyIDOf(pub) {
		t.Errorf("key_id = %q, want %q", s.KeyID, keyIDOf(pub))
	}
	if !strings.HasPrefix(s.KeyID, "sha256:") {
		t.Errorf("key_id 必须是 sha256: 前缀，got %q", s.KeyID)
	}
}

func TestSignatureBlock_JSONShapeMatchesKernel(t *testing.T) {
	// 内核用 DisallowUnknownFields 解析 manifest.sig。多一个字段就是装载失败，
	// 而错误信息只会说「unknown field」，不会告诉你哪一侧该改。
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	b, err := json.Marshal(signManifest([]byte("{}"), priv))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k := range generic {
		switch k {
		case "format", "signatures", "lineage":
		default:
			t.Errorf("顶层多出内核不认识的字段 %q", k)
		}
	}
}

func TestValidateManifest_AcceptsProviderArtifacts(t *testing.T) {
	m := minimalManifest()
	m.Components[0].Exports = []Export{{
		Interface: "nervus.interface.example", Visibility: "public",
	}}
	m.Provider = &ProviderArtifactsRef{
		Descriptor: "provider.binpb",
		Schemas:    "schemas.binpb",
	}
	m.Digests = map[string]string{
		"bin/svc":        "deadbeef",
		"provider.binpb": "cafe",
		"schemas.binpb":  "f00d",
	}
	if err := validateManifest(m); err != nil {
		t.Fatalf("合法的 provider 段被拒: %v", err)
	}
}

func TestManifest_ProviderJSONShapeMatchesKernel(t *testing.T) {
	// 内核用 DisallowUnknownFields 解析 manifest.json。provider 段的字段名与
	// omitempty 行为必须与 pkgregistry.ProviderArtifactsRef 完全一致。
	m := minimalManifest()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "provider") {
		t.Error("Provider 为 nil 时不应出现在 JSON 里（内核靠它区分「无 provider」）")
	}

	m.Provider = &ProviderArtifactsRef{Descriptor: "provider.binpb", Schemas: "schemas.binpb"}
	b, err = json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal with provider: %v", err)
	}
	var generic struct {
		Provider map[string]any `json:"provider"`
	}
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k := range generic.Provider {
		switch k {
		case "descriptor", "schemas":
		default:
			t.Errorf("provider 段多出内核不认识的字段 %q", k)
		}
	}
	if generic.Provider["descriptor"] != "provider.binpb" ||
		generic.Provider["schemas"] != "schemas.binpb" {
		t.Errorf("provider 段内容错误: %+v", generic.Provider)
	}
}

func TestValidateManifest_RejectsBadInput(t *testing.T) {
	digests := map[string]string{"bin/svc": "deadbeef"}

	tests := []struct {
		name  string
		mutex func(*Manifest)
		want  string
	}{
		{"version_code 为 0", func(m *Manifest) { m.VersionCode = 0 }, "version_code"},
		{"ABI 用 Android NDK 名", func(m *Manifest) { m.SupportedABIs = []string{"arm64-v8a"} }, "supported_abis"},
		{"ABI 用裸 CPU 名", func(m *Manifest) { m.SupportedABIs = []string{"aarch64"} }, "supported_abis"},
		{"service 不能 manual", func(m *Manifest) { m.Components[0].LaunchMode = "manual" }, "manual"},
		{"app 不能 always-on", func(m *Manifest) { m.Components[0].Type = "app" }, "always-on"},
		{"entry 绝对路径", func(m *Manifest) { m.Components[0].Entry = "/etc/shadow" }, "相对路径"},
		{"entry 路径逃逸", func(m *Manifest) { m.Components[0].Entry = "../../etc/shadow" }, "相对路径"},
		{"entry 未被 digest 覆盖", func(m *Manifest) { m.Components[0].Entry = "bin/other" }, "digests"},
		{"runtime 非法", func(m *Manifest) { m.Components[0].Runtime = "python" }, "runtime"},
		{"component id 重复", func(m *Manifest) {
			m.Components = append(m.Components, m.Components[0])
		}, "重复"},
		{"导出接口却没有 provider 段", func(m *Manifest) {
			m.Components[0].Exports = []Export{{
				Interface: "nervus.interface.example", Visibility: "public",
			}}
		}, "provider"},
		{"provider.descriptor 为空", func(m *Manifest) {
			m.Provider = &ProviderArtifactsRef{Descriptor: "", Schemas: "schemas.binpb"}
		}, "provider.descriptor"},
		{"provider.schemas 为空", func(m *Manifest) {
			// descriptor 先过 digest 覆盖检查，才能验到 schemas 的空值判定
			m.Digests = map[string]string{"bin/svc": "deadbeef", "provider.binpb": "cafe"}
			m.Provider = &ProviderArtifactsRef{Descriptor: "provider.binpb", Schemas: ""}
		}, "provider.schemas"},
		{"provider 路径逃逸", func(m *Manifest) {
			m.Provider = &ProviderArtifactsRef{
				Descriptor: "../../etc/shadow", Schemas: "schemas.binpb",
			}
		}, "相对路径"},
		{"provider 未被 digest 覆盖", func(m *Manifest) {
			m.Provider = &ProviderArtifactsRef{
				Descriptor: "provider.binpb", Schemas: "schemas.binpb",
			}
		}, "digests"},
		{"descriptor 与 schemas 同一个文件", func(m *Manifest) {
			// 单独给一份 digests：这条要越过覆盖检查才能验到同名判定
			m.Digests = map[string]string{"bin/svc": "deadbeef", "provider.binpb": "cafe"}
			m.Provider = &ProviderArtifactsRef{
				Descriptor: "provider.binpb", Schemas: "provider.binpb",
			}
		}, "同一个文件"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := minimalManifest()
			m.Digests = digests
			tc.mutex(&m)
			err := validateManifest(m)
			if err == nil {
				t.Fatalf("应当被拒绝")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未提及 %q", err, tc.want)
			}
		})
	}
}

func TestValidateManifest_AcceptsMinimal(t *testing.T) {
	m := minimalManifest()
	m.Digests = map[string]string{"bin/svc": "deadbeef"}
	if err := validateManifest(m); err != nil {
		t.Fatalf("最小合法 manifest 被拒: %v", err)
	}
}
