// Command pkgmanagerd 是 Nervus OS 的软件安装系统服务。
//
// 它跑在 systemd 瞬态 unit 里（由 nervud 经 authority 拉起），身份是一个普通
// Package（UID 在 20000-59999 段），沙箱与其它包完全一致。唯一的特权是被内核
// 显式放行连管理通道——见 nervud 的 admin.Config.ServiceUID。
//
// 生命周期：
//
//	nervud 启动扫描 → 发现 nervus.pkgmanagerd（always-on）→ 经 systemd 拉起
//	→ 本进程连控制面握手 → RegisterEndpoint 报到 → Serve 循环
//	→ nervud 停止时 BindsTo=nervud.service 连带停掉本 unit
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nervus-os/nervus-ipc/go/sdk"
)

// componentID 必须与 manifest.json 里 components[].id 一致。
//
// 它是握手时的 declared_component_id：nervud 会用对端 PID → cgroup →
// systemd unit → 启动记录去核对，对不上直接拒绝握手。填错不会让你冒充别人，
// 只会让你连不上——而错误信息是 UNAUTHENTICATED，看起来像权限问题。
const componentID = "main"

// interfaceID 是本服务提供的接口。
//
// 【暂定】：这个接口的 .proto 还没进 nervus-ipc。落地后要按 method_registry
// 机制挂 method_meta，届时此处与 method_id 一并对齐，以 proto 为准。
const interfaceID = "nervus.interface.pkg.manager"

func main() {
	sockPath := flag.String("sock", sdk.DefaultSockPath, "nervud 控制面 UDS")
	adminSock := flag.String("admin-sock", DefaultSockPath, "nervud 管理通道 UDS")
	logLevel := flag.String("log-level", "info", "日志级别 debug/info/warn/error")
	flag.Parse()

	log := newLogger(*logLevel)
	slog.SetDefault(log)

	if err := run(*sockPath, *adminSock, log); err != nil {
		log.Error("pkgmanagerd exited", "err", err)
		os.Exit(1)
	}
	log.Info("pkgmanagerd stopped")
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	// 写 stderr：systemd 会把它收进 journal，不需要自己管日志文件轮转。
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

func run(sockPath, adminSock string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	admin := newAdminClient(adminSock)
	svc := newService(admin, log)

	// 先连控制面。连不上就直接失败退出——systemd 会按退避策略重启，
	// 而 nervud 的 service 监督链会在反复崩溃时按 criticality 处置。
	// 在这里自己写重试循环是错的：那会掩盖「nervud 没起来」这个事实，
	// 让本进程看起来健康。
	host, err := sdk.NewServiceHost(sdk.Config{
		SockPath:    sockPath,
		ComponentID: componentID,
		SDKName:     "nervus-system-server",
		SDKVersion:  "0.1.0",
		Log:         log,
	})
	if err != nil {
		return fmt.Errorf("连接控制面: %w", err)
	}
	defer func() { _ = host.Close() }()

	id := host.Identity()
	log.Info("pkgmanagerd: handshake ok", "package_id", id.PackageID, "component_id", id.ComponentID)

	// 【必须在报到之前把 handler 全部注册完】。报到成功那一刻 nervud 就可能
	// 转发 Dispatch，此时还没注册的 method 会被回 NOT_FOUND——调用方会以为
	// 这个方法不存在，而不是「服务还没准备好」，这种错误很难往回追。
	registerHandlers(host, svc, log)

	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	epID, err := host.RegisterEndpoint(regCtx, sdk.RegisterRequest{
		InterfaceID: interfaceID,
		Major:       1,
		// ResourceHandle 留空 = 未指定。内核 RegisterEndpoint 步骤 4 只在非空时
		// 校验它必须是 Resource Registry 里的已知句柄；本接口不绑任何物理资源，
		// 填一个反而会因为不在表里被 INVALID_ARGUMENT 拒掉。
		//
		// SchemaHash 留空：本接口的 .proto 还没进 nervus-ipc。内核步骤 5 目前
		// 【只记录不比对】（v1 尚无权威 schema Registry），所以现在留空不会被拒；
		// 但 schema Registry 落地后比对会开启，届时必须填上真实 hash。
	})
	if err != nil {
		return fmt.Errorf("注册 endpoint: %w", err)
	}
	log.Info("pkgmanagerd: endpoint registered", "endpoint_id", epID, "interface", interfaceID)

	// Serve 阻塞到连接断开；ctx 取消时主动 Close 让它返回。
	done := make(chan error, 1)
	go func() { done <- host.Serve() }()

	select {
	case <-ctx.Done():
		log.Info("pkgmanagerd: shutting down")
		// 优雅撤下 endpoint，让 nervud 能把这件事当作可预期事件处理
		// （向持有 binding 的调用方发 EndpointDied{SERVICE_SHUTTING_DOWN}），
		// 而不是与「进程崩了」混为一谈。
		unregCtx, unregCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer unregCancel()
		if err := host.UnregisterEndpoint(unregCtx, epID, true); err != nil {
			log.Debug("pkgmanagerd: unregister failed", "err", err)
		}
		_ = host.Close()
		<-done
		return nil
	case err := <-done:
		if err != nil && !errors.Is(err, sdk.ErrClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
}

// registerHandlers 把方法接到业务实现上。
//
// # 现状：一个 handler 都不注册，这是有意的
//
// nervus.interface.pkg.manager 的 .proto 还没进 nervus-ipc，没有生成类型可用。
// 而手搓一套临时的 payload 编码是明确的红线（绝不手搓信封）——临时格式最后
// 都会变成永久格式，还没有 golden vectors 兜着。
//
// 也没有注册一批「返回未实现」的占位 handler：协议的 StatusCode 里根本没有
// UNIMPLEMENTED，硬凑一个语义相近的码只会误导调用方（UNAVAILABLE 意味着
// 「稍后重试」，而这里重试多少次都一样）。SDK 对未注册的 method_id 本来就
// fail closed 回 NOT_FOUND —— 「这个方法不存在」正是此刻的事实，不需要包装。
//
// 于是现在这条链能验到：服务起得来、握得上手、报得了到、能被 App Resolve 到，
// 调用任何方法得到一个准确的 NOT_FOUND。proto 落地后在这里逐个 Handle 即可，
// 服务骨架不用动。
func registerHandlers(host *sdk.ServiceHost, svc *Service, log *slog.Logger) {
	log.Info("pkgmanagerd: no method handlers registered yet",
		"reason", "nervus.interface.pkg.manager proto not landed in nervus-ipc",
		"effect", "calls return NOT_FOUND")

	// proto 落地后在此处接线，形如：
	//
	//	host.Handle(MethodInstall, func(cc sdk.CallContext, p []byte) ([]byte, error) {
	//	    var req pkgv1.InstallRequest
	//	    if err := proto.Unmarshal(p, &req); err != nil {
	//	        return nil, &sdk.StatusError{Code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT}
	//	    }
	//	    info, err := svc.InstallFromFile(req.GetNspkgPath())
	//	    ...
	//	})
	_ = host
	_ = svc
}
