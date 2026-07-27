// Package pkgmanager 是 nervus.pkgmanagerd 的服务实现：对 App 提供软件安装
// 接口，内部经管理通道转发给 nervud。
//
// # 它是一座桥，不是一个决策者
//
//	App (Kotlin) ──IPC──▶ pkgmanagerd ──adminwire──▶ nervud
//	                          │
//	                     解 .nspkg（防 tar-slip）
//
// 全部安全判定在 nervud 的 pkgregistry：多角色验签、digest 复核、OEM 副署准入、
// Host ABI 匹配、升级裁决（防降级 + 签名血统连续性）、权限交集、UID 分配、
// 原子提交。本包一条都不做，也【不允许】做——在这里写出任何形如
// 「如果……就允许」的代码都是设计错误。
//
// 它存在的理由只有一个：App 不可能是 root，连不上 root-only 的管理通道，
// 需要一个跑在 App UID 段、但被内核显式放行的服务替它转发。
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
)

// Method ID 直接取自 proto 生成的枚举，【不在本地重抄一份常量】。
//
// 抄一份的代价不是重复，是它会悄悄过期：proto 改了 method_id，本地常量不会
// 报错，只会让调用路由到错误的方法——而那是运行期才发现的、看起来像
// 「功能不对」的故障。用生成枚举的话，proto 改名/改号在编译期就撞出来。
var (
	methodInstall  = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_INSTALL)
	methodUninstal = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_UNINSTALL)
	methodList     = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_LIST)
	methodSetComp  = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_SET_COMPONENT_ENABLED)
)

// Service 是 pkgmanagerd 的业务实现。
type Service struct {
	admin *Client
	log   *slog.Logger
}

// newService 构造 Service。
func newService(admin *Client, log *slog.Logger) *Service {
	return &Service{admin: admin, log: log}
}

// dataRoot 是本服务的私有数据目录，也是与 App 交接 .nspkg 的唯一位置。
//
// 由内核在安装本包时创建，属主为本包 UID、权限 0700，并且是沙箱里【唯一可写】
// 的路径（systemd 的 ReadWritePaths 只列了它）。WorkingDirectory 也是它。
const dataRoot = "/var/lib/nervus/package-data/nervus.pkgmanagerd"

// ErrUnsafeRelPath 调用方给的路径试图逃出交接目录。
var ErrUnsafeRelPath = errors.New("nspkg_relpath escapes the handoff directory")

// InstallFromRelPath 执行一次完整的安装：向 nervud 要 staging 目录、把 .nspkg
// 解包进去、再让 nervud 复核并提交。
//
// relPath 是【交接目录内的相对路径】，不是绝对路径。协议这么定不是洁癖：
// 本服务跑在 ProtectSystem=strict 的沙箱里，能读到的只有自己的数据目录，
// 一个它读不到的绝对路径只会得到含义模糊的「打不开」。
//
// 路径校验在这里做一次（拒绝绝对路径与 ".." 逃逸），nervud 侧还会再做一次。
// 两道是有意的：本服务的进程内可能有别的 bug 把 relPath 改坏，而 nervud 那道
// 是跨信任边界的最终防线。
func (s *Service) InstallFromRelPath(relPath string) (*PackageInfo, error) {
	nspkgPath, err := resolveHandoff(relPath)
	if err != nil {
		return nil, err
	}
	return s.installFile(nspkgPath)
}

// resolveHandoff 把交接目录内的相对路径解析成绝对路径，拒绝一切逃逸。
func resolveHandoff(relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("%w: empty", ErrUnsafeRelPath)
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("%w: %q is absolute", ErrUnsafeRelPath, relPath)
	}
	full := filepath.Join(dataRoot, relPath)
	cleaned := filepath.Clean(full)
	// 前缀比较【带分隔符】，否则 /var/lib/nervus/package-data/nervus.pkgmanagerd-evil
	// 会混过朴素前缀检查。
	if !strings.HasPrefix(cleaned, dataRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", ErrUnsafeRelPath, relPath)
	}
	return cleaned, nil
}

func (s *Service) installFile(nspkgPath string) (*PackageInfo, error) {
	// 1. 让 nervud 建 staging 目录。
	//
	// 【必须由 nervud 建，不能自己挑一个】。它保证三件事：位置与 PackageRoot
	// 同一文件系统（安装期 renameat2 才不会 EXDEV 失败）、属主与权限受控、
	// 以及 install 时路径逃逸校验有一个明确的「必须是我发出的目录」判据。
	begin, err := s.admin.Do(Request{Cmd: CmdBeginStaging})
	if err != nil {
		return nil, fmt.Errorf("begin-staging: %w", err)
	}
	if !begin.OK {
		return nil, fmt.Errorf("begin-staging 被拒绝 (%s): %s", begin.Code, begin.Message)
	}
	staging := begin.StagingDir
	s.log.Info("pkgmanager: staging allocated", "dir", staging)

	// 2. 解包。这是本服务唯一有安全责任的一步（防 tar-slip）。
	//
	// 失败时【不清理】staging：nervud 自己会在下一次 begin-staging 时清扫超过
	// 一小时的孤儿目录。我们没有删它的权限，也不该有——那个目录是 nervud 的
	// 掌控范围，给转发层删除权等于多开一个可被滥用的口子。
	if err := Unpack(nspkgPath, staging); err != nil {
		s.log.Warn("pkgmanager: unpack failed", "nspkg", nspkgPath, "err", err)
		return nil, fmt.Errorf("解包失败: %w", err)
	}

	// 3. 让 nervud 复核并提交。签名、digest、裁决全在那一侧。
	res, err := s.admin.Do(Request{
		Cmd:        CmdInstall,
		StagingDir: staging,
	})
	if err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	if !res.OK {
		// 原样带上 nervud 给的 code 与 message：安装被拒的原因（签名不对、
		// 版本降级、ABI 不匹配、权限不足）对用户是有意义的，不要在这里
		// 归一化成一句「安装失败」。
		return nil, fmt.Errorf("安装被拒绝 (%s): %s", res.Code, res.Message)
	}
	return res.Package, nil
}

// Uninstall 卸载一个动态安装的 Package。
//
// 系统镜像包卸载不了——nervud 会以 ErrSystemPackageImmutable 拒绝（它们跟随
// 整镜像 OTA）。本服务不预判，直接转发，让拒绝原因由 nervud 给出。
func (s *Service) Uninstall(packageID string) error {
	res, err := s.admin.Do(Request{
		Cmd:       CmdUninstall,
		PackageID: packageID,
	})
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("卸载被拒绝 (%s): %s", res.Code, res.Message)
	}
	return nil
}

// List 列出已装 Package。
func (s *Service) List() ([]PackageInfo, error) {
	res, err := s.admin.Do(Request{Cmd: CmdList})
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	if !res.OK {
		return nil, fmt.Errorf("列表被拒绝 (%s): %s", res.Code, res.Message)
	}
	return res.Packages, nil
}

// SetComponentEnabled 停用/启用一个 Component。
//
// 保护名单里的组件（pkgmanagerd 自己、settings、permissionui、sessiond、
// safety.recovery）停不掉，nervud 会拒绝。这条底线在内核的编译期 switch 里，
// 不由本服务判断——判断权放在这里就等于让转发层能决定系统的自我修复能力。
func (s *Service) SetComponentEnabled(packageID, componentID string, enabled bool) error {
	res, err := s.admin.Do(Request{
		Cmd:         CmdSetEnabled,
		PackageID:   packageID,
		ComponentID: componentID,
		Enabled:     enabled,
	})
	if err != nil {
		return fmt.Errorf("set-enabled: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("停用/启用被拒绝 (%s): %s", res.Code, res.Message)
	}
	return nil
}

// SetPermission 设置一个运行期（GrantUser）权限的授予状态。
//
// 【注意 v1 现状】：内核的 permission.V1GrantAll 打开时，运行期用户确认被短路，
// 所有已声明权限直接放行。本方法仍然会把命令投递过去（nervud 侧的
// SetRuntimeState 依旧会落盘状态），但它对 Allowed 的结果暂时没有影响。
// 执法恢复后自动生效，不需要改本服务。
func (s *Service) SetPermission(packageID, permission, state string) error {
	res, err := s.admin.Do(Request{
		Cmd:        CmdSetPermission,
		PackageID:  packageID,
		Permission: permission,
		GrantState: state,
	})
	if err != nil {
		return fmt.Errorf("set-permission: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("权限设置被拒绝 (%s): %s", res.Code, res.Message)
	}
	return nil
}
