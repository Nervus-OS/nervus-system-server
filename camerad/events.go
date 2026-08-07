//go:build linux

// 本文件推送控制面事件。
//
// # 控制面上【没有】逐帧事件
//
// 共享内存环自带 eventfd 唤醒，消费方阻塞在它上面即可。再在控制面推一条
// 「有新帧」等于把每帧一次的流量搬回控制面——正是共享内存数据面存在的理由。
//
// 所以这里只有两种事件，都不按帧发生：
//
//	StreamStateChanged  STATE     流开了/停了/坏了。中间态合并掉没有损失
//	DeviceError         RELIABLE  掉线、缓冲溢出、驱动错误。不能合并也不能丢
//
// # 推送失败为什么只记日志
//
// PublishEvent 是单向的，没有结果。返回的 error 只表示本地写失败（连接已断），
// 而那时整个服务已经在往下走停机流程了。事件被 nervud 拒绝（event_id 不在契约
// 里、超出速率）不会有任何回音，只在内核审计里留一条 PublishEventRejected。
package main

import (
	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	"google.golang.org/protobuf/proto"
)

// publishState 推送一次流状态变化。
func (c *cameraEndpoint) publishState(streamID uint64, phase camerav1.StreamPhase) {
	payload, err := proto.Marshal(&camerav1.StreamState{
		StreamId: streamID,
		Phase:    phase,
	})
	if err != nil {
		c.log.Warn("camerad: 构造 StreamState 失败", "stream_id", streamID, "err", err)
		return
	}
	// 【不带时间戳】：状态变化的意义在于「发生了」，收到的先后就是全部时序
	// 信息。硬塞一个 time.Now() 会让消费方以为那是设备上的时刻。
	c.publish(uint32(camerav1.CameraEvent_CAMERA_EVENT_STREAM_STATE_CHANGED), payload, 0)
}

// publishDeviceError 推送一次设备级错误。
//
// droppedFrames 只在 BUFFER_OVERRUN 下有意义，其余传 0。
func (c *cameraEndpoint) publishDeviceError(
	streamID uint64, kind camerav1.CameraDeviceErrorKind, droppedFrames uint64,
) {
	payload, err := proto.Marshal(&camerav1.CameraDeviceError{
		StreamId:      streamID,
		Kind:          kind,
		DroppedFrames: droppedFrames,
	})
	if err != nil {
		c.log.Warn("camerad: 构造 CameraDeviceError 失败", "stream_id", streamID, "err", err)
		return
	}
	c.publish(uint32(camerav1.CameraEvent_CAMERA_EVENT_DEVICE_ERROR), payload, 0)
}

func (c *cameraEndpoint) publish(eventID uint32, payload []byte, monotonicNanos uint64) {
	if c.ep == nil {
		// endpoint 还没报到（装配期）或已经撤下（停机中）。
		return
	}
	if err := c.ep.PublishEventAt(eventID, payload, monotonicNanos); err != nil {
		c.log.Debug("camerad: 推送事件失败", "event_id", eventID, "err", err)
	}
}
