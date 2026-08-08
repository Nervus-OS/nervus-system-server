//go:build linux

// 本文件是取流的生命周期：从 OpenStream 建管子，到帧泵把 V4L2 缓冲搬进共享内存环。
//
// # 数据面不经过 nervud，也不经过本进程的控制面
//
//	App --Request(OpenStream)--> nervud --Dispatch--> camerad
//	camerad --BeginTransfer--> nervud                      建管子
//	camerad --AttachTransfer--> nervud-transfer.sock       自己接上生产端
//	camerad --CommitTransfer--> nervud                     放行消费端
//	camerad --OpenStreamResponse(caller handle)--> App     把凭证交出去
//	App --AttachTransfer--> nervud-transfer.sock           接上消费端
//	                    ↓
//	         此后帧在共享内存环里流动，nervud 与本文件的控制面都不在数据路径上
//
// 【顺序不能变】。Commit 之前 caller ticket 附不上；而 Commit 必须在业务响应
// 确定能成功返回之后——先 Commit 再失败的话，App 拿不到 handle，那条 Transfer
// 却已经放行，成了一条谁都关不掉的悬空管子。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

// v4l2BufferCount 是向驱动申请的采集缓冲数。
//
// 4 个：太少（2）会让一次 GC 停顿就造成驱动侧丢帧；太多只是白占内核内存，
// 而且【不会降低延迟】——多出来的缓冲只是让积压更深，消费方拿到的仍是旧帧。
const v4l2BufferCount = 4

// frameWaitMillis 是帧泵一次 poll 的等待上限。
//
// 它决定「设备不出帧时多久能察觉」。取 1 秒：比任何合理帧率的周期都长得多，
// 不会误报；又足够短，让掉线的摄像头在一秒内进入 FAULT 而不是让 App 干等。
const frameWaitMillis = 1000

// stream 是一路正在进行的取流。
type stream struct {
	id     uint64
	device *v4l2Device
	conn   *sdk.TransferConn
	format *camerav1.StreamFormat
	rate   uint32

	// ctx / cancel 只属于帧泵。CloseStream 走 cancel + 等 done，而不是自己去
	// STREAMOFF——设备句柄非并发安全，只能有一个 goroutine 碰它。
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// cameraEndpoint 是绑到一路摄像头的 endpoint：它的 handler 与状态。
type cameraEndpoint struct {
	host    *sdk.ServiceHost
	ep      *sdk.EndpointHost
	role    string
	device  Device
	log     *slog.Logger
	formats []formatOption

	mu       sync.Mutex
	streams  map[uint64]*stream
	nextID   uint64
	transfer sdk.Endpoint
	// transferResolved 记录 Transfer Control endpoint 是否已解析。
	transferResolved bool
}

// maxStreamsPerCamera 与 camera.proto 里 OpenStream 的 transfer.max_streams 一致。
//
// 【两处必须相同】：nervud 按 MethodMeta 收紧，本地再拦一道只是为了给出
// 一个带 typed detail 的错误（TOO_MANY_STREAMS），而不是让调用方拿到
// 一个泛泛的 RESOURCE_EXHAUSTED。
const maxStreamsPerCamera = 4

func newCameraEndpoint(
	host *sdk.ServiceHost, role string, device Device, log *slog.Logger,
) (*cameraEndpoint, error) {
	dev, err := openDevice(device.Node)
	if err != nil {
		return nil, err
	}
	// 【枚举一次，之后不再枚举】：ENUM_FMT 在取流期间可能失败或阻塞，而
	// DescribeStream 是 App 建流之前必调的第一个方法。开机时问清楚、之后
	// 只答不问，避免把一次驱动抖动变成「App 完全用不了摄像头」。
	formats, err := dev.enumerateFormats()
	closeErr := dev.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		log.Warn("camerad: 关闭枚举用的设备句柄失败", "node", device.Node, "err", closeErr)
	}
	if len(formats) == 0 {
		return nil, fmt.Errorf("camerad: %s 没有报告任何离散取流格式", device.Node)
	}

	return &cameraEndpoint{
		host:    host,
		role:    role,
		device:  device,
		log:     log.With("role", role, "node", device.Node),
		formats: formats,
		streams: make(map[uint64]*stream),
	}, nil
}

// install 把三个方法挂上 endpoint 并报到。
func (c *cameraEndpoint) install(ctx context.Context, schemaHash []byte) error {
	c.ep = c.host.NewEndpoint(sdk.RegisterRequest{
		InterfaceID:    interfaceCamera,
		Major:          1,
		SchemaHash:     schemaHash,
		ResourceHandle: c.role,
	})
	for id, fn := range map[uint32]sdk.Handler{
		uint32(camerav1.CameraMethod_CAMERA_METHOD_DESCRIBE_STREAM): c.describeStream,
		uint32(camerav1.CameraMethod_CAMERA_METHOD_OPEN_STREAM):     c.openStream,
		uint32(camerav1.CameraMethod_CAMERA_METHOD_CLOSE_STREAM):    c.closeStream,
	} {
		if err := c.ep.Handle(id, fn); err != nil {
			return err
		}
	}
	endpointID, err := c.ep.Register(ctx)
	if err != nil {
		return fmt.Errorf("camerad: 报到 %s: %w", c.role, err)
	}
	c.log.Info("camerad: endpoint registered", "endpoint_id", endpointID)
	return nil
}

func (c *cameraEndpoint) describeStream(cc sdk.CallContext, _ []byte) ([]byte, error) {
	out := &camerav1.StreamDescription{
		Formats: make([]*camerav1.StreamFormat, 0, len(c.formats)),
	}
	for _, option := range c.formats {
		pixel, ok := pixelFormatOf(option.PixelFormat)
		if !ok {
			// 平台不认识的 fourcc。【跳过而不是报 UNSPECIFIED】：报出来的话
			// App 会拿到一个自己无法解释布局的格式，然后按它去解析内存。
			continue
		}
		out.Formats = append(out.Formats, &camerav1.StreamFormat{
			PixelFormat:   pixel,
			Width:         option.Width,
			Height:        option.Height,
			FrameRates:    option.FrameRates,
			MaxFrameBytes: option.MaxBytes,
		})
	}
	return proto.Marshal(out)
}

func (c *cameraEndpoint) openStream(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var req camerav1.OpenStreamRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			camerav1.CameraReason_CAMERA_REASON_UNSPECIFIED, camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	option, ok := c.matchFormat(req.GetFormat(), req.GetFrameRate())
	if !ok {
		// 【不做就近匹配】：静默给一个更小的分辨率会让 App 按错误的尺寸解析
		// 共享内存，画面撕裂却查不出原因。
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			camerav1.CameraReason_CAMERA_REASON_FORMAT_NOT_SUPPORTED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	c.mu.Lock()
	if len(c.streams) >= maxStreamsPerCamera {
		c.mu.Unlock()
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_RESOURCE_EXHAUSTED,
			camerav1.CameraReason_CAMERA_REASON_TOO_MANY_STREAMS,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}
	c.nextID++
	streamID := c.nextID
	c.mu.Unlock()

	begun, err := c.beginTransfer(cc, option.MaxBytes)
	if err != nil {
		return nil, err
	}

	// 从这里到 Commit 之间的任何失败都必须 Abort，否则那条 PREPARED Transfer
	// 会一直占着直到连接断开。
	committed := false
	defer func() {
		if !committed {
			c.abortTransfer(cc.Ctx, begun.GetProvider().GetTransferId())
		}
	}()

	st, err := c.startCapture(streamID, option, req.GetFrameRate(), begun.GetProvider())
	if err != nil {
		return nil, err
	}

	response, err := proto.Marshal(&camerav1.OpenStreamResponse{
		StreamId:  streamID,
		Transfer:  begun.GetCaller(),
		Format:    st.format,
		FrameRate: st.rate,
	})
	if err != nil {
		c.teardown(st)
		return nil, err
	}

	// 【响应确定能返回之后才 Commit】。先 Commit 再序列化失败的话，App 拿不到
	// handle，那条 Transfer 却已经放行——成了一条谁都关不掉的悬空管子。
	if err := c.commitTransfer(cc.Ctx, begun.GetProvider().GetTransferId()); err != nil {
		c.teardown(st)
		return nil, err
	}
	committed = true

	c.mu.Lock()
	c.streams[streamID] = st
	c.mu.Unlock()

	// 【登记事件归属】：这两个事件声明了 scoped，不登记的话订阅方一律订不上。
	//
	// 用 cc.RouteID 而不是自报调用方身份——它证明「这条流属于正在调我的这一位」，
	// 而那一位是谁 nervud 自己知道。
	//
	// 放在 Commit 之后：那之前任何一步失败都会走 teardown，而 teardown 会
	// 撤销登记；登记得太早只会多一次无谓的撤销。
	if err := c.ep.BindEventScope(streamID, cc.RouteID); err != nil {
		// 登记失败不该让已经建好的流失败——帧照样流得动，只是订阅不了状态
		// 事件。记 WARN 让它可见，而不是把一次可用的取流变成失败。
		c.log.Warn("camerad: 登记事件归属失败，本流的状态事件将订阅不到",
			"stream_id", streamID, "err", err)
	}

	c.publishState(streamID, camerav1.StreamPhase_STREAM_PHASE_STARTING)
	go c.pump(st)
	return response, nil
}

func (c *cameraEndpoint) closeStream(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var req camerav1.CloseStreamRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			camerav1.CameraReason_CAMERA_REASON_UNSPECIFIED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	c.mu.Lock()
	st, ok := c.streams[req.GetStreamId()]
	if ok {
		delete(c.streams, req.GetStreamId())
	}
	c.mu.Unlock()

	if !ok {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			camerav1.CameraReason_CAMERA_REASON_STREAM_NOT_FOUND,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}
	c.teardown(st)
	// 【先发状态再撤归属】：撤销之后订阅方就收不到了，而 STOPPED 正是它
	// 最需要的那一条。
	c.publishState(req.GetStreamId(), camerav1.StreamPhase_STREAM_PHASE_STOPPED)
	c.releaseScope(req.GetStreamId())
	return nil, nil
}

// releaseScope 撤销一条流的事件归属。
//
// 【必须显式撤销】：不撤的话，一个反复开关流的 camerad 会让 nervud 的归属表
// 无界增长。连接断开与 endpoint 撤下会连带清理，但那兜的是崩溃，不是正常关闭。
func (c *cameraEndpoint) releaseScope(streamID uint64) {
	if c.ep == nil {
		return
	}
	if err := c.ep.ReleaseEventScope(streamID); err != nil {
		c.log.Debug("camerad: 撤销事件归属失败", "stream_id", streamID, "err", err)
	}
}

// matchFormat 在枚举结果里找【完全一致】的组合。
func (c *cameraEndpoint) matchFormat(
	want *camerav1.StreamFormat, rate uint32,
) (formatOption, bool) {
	if want == nil {
		return formatOption{}, false
	}
	fourcc, ok := fourccOf(want.GetPixelFormat())
	if !ok {
		return formatOption{}, false
	}
	for _, option := range c.formats {
		if option.PixelFormat != fourcc ||
			option.Width != want.GetWidth() || option.Height != want.GetHeight() {
			continue
		}
		if rate == 0 {
			return option, true
		}
		for _, supported := range option.FrameRates {
			if supported == rate {
				return option, true
			}
		}
		return formatOption{}, false
	}
	return formatOption{}, false
}

// startCapture 打开设备、设格式、开流，并接上数据面生产端。
func (c *cameraEndpoint) startCapture(
	streamID uint64, option formatOption, rate uint32, handle *ipcv1.TransferHandle,
) (*stream, error) {
	dev, err := openDevice(c.device.Node)
	if err != nil {
		return nil, deviceUnavailable()
	}

	actual, err := dev.setFormat(option.PixelFormat, option.Width, option.Height)
	if err != nil {
		_ = dev.Close()
		return nil, deviceUnavailable()
	}
	// S_FMT 是协商而非设置：驱动会把不支持的请求改成最近的支持值并【不报错】。
	// 回读对不上就必须失败——继续下去等于按请求的尺寸解析按别的尺寸排布的内存。
	if actual.width != option.Width || actual.height != option.Height ||
		actual.pixelformat != option.PixelFormat {
		_ = dev.Close()
		c.log.Warn("camerad: 驱动改写了请求的格式",
			"want_w", option.Width, "want_h", option.Height,
			"got_w", actual.width, "got_h", actual.height)
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			camerav1.CameraReason_CAMERA_REASON_FORMAT_NOT_SUPPORTED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}
	dev.setFrameRate(rate)

	if err := dev.startStreaming(v4l2BufferCount); err != nil {
		_ = dev.Close()
		c.log.Warn("camerad: 开流失败", "err", err)
		return nil, deviceUnavailable()
	}

	attachCtx, cancelAttach := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := sdk.AttachTransfer(attachCtx, handle)
	cancelAttach()
	if err != nil {
		_ = dev.Close()
		c.log.Warn("camerad: 附着数据面失败", "err", err)
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
			camerav1.CameraReason_CAMERA_REASON_DRIVER_FAULT,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	pixel, _ := pixelFormatOf(option.PixelFormat)
	ctx, cancel := context.WithCancel(context.Background())
	return &stream{
		id:     streamID,
		device: dev,
		conn:   conn,
		format: &camerav1.StreamFormat{
			PixelFormat:   pixel,
			Width:         option.Width,
			Height:        option.Height,
			FrameRates:    option.FrameRates,
			MaxFrameBytes: option.MaxBytes,
		},
		rate:   rate,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}, nil
}

// pump 是帧泵：把 V4L2 缓冲搬进共享内存环。
//
// # 它是唯一碰设备的 goroutine
//
// v4l2Device 不是并发安全的，DQBUF/QBUF 必须串行。让控制面 handler 也去碰设备
// 会造成两个线程同时操作同一个 fd——V4L2 对此的反应是未定义的。CloseStream
// 走 cancel + 等 done，而不是自己去 STREAMOFF。
//
// # 丢帧策略
//
// 环满时【丢新帧】并继续，不阻塞。视频流天生 lossy：阻塞在这里会让 V4L2 的
// 缓冲池很快耗尽，驱动开始在内核里丢帧——那时丢的是哪一帧、丢了多少，
// 用户态完全看不见。在这里丢，至少 dropped 计数是准的。
func (c *cameraEndpoint) pump(st *stream) {
	defer close(st.done)

	var dropped uint64

	for {
		select {
		case <-st.ctx.Done():
			return
		default:
		}

		if err := st.device.waitFrame(frameWaitMillis); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				// 一秒没出帧。可能只是低帧率设备，继续等。
				continue
			}
			c.log.Warn("camerad: 等待帧失败", "stream_id", st.id, "err", err)
			c.publishDeviceError(st.id,
				camerav1.CameraDeviceErrorKind_CAMERA_DEVICE_ERROR_KIND_DISCONNECTED, dropped)
			c.publishState(st.id, camerav1.StreamPhase_STREAM_PHASE_FAULT)
			c.releaseScope(st.id)
			return
		}

		frame, err := st.device.dequeue()
		if errors.Is(err, errNoFrame) {
			continue
		}
		if err != nil {
			c.log.Warn("camerad: 取帧失败", "stream_id", st.id, "err", err)
			c.publishDeviceError(st.id,
				camerav1.CameraDeviceErrorKind_CAMERA_DEVICE_ERROR_KIND_DRIVER_FAULT, dropped)
			c.publishState(st.id, camerav1.StreamPhase_STREAM_PHASE_FAULT)
			return
		}

		// 【时间戳原样透传】：这是传感器出帧的那一刻，不含后面任何一段排队延迟。
		// 用 time.Now() 替代会产出一个看起来合理、实际混进了调度抖动的数字。
		writeErr := st.conn.WriteRing(frame.Data, 0, frame.MonotonicNanos)
		if releaseErr := st.device.release(frame); releaseErr != nil {
			// 缓冲还不回去，池耗尽后再也取不到帧。这条路走不下去了。
			c.log.Error("camerad: 归还缓冲失败", "stream_id", st.id, "err", releaseErr)
			c.publishDeviceError(st.id,
				camerav1.CameraDeviceErrorKind_CAMERA_DEVICE_ERROR_KIND_DRIVER_FAULT, dropped)
			c.publishState(st.id, camerav1.StreamPhase_STREAM_PHASE_FAULT)
			return
		}
		if writeErr != nil {
			dropped++
			if dropped%100 == 1 {
				// 每丢 100 帧记一次：逐帧记日志会让一个消费不动的 App
				// 把日志盘写满，而那比丢帧严重得多。
				c.log.Warn("camerad: 消费方跟不上，丢帧",
					"stream_id", st.id, "dropped", dropped, "err", writeErr)
			}
			continue
		}
		if dropped > 0 {
			c.publishDeviceError(st.id,
				camerav1.CameraDeviceErrorKind_CAMERA_DEVICE_ERROR_KIND_BUFFER_OVERRUN, dropped)
			dropped = 0
		}
	}
}

// teardown 停一路流并释放全部资源。幂等。
func (c *cameraEndpoint) teardown(st *stream) {
	st.cancel()
	select {
	case <-st.done:
	case <-time.After(3 * time.Second):
		// 帧泵没在期限内退出。继续关设备会让它在一个已关闭的 fd 上做 ioctl。
		// 记下来，然后仍然关——泄漏一个 goroutine 好过泄漏一个摄像头。
		c.log.Error("camerad: 帧泵未能及时退出", "stream_id", st.id)
	}
	if err := st.conn.Close(); err != nil {
		c.log.Warn("camerad: 关闭数据面失败", "stream_id", st.id, "err", err)
	}
	if err := st.device.Close(); err != nil {
		c.log.Warn("camerad: 关闭设备失败", "stream_id", st.id, "err", err)
	}
}

// closeAll 在停机时收掉全部流。
func (c *cameraEndpoint) closeAll() {
	c.mu.Lock()
	streams := make([]*stream, 0, len(c.streams))
	for _, st := range c.streams {
		streams = append(streams, st)
	}
	c.streams = make(map[uint64]*stream)
	c.mu.Unlock()

	for _, st := range streams {
		c.teardown(st)
		c.releaseScope(st.id)
	}
}
