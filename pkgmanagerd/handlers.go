// 本文件是 IPC 方法到业务实现的适配层：解 payload、调 Service、编结果、
// 把失败翻成带 typed detail 的 StatusError。
//
// 刻意与 service.go 分开：那边是「怎么跟 nervud 打交道」，这边是「怎么跟 App
// 打交道」。两侧的错误语义完全不同——nervud 给的是 adminwire 的 code 字符串，
// App 要的是 StatusCode + PackageManagerErrorDetail。
package main

import (
	"log/slog"
	"strings"

	"google.golang.org/protobuf/proto"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
)

// registerHandlers 把四个方法接到业务实现上。
//
// 【必须在 RegisterEndpoint 之前调用完】。报到成功那一刻 nervud 就可能转发
// Dispatch，此时还没注册的 method 会被回 NOT_FOUND——调用方会以为这个方法
// 不存在，而不是「服务还没准备好」，这种错误很难往回追。
func registerHandlers(host *sdk.ServiceHost, svc *Service, log *slog.Logger) {
	host.Handle(methodInstall, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req pkgv1.InstallRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, invalidArgument()
		}
		// consented_permissions 原样转发。本服务不复核它：谁有资格填这个字段
		// 由 nervud 的 needs_user_confirmation 门决定（只对持
		// perm.permission.admin 的调用方放行，即系统的确认界面），而哪些条目
		// 真正落库由 Catalog 决定。在这里加一层判断只会产生第二个真相源。
		info, err := svc.InstallFromRelPath(
			req.GetNspkgRelpath(), req.GetConsentedPermissions())
		if err != nil {
			return nil, mapErr(err, log, "install")
		}
		return proto.Marshal(&pkgv1.InstallResult{Package: toProto(info)})
	})

	host.Handle(methodUninstal, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req pkgv1.UninstallRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, invalidArgument()
		}
		if err := svc.Uninstall(req.GetPackageId()); err != nil {
			return nil, mapErr(err, log, "uninstall")
		}
		// response_type 为空 = 空 success，不回 payload
		return nil, nil
	})

	host.Handle(methodList, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		// ListRequest 目前是空消息，仍然解一次：payload 里有垃圾说明调用方
		// 用错了类型，早点报 INVALID_ARGUMENT 好过静默忽略。
		var req pkgv1.ListRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, invalidArgument()
		}
		infos, err := svc.List()
		if err != nil {
			return nil, mapErr(err, log, "list")
		}
		out := &pkgv1.ListResult{Packages: make([]*pkgv1.PackageInfo, 0, len(infos))}
		for i := range infos {
			out.Packages = append(out.Packages, toProto(&infos[i]))
		}
		return proto.Marshal(out)
	})

	host.Handle(methodSetComp, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req pkgv1.SetComponentEnabledRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, invalidArgument()
		}
		err := svc.SetComponentEnabled(req.GetPackageId(), req.GetComponentId(), req.GetEnabled())
		if err != nil {
			return nil, mapErr(err, log, "set-component-enabled")
		}
		return nil, nil
	})
}

// toProto 把 adminwire 的 PackageInfo 投影成接口层的 PackageInfo。
//
// 两个类型刻意不共用：adminwire 那份是内核的内部投影（将来可能带更多诊断
// 字段），接口层这份是给 App 看的对外契约。共用会让内核加一个内部字段就
// 意外泄漏到 App 侧。
func toProto(p *PackageInfo) *pkgv1.PackageInfo {
	if p == nil {
		return nil
	}
	return &pkgv1.PackageInfo{
		PackageId:          p.ID,
		Version:            p.Version,
		VersionCode:        p.VersionCode,
		Trust:              p.Trust,
		Source:             p.Source,
		GrantedPermissions: p.Granted,
		DisabledComponents: p.Disabled,
	}
}

func invalidArgument() error {
	return &sdk.StatusError{Code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT}
}

// mapErr 把业务错误翻成带 typed detail 的 StatusError。
//
// # 为什么靠字符串匹配，以及为什么这是暂时的
//
// nervud 的管理通道（adminwire）只回一个粗粒度的 code 字符串（failed /
// not-found / bad-request）加一句人类可读的 message。要给 App 可区分的原因
// （签名无效 vs 防降级 vs ABI 不匹配——三者的正确反应完全不同），此刻只能
// 从 message 里认。
//
// 这是**明确的技术债**，位置在 nervud 的 adminwire：它应该回结构化的原因码，
// 而不是让下游从人类可读文本里反推。在那之前，认不出来的一律落到
// UNSPECIFIED——fail closed，UI 提示「联系发布者」而不是猜一个可能误导的原因。
//
// 原始错误只进本地日志：把内部错误文本塞给 App 等于把 nervud 的实现细节
// 泄漏出去。
func mapErr(err error, log *slog.Logger, op string) error {
	log.Warn("pkgmanager: operation failed", "op", op, "err", err)

	reason := classify(err.Error())
	detail, merr := proto.Marshal(&pkgv1.PackageManagerErrorDetail{Reason: reason})
	if merr != nil {
		// 连 detail 都编不出来是本端 bug，不该让它掩盖原始失败
		log.Error("pkgmanager: marshal error detail failed", "err", merr)
		return &sdk.StatusError{Code: ipcv1.StatusCode_STATUS_CODE_INTERNAL}
	}
	return &sdk.StatusError{
		Code:   codeFor(reason),
		Detail: detail,
	}
}

// classify 从 nervud 的错误文本里认原因。见 mapErr 关于技术债的说明。
func classify(msg string) pkgv1.PackageManagerReason {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "signature"), strings.Contains(m, "signer"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_SIGNATURE_INVALID
	case strings.Contains(m, "digest"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_DIGEST_MISMATCH
	case strings.Contains(m, "downgrade"), strings.Contains(m, "version_code"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_DOWNGRADE
	case strings.Contains(m, "abi"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ABI_MISMATCH
	case strings.Contains(m, "manifest"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_MANIFEST_INVALID
	case strings.Contains(m, "lineage"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_LINEAGE_BROKEN
	case strings.Contains(m, "immutable"), strings.Contains(m, "protected"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_IMMUTABLE
	case strings.Contains(m, "解包失败"), strings.Contains(m, "archive"), strings.Contains(m, "escapes"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ARCHIVE_INVALID
	case strings.Contains(m, "not-found"), strings.Contains(m, "not found"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_PACKAGE_NOT_FOUND
	default:
		// fail closed：认不出来就说认不出来，不猜
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_UNSPECIFIED
	}
}

// codeFor 给出与 reason 匹配的外层 StatusCode。
//
// 外层 code 决定 SDK 的恢复行为（IsRetryable / NeedsReResolve），typed detail
// 决定 UI 怎么解释。两者必须一致：一个 FAILED_PRECONDITION 配
// PACKAGE_NOT_FOUND 会让 SDK 不重新 Resolve、UI 却说找不到包，互相矛盾。
func codeFor(r pkgv1.PackageManagerReason) ipcv1.StatusCode {
	switch r {
	case pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_PACKAGE_NOT_FOUND:
		return ipcv1.StatusCode_STATUS_CODE_NOT_FOUND
	case pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_MANIFEST_INVALID,
		pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ARCHIVE_INVALID:
		// 包本身有问题 = 调用方给错了东西
		return ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
	case pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_UNSPECIFIED:
		return ipcv1.StatusCode_STATUS_CODE_INTERNAL
	default:
		// 签名/digest/降级/ABI/血统/不可变：前置条件不满足，重试无用
		return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION
	}
}
