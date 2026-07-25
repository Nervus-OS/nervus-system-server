package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const usage = `sysmanifest — 给系统包目录树补齐 manifest.json 与 manifest.sig

用法:
  sysmanifest -dir <包目录> -template <模板.json> -key <私钥.pem> [-version-code N]

参数:
  -dir           系统包目录（内含已构建的载荷，如 bin/pkgmanagerd）
  -template      manifest 模板，digests 字段由本工具填充
  -key           Ed25519 私钥（PKCS#8 PEM）。用 openssl genpkey -algorithm ed25519 生成
  -version-code  覆盖模板里的 version_code（CI 递增用）
  -print         只打印将写出的 manifest，不落盘

产出:
  <dir>/manifest.json   被签名的那份原始字节
  <dir>/manifest.sig    platform-release 角色的 Ed25519 签名块

注意: 系统包是【解包后的目录树】，不是 .nspkg。后者是动态安装用的格式。
`

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func main() {
	dir := flag.String("dir", "", "系统包目录")
	tmpl := flag.String("template", "", "manifest 模板 JSON")
	keyPath := flag.String("key", "", "Ed25519 私钥 PEM")
	versionCode := flag.Uint64("version-code", 0, "覆盖 version_code")
	printOnly := flag.Bool("print", false, "只打印不落盘")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if err := run(*dir, *tmpl, *keyPath, *versionCode, *printOnly); err != nil {
		fmt.Fprintf(os.Stderr, "sysmanifest: %v\n", err)
		os.Exit(1)
	}
}

func run(dir, tmplPath, keyPath string, versionCode uint64, printOnly bool) error {
	if dir == "" || tmplPath == "" {
		return fmt.Errorf("-dir 与 -template 必填（-h 看用法）")
	}
	if !printOnly && keyPath == "" {
		return fmt.Errorf("-key 必填（或用 -print 只预览）")
	}

	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		return fmt.Errorf("读模板: %w", err)
	}
	var m Manifest
	// 模板也用 DisallowUnknownFields：模板里写错一个字段名，本工具会立刻报错，
	// 而不是把它默默丢掉、等内核装载时才发现某个配置没生效。
	dec := json.NewDecoder(newReader(tmplBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("解析模板: %w", err)
	}

	if versionCode != 0 {
		m.VersionCode = versionCode
	}
	if m.Schema == 0 {
		m.Schema = manifestSchemaV1
	}

	// digests 一律由本工具计算并【覆盖】模板里的任何值：手写的 digest 一定会
	// 过期，而过期的 digest 在装载时表现为「镜像损坏」——一个足以让人查半天
	// 硬件的假信号。
	digests, err := computeDigests(dir)
	if err != nil {
		return fmt.Errorf("计算 digest: %w", err)
	}
	m.Digests = digests

	if err := validateManifest(m); err != nil {
		return fmt.Errorf("manifest 校验失败: %w", err)
	}

	manifestBytes, err := encodeManifest(m)
	if err != nil {
		return err
	}

	if printOnly {
		fmt.Printf("%s", manifestBytes)
		fmt.Fprintf(os.Stderr, "\n--- %d 个载荷文件 ---\n", len(digests))
		for _, k := range sortedKeys(digests) {
			fmt.Fprintf(os.Stderr, "  %s  %s\n", digests[k][:16], k)
		}
		return nil
	}

	priv, err := loadPrivateKey(keyPath)
	if err != nil {
		return err
	}
	sigBlock := signManifest(manifestBytes, priv)
	sigBytes, err := json.MarshalIndent(sigBlock, "", "  ")
	if err != nil {
		return fmt.Errorf("编码签名块: %w", err)
	}
	sigBytes = append(sigBytes, '\n')

	// 先写 manifest.json 再写 manifest.sig。反过来会留下一个「签名已在盘上、
	// manifest 还是旧的」的中间态，那种组合验签必然失败，且失败信息指向
	// 「签名不匹配」而不是「构建被打断」，误导性很强。
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), manifestBytes, 0o644); err != nil {
		return fmt.Errorf("写 manifest.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, signatureFileName), sigBytes, 0o644); err != nil {
		return fmt.Errorf("写 manifest.sig: %w", err)
	}

	fmt.Printf("✅ %s\n", m.PackageID)
	fmt.Printf("   version      %s (code %d)\n", m.Version, m.VersionCode)
	fmt.Printf("   components   %d\n", len(m.Components))
	fmt.Printf("   digests      %d 个文件\n", len(digests))
	fmt.Printf("   key_id       %s\n", sigBlock.Signatures[0].KeyID)
	return nil
}

// loadPrivateKey 读 PKCS#8 PEM 里的 Ed25519 私钥。
//
// 只接受 Ed25519：内核的 SigAlg 也只允许 ed25519，未知算法一律拒绝、不做
// 「尽量兼容」。在这里就拒掉 RSA/ECDSA，好过产出一个内核装载时才拒绝的包。
func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读私钥: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s 不是有效的 PEM", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析 PKCS#8 私钥: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("私钥类型是 %T，只接受 Ed25519", key)
	}
	return priv, nil
}
