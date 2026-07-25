package main

import (
	"errors"
	"strings"
	"testing"

	pkgv1 "github.com/nervus-os/nervus-ipc/go/protocol/interface/pkgmanagerv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/go/protocol/ipcv1"
)

func TestResolveHandoff_RejectsEscapes(t *testing.T) {
	// 交接目录是本服务沙箱里唯一可写的路径。调用方给的相对路径若能逃出去，
	// 就等于让 App 指定本进程读取任意文件——而本进程的 UID 能读到的东西
	// 未必是调用方能读到的。
	bad := []struct {
		name string
		path string
	}{
		{"空路径", ""},
		{"绝对路径", "/etc/shadow"},
		{"上跳一级", "../other-package/x.nspkg"},
		{"多级上跳", "../../../../etc/shadow"},
		{"中间上跳", "sub/../../escape.nspkg"},
		{"仅上跳", ".."},
		{"当前目录", "."},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveHandoff(tc.path)
			if err == nil {
				t.Fatalf("必须拒绝，却解析成了 %q", got)
			}
			if !errors.Is(err, ErrUnsafeRelPath) {
				t.Errorf("err = %v, want ErrUnsafeRelPath", err)
			}
		})
	}
}

func TestResolveHandoff_SiblingPrefixIsNotInside(t *testing.T) {
	// 前缀比较不带分隔符的话，一个叫 <dataRoot>-evil 的兄弟目录会混过检查。
	// 这条断言锁住那个分隔符。
	if _, err := resolveHandoff("../nervus.pkgmanagerd-evil/x.nspkg"); err == nil {
		t.Fatal("兄弟目录（同前缀）必须被拒绝")
	}
}

func TestResolveHandoff_AcceptsNormalPaths(t *testing.T) {
	ok := []string{"app.nspkg", "downloads/app.nspkg", "a/b/c.nspkg", "./app.nspkg"}
	for _, p := range ok {
		got, err := resolveHandoff(p)
		if err != nil {
			t.Errorf("%q 应被接受: %v", p, err)
			continue
		}
		if !strings.HasPrefix(got, dataRoot+"/") {
			t.Errorf("%q → %q，未落在交接目录内", p, got)
		}
	}
}

func TestClassify_MapsKernelMessages(t *testing.T) {
	// 这些是 nervud 侧真实错误文本里会出现的关键词。分类错了会让 UI 给出
	// 误导性的处置建议（比如把「包损坏」说成「版本太旧」）。
	tests := []struct {
		msg  string
		want pkgv1.PackageManagerReason
	}{
		{"pkgregistry: signature verification failed", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_SIGNATURE_INVALID},
		{"no developer signature", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_SIGNATURE_INVALID},
		{"pkgregistry: digest mismatch: {Missing:[bin/x]}", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_DIGEST_MISMATCH},
		{"upgrade rejected: version_code 1 <= 2", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_DOWNGRADE},
		{"manifest has empty or invalid supported_abis", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ABI_MISMATCH},
		{"pkgregistry: lineage broken", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_LINEAGE_BROKEN},
		{"system-image package cannot be uninstalled (immutable)", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_IMMUTABLE},
		{"解包失败: archive entry escapes destination", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ARCHIVE_INVALID},
		{"卸载被拒绝 (not-found): no such package", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_PACKAGE_NOT_FOUND},
	}
	for _, tc := range tests {
		if got := classify(tc.msg); got != tc.want {
			t.Errorf("classify(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestClassify_UnknownFailsClosed(t *testing.T) {
	// 认不出来就说认不出来。猜一个可能误导的原因比承认不知道更糟——
	// 用户会照着错误的建议白折腾。
	got := classify("something entirely unexpected happened")
	if got != pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_UNSPECIFIED {
		t.Fatalf("未知错误 = %v, want UNSPECIFIED", got)
	}
}

func TestCodeFor_MatchesReasonSemantics(t *testing.T) {
	// 外层 code 决定 SDK 的恢复行为（IsRetryable / NeedsReResolve），
	// typed detail 决定 UI 怎么解释。两者必须一致，否则 SDK 与 UI 会打架。
	tests := []struct {
		reason pkgv1.PackageManagerReason
		want   ipcv1.StatusCode
	}{
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_PACKAGE_NOT_FOUND, ipcv1.StatusCode_STATUS_CODE_NOT_FOUND},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_MANIFEST_INVALID, ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ARCHIVE_INVALID, ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_SIGNATURE_INVALID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_DOWNGRADE, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_IMMUTABLE, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_UNSPECIFIED, ipcv1.StatusCode_STATUS_CODE_INTERNAL},
	}
	for _, tc := range tests {
		if got := codeFor(tc.reason); got != tc.want {
			t.Errorf("codeFor(%v) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

func TestMethodIDs_MatchProtoEnum(t *testing.T) {
	// 本地变量若与 proto 枚举脱钩，调用会路由到错误的方法——运行期才发现，
	// 看起来像「功能不对」。这条断言让脱钩在编译/测试期暴露。
	if methodInstall != 1 || methodUninstal != 2 || methodList != 3 || methodSetComp != 4 {
		t.Fatalf("method id 与 proto 不符: install=%d uninstall=%d list=%d setcomp=%d",
			methodInstall, methodUninstal, methodList, methodSetComp)
	}
}
