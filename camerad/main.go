//go:build linux

// Command camerad 是 Nervus OS 的通用摄像头服务。
//
// # 它把「一堆 /dev/videoN」变成「板上有哪几路摄像头」
//
// V4L2 只回答「这个节点能出什么格式」。它答不了「哪个是前视」——那是板级事实，
// 写在服务目录下的板级 JSON 里，由本服务翻译成 Catalog 里的语义资源。
//
// # 一路摄像头 = 两个 endpoint
//
//	nervus.interface.camera@1         采集，绑 nervus.resource.camera（共享观察）
//	nervus.interface.camera.config@1  配置，绑 nervus.resource.camera.config（独占租约）
//
// 拆开的理由见 config.go 文件头：两件事的资源语义相反，而内核租约按资源发。
//
// # 厂商怎么接进来
//
// 厂商【不实现标准接口】，而是实现自己命名空间下的私有接口
// （vendor.acme.camerad.interface.source@1 一类），由本服务转成标准接口。
// 这样标准语义（facing=front）由平台的板级配置给出，厂商无法自封；数据面
// 仍然直达，帧不经过本进程复制一次。
//
// 【那条路径尚未接线】：当前只支持 V4L2 设备。接厂商源需要本服务做一次
// Resolve 到私有接口并把它的 TransferHandle 转交给 App——那要求先有一个
// 真实的厂商 Provider 来定义私有契约的形状。
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

	"github.com/nervus-os/nervus-ipc/sdk"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

// defaultProfilePath 是板级配置在安装后的位置，与 manifest 的 digests 覆盖一致。
const defaultProfilePath = "/usr/lib/nervus/packages/nervus.camerad/board.json"

func main() {
	profilePath := flag.String("profile", defaultProfilePath, "板级摄像头配置 JSON")
	sockPath := flag.String("sock", sdk.DefaultSockPath, "nervud 控制面 UDS")
	dryRun := flag.Bool("dry-run", false, "只打印 role → 设备映射后退出，不连控制面")
	logLevel := flag.String("log-level", "info", "日志级别 debug/info/warn/error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := run(*profilePath, *sockPath, *dryRun, log); err != nil {
		log.Error("camerad exited", "err", err)
		os.Exit(1)
	}
	log.Info("camerad stopped")
}

func run(profilePath, sockPath string, dryRun bool, log *slog.Logger) error {
	profile, err := boardprofile.Load(profilePath)
	if err != nil {
		return err
	}
	log.Info("camerad: 板级配置已加载",
		"board", profile.Board,
		"fixed_cameras", len(profile.Cameras),
		"declared_roles", len(profile.AllRoles()))

	bindings, err := Bind(profile, listSysfsDevices)
	if err != nil {
		return err
	}
	reportBindings(profile, bindings, log)

	if dryRun {
		for _, b := range bindings {
			fmt.Printf("%-20s %-16s %s\n", b.Role, b.Device.Node, b.Device.Name)
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 连不上就直接失败退出，让 supervisor 按指数退避重启。自己写重试循环会
	// 掩盖「nervud 没起来」这个事实，让本进程看起来健康。
	host, err := sdk.NewServiceHost(sdk.Config{
		SockPath:    sockPath,
		ComponentID: componentID,
		SDKName:     "nervus-system-server",
		SDKVersion:  "0.1.0",
		Log:         log,
	})
	if err != nil {
		return fmt.Errorf("camerad: 连接控制面: %w", err)
	}
	defer func() { _ = host.Close() }()

	identity := host.Identity()
	log.Info("camerad: handshake ok",
		"package_id", identity.PackageID, "component_id", identity.ComponentID)

	captureHash, configHash, err := schemaHashes()
	if err != nil {
		return err
	}

	endpoints, err := installEndpoints(ctx, host, bindings, captureHash, configHash, log)
	// 【无论装配成不成功都要收流】：部分装配成功时已经有设备被打开了，
	// 直接返回会把它们连同 fd 一起泄漏到进程退出。
	defer closeAll(endpoints)
	if err != nil {
		return err
	}
	if len(endpoints) == 0 {
		// 一台没插摄像头的机器。【正常运行而不是退出】：退出会让 supervisor
		// 反复重启一个本来就没事可做的服务，而插上摄像头需要重启整机才生效。
		log.Warn("camerad: 没有任何可用摄像头，服务空转")
	}

	return serve(ctx, host, log)
}

// serve 跑读循环直到收到停机信号或连接断开。
func serve(ctx context.Context, host *sdk.ServiceHost, log *slog.Logger) error {
	served := make(chan error, 1)
	go func() { served <- host.Serve() }()

	select {
	case <-ctx.Done():
		log.Info("camerad: 收到停机信号")
		return nil
	case err := <-served:
		if err == nil || errors.Is(err, sdk.ErrClosed) {
			return nil
		}
		// 连接断开 = nervud 停了或本连接被判为慢消费者。退出让 supervisor
		// 重启，不要在这里假装还活着继续空转。
		return fmt.Errorf("camerad: 控制面连接断开: %w", err)
	}
}

// endpointPair 是一路摄像头的采集面与配置面。
type endpointPair struct {
	capture *cameraEndpoint
	config  *configEndpoint
}

// installEndpoints 为每个绑定装配两个 endpoint。
//
// # 单路失败不拖垮整机
//
// 一路摄像头装不上（设备打不开、驱动不报任何离散格式）时记 WARN 继续，而不是
// 整个服务退出。四路里坏一路就让另外三路也不可用，是把一个局部故障放大成整机
// 故障——而机器人上摄像头松动是常事。
//
// 【但报到失败是硬错误】：那说明 Catalog 与本二进制的契约对不上（schema hash
// 不符、资源没声明），后面每一路都会以同样方式失败，继续只是把同一个错误
// 重复 N 遍。
func installEndpoints(
	ctx context.Context,
	host *sdk.ServiceHost,
	bindings []Binding,
	captureHash, configHash []byte,
	log *slog.Logger,
) ([]endpointPair, error) {
	var installed []endpointPair

	for _, b := range bindings {
		capture, err := newCameraEndpoint(host, b.Role, b.Device, log)
		if err != nil {
			log.Warn("camerad: 这一路摄像头装配失败，跳过",
				"role", b.Role, "node", b.Device.Node, "err", err)
			continue
		}
		if err := capture.install(ctx, captureHash); err != nil {
			return installed, err
		}

		config := newConfigEndpoint(host, b.Role, b.Device, log)
		if err := config.install(ctx, configHash); err != nil {
			return installed, err
		}

		installed = append(installed, endpointPair{capture: capture, config: config})
	}
	return installed, nil
}

func closeAll(pairs []endpointPair) {
	for _, pair := range pairs {
		pair.capture.closeAll()
	}
}

// reportBindings 把映射结果写进日志。
//
// 声明了但没插的 role 单独用 WARN 说一句：板级集成阶段最常见的问题就是
// 「配置里写了前视，但线没插上」，而那时日志里只是【少了一行】——
// 少一行没人会注意到。
func reportBindings(profile *boardprofile.Profile, bindings []Binding, log *slog.Logger) {
	bound := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		bound[b.Role] = true
		log.Info("camerad: role 已绑定",
			"role", b.Role, "node", b.Device.Node,
			"usb_path", b.Device.USBPath, "name", b.Device.Name,
			"labels", profile.LabelsFor(b.Role))
	}
	for _, role := range profile.AllRoles() {
		if !bound[role] {
			log.Warn("camerad: 声明的 role 没有对应设备", "role", role)
		}
	}
}
