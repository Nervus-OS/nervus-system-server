package main

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// zeroReader 是一个无限零字节源，用来把 header 声明的大小填满。
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// entry 是构造测试用 .nspkg 的一条 tar 条目。
type entry struct {
	name     string
	body     string
	mode     int64
	typeflag byte
	linkname string
	// sizeLie 非零时把 header 里的 Size 写成它，而不是 len(body)。
	// 用于验证 CopyN 的兜底：header 可以撒谎。
	sizeLie int64
}

// buildNspkg 把 entries 打成一个 zstd+tar 的 .nspkg，返回路径。
func buildNspkg(t *testing.T, entries []entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pkg.nspkg")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()

	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(e.body))
		if tf != tar.TypeReg {
			size = 0
		}
		if e.sizeLie != 0 {
			size = e.sizeLie
		}
		hdr := &tar.Header{Name: e.name, Typeflag: tf, Mode: mode, Size: size, Linkname: e.linkname}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		switch {
		case tf != tar.TypeReg:
			// 非常规类型没有 body。
		case e.sizeLie != 0:
			// tar.Writer 要求写满 header 声明的字节数，否则 Close 报
			// "missed writing N bytes"。填零即可——zstd 把零压得几乎不占空间，
			// 而 Unpack 在读 body 之前就按 hdr.Size 拒绝了，根本读不到这里。
			if _, err := io.CopyN(tw, zeroReader{}, size); err != nil {
				t.Fatalf("pad body %q: %v", e.name, err)
			}
		default:
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zstd: %v", err)
	}
	return p
}

// unpackInto 把 entries 解进一个新建的 staging 目录，返回该目录与错误。
func unpackInto(t *testing.T, entries []entry) (string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	return dest, Unpack(buildNspkg(t, entries), dest)
}

// 正常包必须能解开，包括嵌套目录。
//
// 先立这条：后面全是拒绝用例，没有一条成功用例的话，一个「什么都拒绝」的
// 实现也能让整个文件变绿。
func TestUnpack_NormalPackage(t *testing.T) {
	dest, err := unpackInto(t, []entry{
		{name: "manifest.json", body: `{"schema":1}`},
		{name: "manifest.sig", body: `{"format":1}`},
		{name: "bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "bin/app", body: "ELF", mode: 0o755},
		{name: "lib/native/libfoo.so", body: "SO", mode: 0o644},
	})
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	for _, rel := range []string{"manifest.json", "manifest.sig", "bin/app", "lib/native/libfoo.so"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	// 可执行位要保留——丢了的话装出来的包跑不起来，而症状是 203/EXEC，
	// 排查时很难想到是解包丢了权限位。
	fi, err := os.Stat(filepath.Join(dest, "bin/app"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("bin/app mode = %v, want executable", fi.Mode().Perm())
	}
}

// ".." 逃逸：最经典的 tar-slip。
func TestUnpack_RejectsDotDotEscape(t *testing.T) {
	for _, name := range []string{
		"../evil",
		"../../etc/cron.d/evil",
		"bin/../../../evil",
		"./../evil",
	} {
		t.Run(name, func(t *testing.T) {
			dest, err := unpackInto(t, []entry{{name: name, body: "pwned"}})
			if err == nil {
				t.Fatalf("Unpack accepted escaping entry %q", name)
			}
			// 不仅要报错，还必须【没写出去】。报错但已落盘等于没防住。
			outside := filepath.Join(filepath.Dir(dest), "evil")
			if _, statErr := os.Stat(outside); statErr == nil {
				t.Errorf("entry %q was written outside dest at %s", name, outside)
			}
		})
	}
}

// 绝对路径条目。
func TestUnpack_RejectsAbsolutePath(t *testing.T) {
	_, err := unpackInto(t, []entry{{name: "/etc/passwd", body: "pwned"}})
	if err == nil {
		t.Fatal("Unpack accepted an absolute entry name")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("err = %v, want it to mention 'absolute'", err)
	}
}

// 空条目名。
func TestUnpack_RejectsEmptyName(t *testing.T) {
	_, err := unpackInto(t, []entry{{name: "", body: "x"}})
	if err == nil {
		t.Fatal("Unpack accepted an empty entry name")
	}
}

// 符号链接与硬链接：symlink 指向目标目录外就是一条逃逸通道，
// 而受签名的 App 包里两者都没有合法用途。
func TestUnpack_RejectsLinks(t *testing.T) {
	cases := []struct {
		label string
		e     entry
	}{
		{"symlink", entry{name: "evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
		{"hardlink", entry{name: "evil", typeflag: tar.TypeLink, linkname: "manifest.json"}},
		{"symlink-relative", entry{name: "bin/x", typeflag: tar.TypeSymlink, linkname: "../../../etc"}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			if _, err := unpackInto(t, []entry{tc.e}); err == nil {
				t.Fatalf("Unpack accepted a %s entry", tc.label)
			}
		})
	}
}

// 设备节点与 FIFO。
func TestUnpack_RejectsSpecialFiles(t *testing.T) {
	cases := []struct {
		label string
		tf    byte
	}{
		{"chardev", tar.TypeChar},
		{"blockdev", tar.TypeBlock},
		{"fifo", tar.TypeFifo},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			if _, err := unpackInto(t, []entry{{name: "evil", typeflag: tc.tf}}); err == nil {
				t.Fatalf("Unpack accepted a %s entry", tc.label)
			}
		})
	}
}

// setuid / setgid / sticky 位必须被剥掉。
//
// tar 的 Mode 是完整的 st_mode 低位，能带 04000。一个 setuid root 的文件
// 落到磁盘上就是提权——即便 nervud 随后按 digest 复核内容通过，权限位
// 不在 digest 覆盖范围内。
func TestUnpack_StripsSetuidBits(t *testing.T) {
	dest, err := unpackInto(t, []entry{
		{name: "bin/suid", body: "x", mode: 0o4755},
		{name: "bin/sgid", body: "x", mode: 0o2755},
		{name: "bin/sticky", body: "x", mode: 0o1755},
	})
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	for _, rel := range []string{"bin/suid", "bin/sgid", "bin/sticky"} {
		fi, err := os.Stat(filepath.Join(dest, rel))
		if err != nil {
			t.Fatal(err)
		}
		if m := fi.Mode(); m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s mode = %v, want setuid/setgid/sticky stripped", rel, m)
		}
	}
}

// 同名条目重复出现必须失败，不能后写覆盖先写。
//
// tar 允许重复条目名。若覆盖，攻击者可以让第一条通过 nervud 的 digest 复核
// 视野内的内容，再用第二条把它换掉——而 O_EXCL 让这条路直接不通。
func TestUnpack_RejectsDuplicateEntries(t *testing.T) {
	_, err := unpackInto(t, []entry{
		{name: "bin/app", body: "good"},
		{name: "bin/app", body: "evil"},
	})
	if err == nil {
		t.Fatal("Unpack accepted a duplicate entry name; later content would silently win")
	}
}

// 超大条目：header 自报的 Size 超上限就该拒。
func TestUnpack_RejectsOversizedEntry(t *testing.T) {
	_, err := unpackInto(t, []entry{{name: "big", sizeLie: maxEntryBytes + 1}})
	if err == nil {
		t.Fatal("Unpack accepted an entry over the size cap")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v, want it to mention 'too large'", err)
	}
}

// 条目数上限：海量小条目式炸弹。
func TestUnpack_RejectsTooManyEntries(t *testing.T) {
	entries := make([]entry, 0, maxTotalEntries+2)
	for i := 0; i <= maxTotalEntries+1; i++ {
		entries = append(entries, entry{name: "f" + itoa(i), body: "x"})
	}
	_, err := unpackInto(t, entries)
	if err == nil {
		t.Fatal("Unpack accepted an archive over the entry-count cap")
	}
	if !strings.Contains(err.Error(), "too many entries") {
		t.Errorf("err = %v, want it to mention 'too many entries'", err)
	}
}

// safeJoin 的兄弟目录前缀：/a/staging-evil 不得混过 /a/staging 的检查。
//
// 单独测 safeJoin 而不是走 Unpack，是因为要构造出这个场景需要一个真实存在的
// 同前缀兄弟目录，而这条性质本身只关乎字符串比较。
func TestSafeJoin_SiblingPrefixIsNotInside(t *testing.T) {
	base := filepath.Clean("/var/lib/nervus/staging")
	if _, err := safeJoin(base, "../staging-evil/x"); err == nil {
		t.Fatal("safeJoin accepted a sibling directory sharing the base prefix")
	}
}

// safeJoin 接受正常路径。
func TestSafeJoin_AcceptsNormalNames(t *testing.T) {
	base := filepath.Clean("/var/lib/nervus/staging")
	for _, name := range []string{"manifest.json", "bin/app", "lib/native/libfoo.so", "./bin/app"} {
		got, err := safeJoin(base, name)
		if err != nil {
			t.Errorf("safeJoin(%q) = %v, want ok", name, err)
			continue
		}
		if !strings.HasPrefix(got, base+string(os.PathSeparator)) {
			t.Errorf("safeJoin(%q) = %q, want inside %q", name, got, base)
		}
	}
}

// itoa 避免为一行数字转换引入 strconv 之外的东西。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
