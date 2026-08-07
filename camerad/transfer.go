//go:build linux

// 本文件封装 Transfer Control 三步：Begin / Commit / Abort。
//
// # 为什么必须在同一条 ServiceHost 连接上发起
//
// BeginTransfer 要填 origin_route_id，而 route_id 是【连接作用域】句柄——它是
// nervud 为「这条连接上的这次 Dispatch」分配的号。另开一条 Client 连接去发
// BeginTransfer，即使进程身份完全相同，nervud 也查不到那个 route，回 NOT_FOUND。
//
// 所以本文件的方法都挂在 cameraEndpoint 上，用它持有的 ServiceHost。
package main

import (
	"context"
	"fmt"
	"time"

	transferv1 "github.com/nervus-os/nervus-ipc/protocol/interface/transferv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

// interfaceTransferControl 是 nervud 的内建 Transfer Control 接口。
const interfaceTransferControl = "nervus.interface.transfer.control"

// transferCallTimeout 是三个控制方法的超时。
//
// 与 transfer_control.proto 声明的 max_timeout_ms（3000）一致：填更大的值不会
// 生效（nervud 按 MethodMeta 收紧），只会让本地的等待与实际行为对不上。
const transferCallTimeout = 3 * time.Second

// resolveTransferControl 懒解析 Transfer Control endpoint 并缓存。
//
// 【懒解析而不是启动时解析】：一台没有摄像头的机器上 camerad 永远不会开流，
// 启动时强行解析只会让它在一个用不到的依赖上失败退出。
//
// 缓存的 binding 可能失效（内核重启过）。失效表现为 Begin 时的 NOT_FOUND，
// 由 beginTransfer 重新解析一次。
func (c *cameraEndpoint) resolveTransferControl(ctx context.Context) (sdk.Endpoint, error) {
	c.mu.Lock()
	if c.transferResolved {
		ep := c.transfer
		c.mu.Unlock()
		return ep, nil
	}
	c.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, transferCallTimeout)
	defer cancel()
	ep, err := c.host.ResolveEndpoint(rctx, sdk.ResolveRequest{
		InterfaceID: interfaceTransferControl,
		MinMajor:    1,
		MaxMajor:    1,
		// 不填 selector：Transfer Control 不绑任何 Resource。v2 起空 selector
		// 就是字面意思，不再隐式指向底盘。
	})
	if err != nil {
		return sdk.Endpoint{}, fmt.Errorf("camerad: 解析 %s: %w", interfaceTransferControl, err)
	}

	c.mu.Lock()
	c.transfer = ep
	c.transferResolved = true
	c.mu.Unlock()
	return ep, nil
}

func (c *cameraEndpoint) invalidateTransferControl() {
	c.mu.Lock()
	c.transferResolved = false
	c.mu.Unlock()
}

// beginTransfer 为本次 Dispatch 建一条 PREPARED 的共享内存管子。
//
// maxPacketBytes 传单帧上限：nervud 据它定共享内存槽大小。传小了帧装不下，
// 表现是每一帧都写失败；传大了只是多占些内存。
func (c *cameraEndpoint) beginTransfer(
	cc sdk.CallContext, maxPacketBytes uint32,
) (*transferv1.BeginTransferResponse, error) {
	response, err := c.callTransfer(cc, maxPacketBytes)
	if err == nil {
		return response, nil
	}
	// binding 失效（内核重启过 / 权限被撤）：重新解析一次再试。
	// 不重试的话，nervud 重启之后本进程会永久失去开流能力，直到自己也重启。
	if !sdk.NeedsReResolve(err) {
		return nil, err
	}
	c.log.Warn("camerad: Transfer Control binding 失效，重新解析", "err", err)
	c.invalidateTransferControl()
	return c.callTransfer(cc, maxPacketBytes)
}

func (c *cameraEndpoint) callTransfer(
	cc sdk.CallContext, maxPacketBytes uint32,
) (*transferv1.BeginTransferResponse, error) {
	ep, err := c.resolveTransferControl(cc.Ctx)
	if err != nil {
		return nil, deviceUnavailable()
	}

	req, err := proto.Marshal(&transferv1.BeginTransferRequest{
		OriginRouteId: cc.RouteID,
		Direction:     ipcv1.TransferDirection_TRANSFER_DIRECTION_PROVIDER_TO_CALLER,
		// 显式要 SHARED_MEMORY_RING 而不是留 UNSPECIFIED 让 nervud 选基线：
		// 基线是 FRAMED_RELAY，一路 1080p30 走它就是 ~180 MB/s 逐帧穿过内核。
		// camera.proto 的 allowed_modes 只列了 RING，这里与它一致。
		PreferredMode:           ipcv1.TransferMode_TRANSFER_MODE_SHARED_MEMORY_RING,
		RequestedMaxPacketBytes: maxPacketBytes,
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(cc.Ctx, transferCallTimeout)
	defer cancel()
	out, err := c.host.Call(ctx, ep.EndpointID,
		uint32(transferv1.TransferControlMethod_TRANSFER_CONTROL_METHOD_BEGIN_TRANSFER),
		req, transferCallTimeout)
	if err != nil {
		return nil, err
	}

	var response transferv1.BeginTransferResponse
	if err := proto.Unmarshal(out, &response); err != nil {
		return nil, err
	}
	if response.GetProvider() == nil || response.GetCaller() == nil {
		// 少一半句柄意味着这条管子只有一端能接上，另一端会永久等待。
		return nil, fmt.Errorf("camerad: BeginTransfer 只返回了一半句柄")
	}
	return &response, nil
}

// commitTransfer 放行消费端。【必须在业务响应确定能返回之后】，见 stream.go 文件头。
func (c *cameraEndpoint) commitTransfer(ctx context.Context, transferID []byte) error {
	ep, err := c.resolveTransferControl(ctx)
	if err != nil {
		return deviceUnavailable()
	}
	req, err := proto.Marshal(&transferv1.CommitTransferRequest{TransferId: transferID})
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, transferCallTimeout)
	defer cancel()
	_, err = c.host.Call(cctx, ep.EndpointID,
		uint32(transferv1.TransferControlMethod_TRANSFER_CONTROL_METHOD_COMMIT_TRANSFER),
		req, transferCallTimeout)
	return err
}

// abortTransfer 放弃一条还没 Commit 的管子。
//
// 【不返回错误，只记日志】：它跑在失败路径的 defer 里，调用方已经在返回一个
// 更要紧的错误了。这里再叠一个只会盖住根因。
//
// nervud 在 route 取消或连接断开时也会自动 Abort，所以漏掉一次不会永久泄漏——
// 但那要等到连接断开，中间这段时间那条管子一直占着额度。
func (c *cameraEndpoint) abortTransfer(ctx context.Context, transferID []byte) {
	ep, err := c.resolveTransferControl(ctx)
	if err != nil {
		c.log.Warn("camerad: Abort 时解析 Transfer Control 失败", "err", err)
		return
	}
	req, err := proto.Marshal(&transferv1.AbortTransferRequest{TransferId: transferID})
	if err != nil {
		c.log.Warn("camerad: 构造 AbortTransfer 失败", "err", err)
		return
	}
	// 用 Background 而不是 ctx：这条路径常常正是因为 ctx 已经超时才走到的，
	// 拿一个已经取消的 ctx 去 Abort 等于不 Abort。
	cctx, cancel := context.WithTimeout(context.Background(), transferCallTimeout)
	defer cancel()
	if _, err := c.host.Call(cctx, ep.EndpointID,
		uint32(transferv1.TransferControlMethod_TRANSFER_CONTROL_METHOD_ABORT_TRANSFER),
		req, transferCallTimeout); err != nil {
		c.log.Warn("camerad: AbortTransfer 失败", "err", err)
	}
}
