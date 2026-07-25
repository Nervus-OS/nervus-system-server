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
	"fmt"
	"log/slog"
)

// Method ID。与 nervus.interface.pkg.manager 接口的 method_id 枚举对应。
//
// 【暂定值】。这个接口的 .proto 还没进 nervus-ipc——它需要先按 A 组的
// method_registry 机制挂上 method_meta（权限、风险级、是否需确认），
// 才谈得上冻结。在那之前这里是本地常量，proto 落地后必须回来对齐，
// 且以 proto 为准。
const (
	MethodInstall       uint32 = 1
	MethodUninstall     uint32 = 2
	MethodList          uint32 = 3
	MethodSetEnabled    uint32 = 4
	MethodSetPermission uint32 = 5
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

// InstallFromFile 执行一次完整的安装：向 nervud 要 staging 目录、把 .nspkg
// 解包进去、再让 nervud 复核并提交。
//
// nspkgPath 由调用方（App）给出。本服务【不校验它的内容】——那是 nervud 的活。
// 但要注意：调用方能指定任意路径，而本服务跑在自己的沙箱里，能读到的只有
// 自己的私有数据目录与只读的系统区。App 想装的包必须先落到双方都能访问的
// 位置，这个交接约定属于接口层，不在本函数。
func (s *Service) InstallFromFile(nspkgPath string) (*PackageInfo, error) {
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
