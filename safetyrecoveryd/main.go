// Command safetyrecoveryd 是 Nervus OS 的 Safety 恢复策略服务。
//
// # 机制在内核、策略在这里
//
// 内核的 safety.Rearm() 只复核一条【不可绕过的硬前置】：停止进度必须已经落定
// ——要么正常链已到 OUTPUT_DISABLED / STANDSTILL_CONFIRMED（输出确认关闭），
// 要么已入 DELIVERY_FAULT / STANDSTILL_TIMEOUT 这类必须人工处置的终态旁支。
// 停止还在途中就解开 latch，等于在还不知道输出有没有关掉的情况下重新放行运动。
//
// 「此刻该不该 re-arm」本身是策略：要看设备状态、故障原因、用户是否在场、
// 是否已经排除了触发停机的那个条件。那些会随产品演进，不该塞进内核。
//
// 本服务就是那个策略层。它通过标准 IPC 调 nervus.interface.safety.control ——
// 那是一个【内建 endpoint】，实现者是 nervud 自己，但调用形态与任何 Provider
// 提供的接口完全一样。
//
// # 它现在做什么
//
// 观察。周期性读安全态并记录状态迁移，尤其是进入 REARM_REQUIRED 的时刻与
// 当时的停止相位——那是事后复盘「为什么停机、停到哪一步」的第一手材料。
//
// # 它现在【不】做什么：自动 re-arm
//
// 没有实现任何自动解除停机的逻辑，这是刻意的。自动 re-arm 需要先回答
// 「触发停机的条件是否已经消失」，而那个判断依赖 Provider 的设备状态上报——
// safety.proto 的五条消息（SafetyHalt / HaltAccepted / StopProgress /
// StandstillConfirmed / ProviderFault）目前【没有承载通道】：内核侧的投递端
// 是 NopPath，上报端是 NopReports。
//
// 在拿不到设备真实状态之前写一个「等 N 秒就自动 re-arm」的策略，等于用一个
// 计时器决定机器什么时候可以重新动起来。那比不做更危险。
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

	"google.golang.org/protobuf/proto"

	safetyv1 "github.com/nervus-os/nervus-ipc/protocol/interface/safetyv1"
	"github.com/nervus-os/nervus-ipc/sdk"
)

// componentID 必须与 manifest.json 的 components[].id 一致。
// nervud 会用对端 PID → cgroup → systemd unit 核对它，对不上直接拒绝握手。
const componentID = "main"

// interfaceID 是内核的内建 Safety 接口。
//
// 注意这里是【消费】而不是提供：本服务不 RegisterEndpoint，它 ResolveEndpoint。
// 与 pkgmanagerd 正好相反——那个是 Provider，这个是 Client。
const interfaceID = "nervus.interface.safety.control"

// pollInterval 是安全态轮询周期。
//
// 【轮询而非订阅，理由已经变了】：订阅通道（envelope 40-45）现已接通，但
// nervus.interface.safety.control 这个内建接口【没有声明任何 EventMeta】——
// 订阅一个契约里不存在的 event_id 会被 RouteEvent 直接拒掉。
//
// 要改成订阅，得先在内核那侧给 safety 内建接口声明事件；那是 safety.proto
// 五条消息拿到承载通道之后的事（见文件头）。
//
// 2 秒是权衡：安全态迁移是低频事件（正常运行时根本不变），轮询太密只是白烧
// CPU 与审计条目；但停机之后运维盯着 UI 等状态更新，太慢会显得系统没反应。
const pollInterval = 2 * time.Second

func main() {
	sockPath := flag.String("sock", sdk.DefaultSockPath, "nervud 控制面 UDS")
	logLevel := flag.String("log-level", "info", "日志级别 debug/info/warn/error")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	slog.SetDefault(log)

	if err := run(*sockPath, log); err != nil {
		log.Error("safetyrecoveryd exited", "err", err)
		os.Exit(1)
	}
	log.Info("safetyrecoveryd stopped")
}

func parseLevel(s string) slog.Level {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return lv
}

func run(sockPath string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 连不上就直接失败退出，让 nervud 的 supervisor 按指数退避重启。
	// 自己写重试循环会掩盖「nervud 没起来」这个事实，让本进程看起来健康。
	c, err := sdk.Connect(sdk.Config{
		SockPath:    sockPath,
		ComponentID: componentID,
		SDKName:     "nervus-system-server",
		SDKVersion:  "0.1.0",
		Log:         log,
	})
	if err != nil {
		return fmt.Errorf("连接控制面: %w", err)
	}
	defer func() { _ = c.Close() }()

	id := c.Identity()
	log.Info("safetyrecoveryd: handshake ok",
		"package_id", id.PackageID, "component_id", id.ComponentID)

	ep, err := resolveSafety(ctx, c)
	if err != nil {
		return err
	}
	log.Info("safetyrecoveryd: resolved builtin safety endpoint",
		"endpoint_id", ep.EndpointID, "interface", interfaceID)

	return watch(ctx, c, ep, log)
}

// resolveSafety 解析内核的内建 Safety endpoint。
func resolveSafety(ctx context.Context, c *sdk.Client) (sdk.Endpoint, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	ep, err := c.ResolveEndpoint(rctx, sdk.ResolveRequest{
		InterfaceID: interfaceID,
		MinMajor:    1,
		MaxMajor:    1,
		// 不填 selector：Safety 是整机状态，不绑任何单个 Resource。
		//
		// v2 起这就是字面意思。v1 时代空 selector 会被隐式当成
		// {motion.base, base.main}，于是本次 Resolve 会莫名其妙连带解析出一个
		// 底盘句柄；那条隐式默认已经随 major 2 一起移除。
	})
	if err != nil {
		return sdk.Endpoint{}, fmt.Errorf("解析 %s: %w", interfaceID, err)
	}
	return ep, nil
}

// watch 周期性读安全态并记录迁移。
func watch(ctx context.Context, c *sdk.Client, ep sdk.Endpoint, log *slog.Logger) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var last *safetyv1.SafetyState

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		st, err := getState(ctx, c, ep)
		if err != nil {
			if errors.Is(err, sdk.ErrClosed) {
				// 连接没了 = nervud 停了或本连接被判为慢消费者。退出让
				// supervisor 重启，不要在这里假装还活着继续空转。
				return fmt.Errorf("控制面连接断开: %w", err)
			}
			if sdk.NeedsReResolve(err) {
				// binding 失效（内核重启过 / 权限被撤）。重新 Resolve 一次。
				log.Warn("safetyrecoveryd: endpoint invalidated, re-resolving", "err", err)
				newEP, rerr := resolveSafety(ctx, c)
				if rerr != nil {
					return rerr
				}
				ep = newEP
				continue
			}
			log.Warn("safetyrecoveryd: GetState failed", "err", err)
			continue
		}

		if changed(last, st) {
			logTransition(log, last, st)
			last = st
		}
	}
}

func getState(ctx context.Context, c *sdk.Client, ep sdk.Endpoint) (*safetyv1.SafetyState, error) {
	req, err := proto.Marshal(&safetyv1.GetSafetyStateRequest{})
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := c.Call(cctx, ep.EndpointID,
		uint32(safetyv1.SafetyControlMethod_SAFETY_CONTROL_METHOD_GET_STATE),
		req, 2*time.Second)
	if err != nil {
		return nil, err
	}
	var st safetyv1.SafetyState
	if err := proto.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("解码 SafetyState: %w", err)
	}
	return &st, nil
}

// changed 报告两次快照是否有实质变化。
//
// 三个字段都要比：epoch 单独变化（抢占、Provider 重启）不改变顶层状态，
// 但它意味着所有在途运动命令已被废止——那是必须记下来的事件。
func changed(a, b *safetyv1.SafetyState) bool {
	if a == nil {
		return true
	}
	return a.GetState() != b.GetState() ||
		a.GetMotionEpoch() != b.GetMotionEpoch() ||
		a.GetStopPhase() != b.GetStopPhase()
}

// logTransition 记录一次状态迁移。
//
// 进入 REARM_REQUIRED 的时刻与当时的停止相位是事后复盘「为什么停机、停到
// 哪一步」的第一手材料，用 WARN 级别让它在日志里显眼。
func logTransition(log *slog.Logger, from, to *safetyv1.SafetyState) {
	attrs := []any{
		"state", to.GetState().String(),
		"motion_epoch", to.GetMotionEpoch(),
		"stop_phase", to.GetStopPhase().String(),
	}
	if from != nil {
		attrs = append(attrs, "from_state", from.GetState().String())
	}

	switch to.GetState() {
	case safetyv1.TopState_TOP_STATE_SAFETY_LATCHED,
		safetyv1.TopState_TOP_STATE_REARM_REQUIRED:
		log.Warn("safetyrecoveryd: safety state changed", attrs...)
	default:
		log.Info("safetyrecoveryd: safety state changed", attrs...)
	}
}
