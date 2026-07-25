// 本文件把 .nspkg（zstd 压缩的 tar）解包进 nervud 指定的 staging 目录。
//
// # 这是 pkgmanagerd 唯一的「重活」
//
// 其余全是转发。而这一步恰恰是本服务里唯一有安全责任的地方：解包发生在
// 提交给 nervud 复核【之前】，一个恶意 .nspkg 若带 "../.." 或绝对路径条目，
// 解包时就能写到目标目录之外。
//
// 分工要说清楚：nervud 会对 staging 里每个文件按 manifest.Digests 重新做内容
// 复核（VerifyDigests），所以【内容层面】的信任锚在 nervud，不在这里。本文件
// 只保证解包这一步本身不越界——这一条 nervud 替不了，因为越界发生在它看到
// 这棵树之前。
//
// 逻辑与内核 cmd/nervusctl/unpack.go 一致（那份已经在生产路径上验证过）。
// 刻意照抄而不是「改进」：这类防护的每一条都对应一种具体攻击，重写等于
// 重新踩一遍坑。
package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// maxEntryBytes 是单个解包条目的字节上限，挡住解压炸弹式的超大条目。
// 64 MiB 对正常 App 载荷绰绰有余；真正的包大小约束由 nervud/manifest 侧负责。
const maxEntryBytes = 64 << 20

// maxTotalEntries 是一个 .nspkg 允许的条目数上限，挡住海量小条目式炸弹。
const maxTotalEntries = 10000

// Unpack 把 nspkgPath 解包进 destDir（须已存在）。
//
// destDir 必须是 nervud 经 begin-staging 建好并发回的路径——属主与权限受控，
// 且与 PackageRoot 同一文件系统（安装期的 renameat2 才不会跨盘失败）。
// 绝不要自己挑一个目录解包再交给 nervud。
func Unpack(nspkgPath, destDir string) error {
	f, err := os.Open(nspkgPath)
	if err != nil {
		return fmt.Errorf("open nspkg: %w", err)
	}
	defer func() { _ = f.Close() }()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return fmt.Errorf("zstd reader: %w", err)
	}
	defer zr.Close()

	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	absDest = filepath.Clean(absDest)

	tr := tar.NewReader(zr)
	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		count++
		if count > maxTotalEntries {
			return fmt.Errorf("archive has too many entries (> %d)", maxTotalEntries)
		}

		target, err := safeJoin(absDest, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeRegular(tr, target, hdr); err != nil {
				return err
			}
		default:
			// 拒绝符号链接/硬链接/设备/FIFO 等一切非常规类型：它们要么可用于
			// 逃逸（symlink 指向目标目录外），要么在受签名 App 包里没有合法用途。
			return fmt.Errorf("archive entry %q has unsupported type %d", hdr.Name, hdr.Typeflag)
		}
	}
	return nil
}

// safeJoin 把条目名安全地拼到 base 下，拒绝一切逃逸：空名、绝对路径、
// 清理后仍逃出 base 的（含 ".." 折叠）。
func safeJoin(base, name string) (string, error) {
	if name == "" {
		return "", errors.New("archive entry has empty name")
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry %q is absolute", name)
	}
	target := filepath.Join(base, name)
	cleaned := filepath.Clean(target)
	// 必须严格位于 base 之内（base 本身不算）。前缀比较【带分隔符】，
	// 否则 /a/staging-evil 会通过 /a/staging 的朴素前缀检查。
	if cleaned != base && !strings.HasPrefix(cleaned, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes destination", name)
	}
	return cleaned, nil
}

// writeRegular 把当前 tar 条目写成普通文件。
func writeRegular(tr *tar.Reader, target string, hdr *tar.Header) error {
	if hdr.Size < 0 || hdr.Size > maxEntryBytes {
		return fmt.Errorf("archive entry %q too large (%d bytes)", hdr.Name, hdr.Size)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	// 只取权限低 9 位，显式屏蔽 setuid/setgid/sticky：带这些位的普通文件在 App
	// 包里没有合法用途，而 setuid 落到磁盘是提权隐患。
	//
	// O_EXCL：同名条目重复出现时直接失败，不覆盖。tar 允许重复条目名，
	// 「后写覆盖先写」会让 digest 校验过的内容被悄悄替换掉。
	mode := os.FileMode(hdr.Mode).Perm()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	// CopyN 上限再兜一次底：即便 hdr.Size 谎报，也不会写超过上限。
	if _, err := io.CopyN(out, tr, hdr.Size); err != nil && !errors.Is(err, io.EOF) {
		_ = out.Close()
		return fmt.Errorf("write %q: %w", target, err)
	}
	return out.Close()
}
