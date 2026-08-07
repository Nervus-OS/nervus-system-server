//go:build linux

// 本文件是配置面 endpoint：nervus.interface.camera.config@1。
//
// # 它为什么是独立的 endpoint
//
// 「看画面」和「改参数」的资源语义相反：前者多方共享（导航、录制、AI 检测
// 可以同时看同一路），后者必须独占（两个进程同时改曝光，最后谁说了算取决于
// 时序，而画面会在两个值之间跳）。
//
// 内核租约按资源发、一个 endpoint 绑一个资源，所以两件事必须是两个资源、
// 两个 endpoint。拆开之后，只看画面的 App 永远不需要碰租约，也就永远不会
// 因为别人在调参数而被拒绝。
//
// # 每次调用都重开设备
//
// 配置面不持有设备句柄。原因是采集面【可能正在用它】——V4L2 的设备句柄不是
// 并发安全的，两个 goroutine 同时 ioctl 同一个 fd 是未定义行为。
//
// V4L2 允许同一个节点被多次 open，控制类 ioctl（G_CTRL/S_CTRL）作用在设备上
// 而不是句柄上，因此另开一个句柄改参数是安全的，也是 v4l2-ctl 的做法。
// 代价是每次调用一次 open/close——配置是低频操作，这个代价换来的是
// 「配置面永远不会卡住采集面」。
package main

import (
	"context"
	"fmt"
	"log/slog"

	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

// V4L2 控制 ID。base 是 V4L2_CID_BASE = V4L2_CTRL_CLASS_USER | 0x900。
const (
	v4l2CIDBase = 0x00980000 | 0x900

	v4l2CIDBrightness       = v4l2CIDBase + 0
	v4l2CIDContrast         = v4l2CIDBase + 1
	v4l2CIDSaturation       = v4l2CIDBase + 2
	v4l2CIDAutoWhiteBalance = v4l2CIDBase + 12
	v4l2CIDGain             = v4l2CIDBase + 19
	v4l2CIDWhiteBalanceTemp = v4l2CIDBase + 26
	v4l2CIDRotate           = v4l2CIDBase + 34
	v4l2CIDExposureAuto     = 0x009a0000 + 1 // V4L2_CID_CAMERA_CLASS_BASE
	v4l2CIDExposureAbsolute = 0x009a0000 + 2
	v4l2CIDFocusAbsolute    = 0x009a0000 + 10
	v4l2CIDFocusAuto        = 0x009a0000 + 12
)

// controlMap 把平台可调项映射到 V4L2 控制 ID。
//
// 【用枚举而不是自由字符串键】：字符串键意味着每个厂商可以叫自己喜欢的名字
// （"exposure" / "exposure_time" / "ExposureAbsolute"），而 App 无从写出跨设备
// 可用的代码。厂商独有的可调项走厂商自己的接口，不挤进这张表。
var controlMap = map[camerav1.ControlKind]struct {
	id   uint32
	unit string
}{
	camerav1.ControlKind_CONTROL_KIND_BRIGHTNESS:         {v4l2CIDBrightness, ""},
	camerav1.ControlKind_CONTROL_KIND_CONTRAST:           {v4l2CIDContrast, ""},
	camerav1.ControlKind_CONTROL_KIND_SATURATION:         {v4l2CIDSaturation, ""},
	camerav1.ControlKind_CONTROL_KIND_EXPOSURE_TIME:      {v4l2CIDExposureAbsolute, "100us"},
	camerav1.ControlKind_CONTROL_KIND_AUTO_EXPOSURE:      {v4l2CIDExposureAuto, ""},
	camerav1.ControlKind_CONTROL_KIND_GAIN:               {v4l2CIDGain, ""},
	camerav1.ControlKind_CONTROL_KIND_WHITE_BALANCE:      {v4l2CIDWhiteBalanceTemp, "K"},
	camerav1.ControlKind_CONTROL_KIND_AUTO_WHITE_BALANCE: {v4l2CIDAutoWhiteBalance, ""},
	camerav1.ControlKind_CONTROL_KIND_FOCUS:              {v4l2CIDFocusAbsolute, ""},
	camerav1.ControlKind_CONTROL_KIND_AUTO_FOCUS:         {v4l2CIDFocusAuto, ""},
	camerav1.ControlKind_CONTROL_KIND_ROTATION:           {v4l2CIDRotate, "deg"},
}

// controlOrder 固定 ListControls / GetControls 的输出顺序。
//
// map 迭代顺序随机，直接遍历会让 UI 里的设置项每次刷新都在跳——
// 而这种问题几乎不会有人报成 bug。
var controlOrder = []camerav1.ControlKind{
	camerav1.ControlKind_CONTROL_KIND_BRIGHTNESS,
	camerav1.ControlKind_CONTROL_KIND_CONTRAST,
	camerav1.ControlKind_CONTROL_KIND_SATURATION,
	camerav1.ControlKind_CONTROL_KIND_AUTO_EXPOSURE,
	camerav1.ControlKind_CONTROL_KIND_EXPOSURE_TIME,
	camerav1.ControlKind_CONTROL_KIND_GAIN,
	camerav1.ControlKind_CONTROL_KIND_AUTO_WHITE_BALANCE,
	camerav1.ControlKind_CONTROL_KIND_WHITE_BALANCE,
	camerav1.ControlKind_CONTROL_KIND_AUTO_FOCUS,
	camerav1.ControlKind_CONTROL_KIND_FOCUS,
	camerav1.ControlKind_CONTROL_KIND_ROTATION,
}

// configEndpoint 是一路摄像头的配置面。
type configEndpoint struct {
	host *sdk.ServiceHost
	ep   *sdk.EndpointHost
	role string
	node string
	log  *slog.Logger
}

func newConfigEndpoint(
	host *sdk.ServiceHost, captureRole string, device Device, log *slog.Logger,
) *configEndpoint {
	role := boardprofile.ConfigRole(captureRole)
	return &configEndpoint{
		host: host,
		role: role,
		node: device.Node,
		log:  log.With("role", role, "node", device.Node),
	}
}

func (c *configEndpoint) install(ctx context.Context, schemaHash []byte) error {
	c.ep = c.host.NewEndpoint(sdk.RegisterRequest{
		InterfaceID:    interfaceCameraConfig,
		Major:          1,
		SchemaHash:     schemaHash,
		ResourceHandle: c.role,
	})
	for id, fn := range map[uint32]sdk.Handler{
		uint32(camerav1.CameraConfigMethod_CAMERA_CONFIG_METHOD_LIST_CONTROLS): c.listControls,
		uint32(camerav1.CameraConfigMethod_CAMERA_CONFIG_METHOD_GET_CONTROLS):  c.getControls,
		uint32(camerav1.CameraConfigMethod_CAMERA_CONFIG_METHOD_SET_CONTROLS):  c.setControls,
	} {
		if err := c.ep.Handle(id, fn); err != nil {
			return err
		}
	}
	endpointID, err := c.ep.Register(ctx)
	if err != nil {
		return fmt.Errorf("camerad: 报到配置面 %s: %w", c.role, err)
	}
	c.log.Info("camerad: config endpoint registered", "endpoint_id", endpointID)
	return nil
}

// withDevice 打开设备、执行 fn、关闭。见文件头「每次调用都重开设备」。
func (c *configEndpoint) withDevice(fn func(*v4l2Device) error) error {
	dev, err := openDevice(c.node)
	if err != nil {
		return deviceUnavailable()
	}
	defer func() {
		if closeErr := dev.Close(); closeErr != nil {
			c.log.Warn("camerad: 关闭配置面设备句柄失败", "err", closeErr)
		}
	}()
	return fn(dev)
}

func (c *configEndpoint) listControls(_ sdk.CallContext, _ []byte) ([]byte, error) {
	out := &camerav1.ControlDescriptions{}
	err := c.withDevice(func(dev *v4l2Device) error {
		for _, kind := range controlOrder {
			entry := controlMap[kind]
			info, ok := dev.queryControl(entry.id)
			if !ok {
				// 这台设备没有这一项。【跳过而不是报一个全 0 的范围】：
				// 全 0 范围会让 UI 画出一个拖不动的滑块。
				continue
			}
			out.Controls = append(out.Controls, &camerav1.ControlDescription{
				Kind:         kind,
				Min:          int64(info.Min),
				Max:          int64(info.Max),
				Step:         int64(info.Step),
				DefaultValue: int64(info.Default),
				Unit:         entry.unit,
				ReadOnly:     info.ReadOnly,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return proto.Marshal(out)
}

func (c *configEndpoint) getControls(_ sdk.CallContext, payload []byte) ([]byte, error) {
	var req camerav1.GetControlsRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			camerav1.CameraReason_CAMERA_REASON_UNSPECIFIED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	wanted := req.GetKinds()
	if len(wanted) == 0 {
		wanted = controlOrder
	}

	out := &camerav1.ControlValues{}
	err := c.withDevice(func(dev *v4l2Device) error {
		for _, kind := range wanted {
			entry, known := controlMap[kind]
			if !known {
				continue
			}
			if _, ok := dev.queryControl(entry.id); !ok {
				continue
			}
			value, err := dev.getControl(entry.id)
			if err != nil {
				// 单项读失败不该让整次查询失败：一台设备上有一个坏掉的
				// 可调项时，UI 仍然应该能显示其余的。
				c.log.Debug("camerad: 读可调项失败", "kind", kind.String(), "err", err)
				continue
			}
			out.Values = append(out.Values, &camerav1.ControlValue{
				Kind: kind, Value: int64(value),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return proto.Marshal(out)
}

// setControls 改参数。需要独占租约（camera.proto 声明 requires_control_lease）。
//
// # 逐项校验、整批失败
//
// 一项越界就整批拒绝，不做「能设的先设上」。部分生效会让调用方拿到一个失败
// 却不知道设备处在什么状态——曝光改了、增益没改，画面既不是旧的也不是新的。
//
// # 响应是【重新读回的值】而不是请求的回显
//
// 设备可能把 105 对齐到步进上的 100。回显请求值会让 UI 显示一个设备上并不
// 存在的数字，而下一次 GetControls 又会跳回去。
func (c *configEndpoint) setControls(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var req camerav1.SetControlsRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			camerav1.CameraReason_CAMERA_REASON_UNSPECIFIED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}
	// 【fail closed 复核租约】：nervud 已经裁决过（requires_control_lease），
	// 这里再看一眼 ExecutionContext 是不是真的带了 lease。它不是重新裁决，
	// 是防止「本服务被接到一个没有执行门禁的旧内核上」时静默放行。
	if cc.Execution.GetLeaseId() == 0 {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			camerav1.CameraReason_CAMERA_REASON_UNSPECIFIED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	out := &camerav1.ControlValues{}
	err := c.withDevice(func(dev *v4l2Device) error {
		// 先全部校验再全部下发，避免部分生效。
		type pending struct {
			kind  camerav1.ControlKind
			id    uint32
			value int32
		}
		batch := make([]pending, 0, len(req.GetValues()))

		for _, want := range req.GetValues() {
			entry, known := controlMap[want.GetKind()]
			if !known {
				return cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					camerav1.CameraReason_CAMERA_REASON_CONTROL_NOT_SUPPORTED, want.GetKind())
			}
			info, ok := dev.queryControl(entry.id)
			if !ok {
				return cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					camerav1.CameraReason_CAMERA_REASON_CONTROL_NOT_SUPPORTED, want.GetKind())
			}
			if info.ReadOnly {
				return cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					camerav1.CameraReason_CAMERA_REASON_CONTROL_READ_ONLY, want.GetKind())
			}
			value := want.GetValue()
			if value < int64(info.Min) || value > int64(info.Max) {
				// 【越界必须报错，不能截断】。截断之后 UI 显示的是用户设的值，
				// 设备上是另一个值，而两者永远不会自己对上。
				return cameraError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
					camerav1.CameraReason_CAMERA_REASON_CONTROL_OUT_OF_RANGE, want.GetKind())
			}
			batch = append(batch, pending{kind: want.GetKind(), id: entry.id, value: int32(value)})
		}

		for _, item := range batch {
			if err := dev.setControl(item.id, item.value); err != nil {
				c.log.Warn("camerad: 写可调项失败", "kind", item.kind.String(), "err", err)
				return cameraError(ipcv1.StatusCode_STATUS_CODE_INTERNAL,
					camerav1.CameraReason_CAMERA_REASON_DRIVER_FAULT, item.kind)
			}
		}

		// 重新读回：设备可能把值对齐到步进上。
		for _, item := range batch {
			actual, err := dev.getControl(item.id)
			if err != nil {
				continue
			}
			out.Values = append(out.Values, &camerav1.ControlValue{
				Kind: item.kind, Value: int64(actual),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	response, err := proto.Marshal(out)
	if err != nil {
		return nil, err
	}
	// 生效值变了，通知其它订阅方（可能有 UI 正开着设置页）。
	c.publishChanged(out)
	return response, nil
}

func (c *configEndpoint) publishChanged(values *camerav1.ControlValues) {
	if c.ep == nil {
		return
	}
	payload, err := proto.Marshal(values)
	if err != nil {
		return
	}
	if err := c.ep.PublishEvent(
		uint32(camerav1.CameraConfigEvent_CAMERA_CONFIG_EVENT_CONTROLS_CHANGED), payload,
	); err != nil {
		c.log.Debug("camerad: 推送 ControlsChanged 失败", "err", err)
	}
}
