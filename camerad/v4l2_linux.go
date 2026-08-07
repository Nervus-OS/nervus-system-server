// 本文件是 V4L2 兼容层：把 Linux 的 videodev2 ioctl 包成 camerad 用得上的形状。
//
// # 为什么直接写 ioctl 而不是绑一个 C 库
//
// libv4l2 会做格式转换（把 MJPEG 解成 RGB），那正是本服务【不能】做的事：
// 帧要原样进共享内存环，由消费方决定怎么解。中间插一次转换意味着 CPU 上多一次
// 全帧拷贝，而机器人上跑的多半是拿去喂神经网络的原始帧。
//
// 另外绑 C 库要开 cgo，交叉编译到 arm64 就得配一套 sysroot。ioctl 的结构体布局
// 是稳定 ABI，写死比拉一条工具链依赖便宜。
//
// # 结构体布局与 ioctl 号
//
// 【号是算出来的，不是抄的】。_IOC 把方向、类型、序号与结构体大小编进一个 32 位
// 数，其中大小来自 unsafe.Sizeof。抄一个十六进制常量的话，结构体定义写错了也
// 看不出来——ioctl 会返回 ENOTTY，而那个错误码指向「驱动不支持」，不指向
// 「你的结构体少了 4 字节」。
package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ---- ioctl 编码 -------------------------------------------------------------

const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2

	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<iocDirShift | size<<iocSizeShift | typ<<iocTypeShift | nr<<iocNRShift
}

const v4l2IOCType = 'V'

func iorV(nr, size uintptr) uintptr  { return ioc(iocRead, v4l2IOCType, nr, size) }
func iowV(nr, size uintptr) uintptr  { return ioc(iocWrite, v4l2IOCType, nr, size) }
func iowrV(nr, size uintptr) uintptr { return ioc(iocRead|iocWrite, v4l2IOCType, nr, size) }

// ---- videodev2 结构体 --------------------------------------------------------
//
// 字段顺序与 include/uapi/linux/videodev2.h 逐字对应。填充字段显式写出来而不是
// 靠编译器插——Go 不保证结构体布局，显式写出来才谈得上「与 C 一致」。

type v4l2Capability struct {
	driver       [16]byte
	card         [32]byte
	busInfo      [32]byte
	version      uint32
	capabilities uint32
	deviceCaps   uint32
	reserved     [3]uint32
}

const (
	v4l2CapVideoCapture = 0x00000001
	v4l2CapStreaming    = 0x04000000
	v4l2CapDeviceCaps   = 0x80000000
)

type v4l2Fmtdesc struct {
	index       uint32
	typ         uint32
	flags       uint32
	description [32]byte
	pixelformat uint32
	reserved    [4]uint32
}

type v4l2Frmsizeenum struct {
	index       uint32
	pixelFormat uint32
	typ         uint32
	// discrete 与 stepwise 的联合体，24 字节。只读 discrete 的前两个 u32。
	union    [24]byte
	reserved [2]uint32
}

const v4l2FrmsizeTypeDiscrete = 1

type v4l2Frmivalenum struct {
	index       uint32
	pixelFormat uint32
	width       uint32
	height      uint32
	typ         uint32
	union       [24]byte
	reserved    [2]uint32
}

const v4l2FrmivalTypeDiscrete = 1

// v4l2Format 的 union 含指针成员（v4l2_window.clips），因此按 8 字节对齐——
// type 之后必须有 4 字节填充，否则整个结构体小 4 字节，ioctl 号跟着算错。
type v4l2Format struct {
	typ  uint32
	_    uint32
	data [200]byte
}

type v4l2PixFormat struct {
	width        uint32
	height       uint32
	pixelformat  uint32
	field        uint32
	bytesperline uint32
	sizeimage    uint32
	colorspace   uint32
	priv         uint32
	flags        uint32
	encoding     uint32
	quantization uint32
	xferFunc     uint32
}

const (
	v4l2BufTypeVideoCapture = 1
	v4l2FieldNone           = 1
	v4l2MemoryMMAP          = 1
)

type v4l2RequestBuffers struct {
	count        uint32
	typ          uint32
	memory       uint32
	capabilities uint32
	flags        uint8
	reserved     [3]uint8
}

type v4l2Timeval struct {
	sec  int64
	usec int64
}

type v4l2Buffer struct {
	index     uint32
	typ       uint32
	bytesused uint32
	flags     uint32
	field     uint32
	_         uint32 // timestamp 需 8 字节对齐
	timestamp v4l2Timeval
	timecode  [16]byte
	sequence  uint32
	memory    uint32
	// m 联合体：MMAP 模式下前 4 字节是 offset。64 位上联合体占 8 字节。
	m         uint64
	length    uint32
	reserved2 uint32
	requestFD int32
	_         uint32
}

type v4l2CaptureParm struct {
	capability   uint32
	capturemode  uint32
	numerator    uint32
	denominator  uint32
	extendedmode uint32
	readbuffers  uint32
	reserved     [4]uint32
}

type v4l2StreamParm struct {
	typ  uint32
	data [200]byte
}

const v4l2CapTimePerFrame = 0x1000

type v4l2Control struct {
	id    uint32
	value int32
}

type v4l2QueryCtrl struct {
	id           uint32
	typ          uint32
	name         [32]byte
	minimum      int32
	maximum      int32
	step         int32
	defaultValue int32
	flags        uint32
	reserved     [2]uint32
}

const (
	v4l2CtrlFlagDisabled = 0x0001
	v4l2CtrlFlagReadOnly = 0x0004
	v4l2CtrlFlagGrabbed  = 0x0002
)

// ioctl 号。全部由结构体大小算出，见文件头。
var (
	vidiocQueryCap           = iorV(0, unsafe.Sizeof(v4l2Capability{}))
	vidiocEnumFmt            = iowrV(2, unsafe.Sizeof(v4l2Fmtdesc{}))
	vidiocGFmt               = iowrV(4, unsafe.Sizeof(v4l2Format{}))
	vidiocSFmt               = iowrV(5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqbufs            = iowrV(8, unsafe.Sizeof(v4l2RequestBuffers{}))
	vidiocQuerybuf           = iowrV(9, unsafe.Sizeof(v4l2Buffer{}))
	vidiocQbuf               = iowrV(15, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDqbuf              = iowrV(17, unsafe.Sizeof(v4l2Buffer{}))
	vidiocStreamon           = iowV(18, unsafe.Sizeof(int32(0)))
	vidiocStreamoff          = iowV(19, unsafe.Sizeof(int32(0)))
	vidiocSParm              = iowrV(22, unsafe.Sizeof(v4l2StreamParm{}))
	vidiocGCtrl              = iowrV(27, unsafe.Sizeof(v4l2Control{}))
	vidiocSCtrl              = iowrV(28, unsafe.Sizeof(v4l2Control{}))
	vidiocQueryctrl          = iowrV(36, unsafe.Sizeof(v4l2QueryCtrl{}))
	vidiocEnumFramesizes     = iowrV(74, unsafe.Sizeof(v4l2Frmsizeenum{}))
	vidiocEnumFrameintervals = iowrV(75, unsafe.Sizeof(v4l2Frmivalenum{}))
)

// ioctlPtr 发一次 ioctl。
//
// 【必须重试 EINTR】：Go runtime 会给线程发抢占信号，一次被打断的 ioctl 返回
// EINTR。不重试的表现是随机的、无法复现的采集失败——尤其在 GC 压力大的时候。
func ioctlPtr(fd int, request uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
		if errno == 0 {
			return nil
		}
		if errno == unix.EINTR {
			continue
		}
		return errno
	}
}

// ---- 设备 -------------------------------------------------------------------

// v4l2Device 是一个打开的采集设备。非并发安全，由 stream 串行使用。
type v4l2Device struct {
	fd   int
	node string

	// buffers 是 mmap 出来的采集缓冲。STREAMOFF 之后必须逐个 munmap。
	buffers [][]byte
	// streaming 记录 STREAMON 是否已发出，避免重复 STREAMOFF 时报 EINVAL。
	streaming bool
}

func openDevice(node string) (*v4l2Device, error) {
	// O_NONBLOCK：DQBUF 在没帧时立即返回 EAGAIN，由上层用 poll 等。
	// 阻塞模式下一个掉线的摄像头会让 goroutine 永久卡在 DQBUF 里，
	// 连 CloseStream 都推不动它。
	fd, err := unix.Open(node, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("camerad: 打开 %s: %w", node, err)
	}
	return &v4l2Device{fd: fd, node: node}, nil
}

func (d *v4l2Device) Close() error {
	if d == nil || d.fd < 0 {
		return nil
	}
	_ = d.stopStreaming()
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

// capabilities 返回设备的有效能力位。
//
// 【优先用 device_caps 而不是 capabilities】：后者是整个物理设备的能力并集，
// 一个同时注册了采集节点与 metadata 节点的 UVC 摄像头，两个节点的
// capabilities 完全相同。只有 device_caps 说得清「这一个节点能干什么」。
func (d *v4l2Device) capabilities() (uint32, error) {
	var cap v4l2Capability
	if err := ioctlPtr(d.fd, vidiocQueryCap, unsafe.Pointer(&cap)); err != nil {
		return 0, fmt.Errorf("camerad: QUERYCAP %s: %w", d.node, err)
	}
	if cap.capabilities&v4l2CapDeviceCaps != 0 {
		return cap.deviceCaps, nil
	}
	return cap.capabilities, nil
}

// canCapture 报告一个节点是否真的能取流。
//
// UVC 设备常常注册两个节点：一个采集、一个 metadata。不过滤的话，一半设备会
// 打开成功却永远出不了帧——而症状是「摄像头坏了」，没人会想到是节点选错了。
func canCapture(node string) bool {
	dev, err := openDevice(node)
	if err != nil {
		return false
	}
	defer func() { _ = dev.Close() }()

	caps, err := dev.capabilities()
	if err != nil {
		return false
	}
	return caps&v4l2CapVideoCapture != 0 && caps&v4l2CapStreaming != 0
}

// ---- 格式枚举 ---------------------------------------------------------------

// formatOption 是一种可用的取流配置。
type formatOption struct {
	PixelFormat uint32
	Width       uint32
	Height      uint32
	FrameRates  []uint32
	MaxBytes    uint32
}

// enumerateFormats 列出设备支持的全部离散配置。
//
// 【只收离散尺寸，不收 stepwise】。stepwise 设备（少数 webcam、多数 CSI 桥）
// 允许任意分辨率，展开成列表会产生成千上万条。协商任意尺寸需要一套「请求 →
// 驱动对齐 → 回报实际值」的往返，而本接口刻意不做尺寸协商（见 camera.proto 的
// OpenStreamRequest：不匹配就失败，不静默降级）。
//
// 代价是 stepwise 设备只能通过 nervus.camerad 的私有配置支持。这是已知取舍，
// 不是遗漏。
func (d *v4l2Device) enumerateFormats() ([]formatOption, error) {
	var options []formatOption

	for index := uint32(0); ; index++ {
		desc := v4l2Fmtdesc{index: index, typ: v4l2BufTypeVideoCapture}
		if err := ioctlPtr(d.fd, vidiocEnumFmt, unsafe.Pointer(&desc)); err != nil {
			if errors.Is(err, unix.EINVAL) {
				break // 枚举到头，这是正常终止条件而不是错误
			}
			return nil, fmt.Errorf("camerad: ENUM_FMT %s: %w", d.node, err)
		}
		sizes, err := d.enumerateSizes(desc.pixelformat)
		if err != nil {
			return nil, err
		}
		options = append(options, sizes...)
	}

	sort.Slice(options, func(i, j int) bool {
		if options[i].PixelFormat != options[j].PixelFormat {
			return options[i].PixelFormat < options[j].PixelFormat
		}
		if options[i].Width != options[j].Width {
			return options[i].Width < options[j].Width
		}
		return options[i].Height < options[j].Height
	})
	return options, nil
}

func (d *v4l2Device) enumerateSizes(pixelFormat uint32) ([]formatOption, error) {
	var options []formatOption
	for index := uint32(0); ; index++ {
		enum := v4l2Frmsizeenum{index: index, pixelFormat: pixelFormat}
		if err := ioctlPtr(d.fd, vidiocEnumFramesizes, unsafe.Pointer(&enum)); err != nil {
			if errors.Is(err, unix.EINVAL) {
				break
			}
			return nil, fmt.Errorf("camerad: ENUM_FRAMESIZES %s: %w", d.node, err)
		}
		if enum.typ != v4l2FrmsizeTypeDiscrete {
			break // stepwise/continuous：见 enumerateFormats 的说明
		}
		width := *(*uint32)(unsafe.Pointer(&enum.union[0]))
		height := *(*uint32)(unsafe.Pointer(&enum.union[4]))

		rates, err := d.enumerateRates(pixelFormat, width, height)
		if err != nil {
			return nil, err
		}
		options = append(options, formatOption{
			PixelFormat: pixelFormat,
			Width:       width,
			Height:      height,
			FrameRates:  rates,
			MaxBytes:    maxFrameBytes(pixelFormat, width, height),
		})
	}
	return options, nil
}

func (d *v4l2Device) enumerateRates(pixelFormat, width, height uint32) ([]uint32, error) {
	var rates []uint32
	for index := uint32(0); ; index++ {
		enum := v4l2Frmivalenum{
			index: index, pixelFormat: pixelFormat, width: width, height: height,
		}
		if err := ioctlPtr(d.fd, vidiocEnumFrameintervals, unsafe.Pointer(&enum)); err != nil {
			if errors.Is(err, unix.EINVAL) {
				break
			}
			return nil, fmt.Errorf("camerad: ENUM_FRAMEINTERVALS %s: %w", d.node, err)
		}
		if enum.typ != v4l2FrmivalTypeDiscrete {
			break
		}
		// 间隔是 numerator/denominator 秒，帧率是它的倒数。
		numerator := *(*uint32)(unsafe.Pointer(&enum.union[0]))
		denominator := *(*uint32)(unsafe.Pointer(&enum.union[4]))
		if numerator == 0 {
			continue
		}
		rates = append(rates, denominator/numerator)
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i] < rates[j] })
	return rates, nil
}

// maxFrameBytes 给出一帧的上限字节数，用于定共享内存槽大小。
//
// 压缩格式（MJPEG）没有确定的帧长，取未压缩尺寸的一半作为上限——JPEG 在这个
// 分辨率下压不过 2:1 的情况几乎不存在，而【宁可多留也不能不够】：槽装不下的
// 帧只能丢，表现是画面随机卡顿。
func maxFrameBytes(pixelFormat, width, height uint32) uint32 {
	pixels := width * height
	switch pixelFormat {
	case v4l2PixFmtNV12:
		return pixels * 3 / 2
	case v4l2PixFmtYUYV:
		return pixels * 2
	case v4l2PixFmtRGB24:
		return pixels * 3
	case v4l2PixFmtZ16:
		return pixels * 2
	case v4l2PixFmtMJPEG:
		return pixels * 3 / 2
	default:
		return pixels * 4
	}
}

// V4L2 fourcc。用 v4l2_fourcc 宏的算法拼出来，而不是抄十六进制。
const (
	v4l2PixFmtNV12  = 'N' | 'V'<<8 | '1'<<16 | '2'<<24
	v4l2PixFmtYUYV  = 'Y' | 'U'<<8 | 'Y'<<16 | 'V'<<24
	v4l2PixFmtMJPEG = 'M' | 'J'<<8 | 'P'<<16 | 'G'<<24
	v4l2PixFmtRGB24 = 'R' | 'G'<<8 | 'B'<<16 | '3'<<24
	v4l2PixFmtZ16   = 'Z' | '1'<<8 | '6'<<16 | ' '<<24
)

// ---- 取流 -------------------------------------------------------------------

// setFormat 设定取流格式，返回驱动实际接受的值。
//
// 【必须回读】：S_FMT 是「协商」而不是「设置」——驱动会把不支持的请求改成最近的
// 支持值并写回同一个结构体，而且【不报错】。不回读就会按请求的尺寸去解析一段
// 按别的尺寸排布的内存。
func (d *v4l2Device) setFormat(pixelFormat, width, height uint32) (v4l2PixFormat, error) {
	format := v4l2Format{typ: v4l2BufTypeVideoCapture}
	pix := v4l2PixFormat{
		width:       width,
		height:      height,
		pixelformat: pixelFormat,
		field:       v4l2FieldNone,
	}
	*(*v4l2PixFormat)(unsafe.Pointer(&format.data[0])) = pix

	if err := ioctlPtr(d.fd, vidiocSFmt, unsafe.Pointer(&format)); err != nil {
		return v4l2PixFormat{}, fmt.Errorf("camerad: S_FMT %s: %w", d.node, err)
	}
	return *(*v4l2PixFormat)(unsafe.Pointer(&format.data[0])), nil
}

// setFrameRate 设定帧率。驱动不支持 S_PARM 时静默跳过。
//
// 【不支持不是错误】：许多 CSI 桥固定帧率、不接受设置。为此拒绝开流会让一批
// 设备完全不可用，而它们本来能正常出帧。实际帧率由调用方从 DescribeStream
// 得知。
func (d *v4l2Device) setFrameRate(fps uint32) {
	if fps == 0 {
		return
	}
	parm := v4l2StreamParm{typ: v4l2BufTypeVideoCapture}
	capture := v4l2CaptureParm{numerator: 1, denominator: fps}
	*(*v4l2CaptureParm)(unsafe.Pointer(&parm.data[0])) = capture
	_ = ioctlPtr(d.fd, vidiocSParm, unsafe.Pointer(&parm))
}

// startStreaming 申请缓冲、mmap、入队并开流。
func (d *v4l2Device) startStreaming(bufferCount uint32) error {
	req := v4l2RequestBuffers{
		count:  bufferCount,
		typ:    v4l2BufTypeVideoCapture,
		memory: v4l2MemoryMMAP,
	}
	if err := ioctlPtr(d.fd, vidiocReqbufs, unsafe.Pointer(&req)); err != nil {
		return fmt.Errorf("camerad: REQBUFS %s: %w", d.node, err)
	}
	if req.count < 2 {
		// 单缓冲意味着驱动写入的同时我们在读同一块内存。
		return fmt.Errorf("camerad: %s 只给了 %d 个缓冲，至少需要 2 个", d.node, req.count)
	}

	for i := uint32(0); i < req.count; i++ {
		buf := v4l2Buffer{index: i, typ: v4l2BufTypeVideoCapture, memory: v4l2MemoryMMAP}
		if err := ioctlPtr(d.fd, vidiocQuerybuf, unsafe.Pointer(&buf)); err != nil {
			d.unmapAll()
			return fmt.Errorf("camerad: QUERYBUF %s[%d]: %w", d.node, i, err)
		}
		// MMAP 模式下 m 联合体的前 4 字节是 offset。
		offset := int64(uint32(buf.m))
		mem, err := unix.Mmap(d.fd, offset, int(buf.length),
			unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			d.unmapAll()
			return fmt.Errorf("camerad: mmap %s[%d]: %w", d.node, i, err)
		}
		d.buffers = append(d.buffers, mem)

		if err := ioctlPtr(d.fd, vidiocQbuf, unsafe.Pointer(&buf)); err != nil {
			d.unmapAll()
			return fmt.Errorf("camerad: QBUF %s[%d]: %w", d.node, i, err)
		}
	}

	typ := int32(v4l2BufTypeVideoCapture)
	if err := ioctlPtr(d.fd, vidiocStreamon, unsafe.Pointer(&typ)); err != nil {
		d.unmapAll()
		return fmt.Errorf("camerad: STREAMON %s: %w", d.node, err)
	}
	d.streaming = true
	return nil
}

func (d *v4l2Device) stopStreaming() error {
	if !d.streaming {
		d.unmapAll()
		return nil
	}
	typ := int32(v4l2BufTypeVideoCapture)
	err := ioctlPtr(d.fd, vidiocStreamoff, unsafe.Pointer(&typ))
	d.streaming = false
	// 【无论 STREAMOFF 成不成功都要 munmap】：失败时那些映射依然存在，
	// 泄漏它们会让反复开关流的进程慢慢耗尽地址空间。
	d.unmapAll()
	if err != nil {
		return fmt.Errorf("camerad: STREAMOFF %s: %w", d.node, err)
	}
	return nil
}

func (d *v4l2Device) unmapAll() {
	for _, mem := range d.buffers {
		_ = unix.Munmap(mem)
	}
	d.buffers = nil
}

// capturedFrame 是一帧尚未归还给驱动的采集数据。
//
// Data 指向 mmap 的缓冲，【在 release 之前有效】。拷贝到别处之前不要放手——
// release 之后驱动随时会往同一块内存写下一帧。
type capturedFrame struct {
	Data []byte
	// MonotonicNanos 是驱动打的采集时刻，CLOCK_MONOTONIC 域。
	//
	// 这是唯一有意义的时间戳：它是【传感器出帧的那一刻】，不含后面任何一段
	// 排队延迟。camerad 把它原样透传进共享内存环的帧头。
	MonotonicNanos uint64
	Sequence       uint32
	index          uint32
}

// errNoFrame 表示此刻没有可取的帧，不是错误状态。
var errNoFrame = errors.New("camerad: no frame ready")

// dequeue 取一帧。没有帧时返回 errNoFrame。
func (d *v4l2Device) dequeue() (capturedFrame, error) {
	buf := v4l2Buffer{typ: v4l2BufTypeVideoCapture, memory: v4l2MemoryMMAP}
	if err := ioctlPtr(d.fd, vidiocDqbuf, unsafe.Pointer(&buf)); err != nil {
		if errors.Is(err, unix.EAGAIN) {
			return capturedFrame{}, errNoFrame
		}
		return capturedFrame{}, fmt.Errorf("camerad: DQBUF %s: %w", d.node, err)
	}
	if int(buf.index) >= len(d.buffers) {
		// 驱动给了一个我们没映射过的下标。继续用它等于越界读。
		return capturedFrame{}, fmt.Errorf(
			"camerad: DQBUF %s 返回越界下标 %d（已映射 %d 个）",
			d.node, buf.index, len(d.buffers))
	}
	mem := d.buffers[buf.index]
	if int(buf.bytesused) > len(mem) {
		return capturedFrame{}, fmt.Errorf(
			"camerad: DQBUF %s 声称 %d 字节，缓冲只有 %d",
			d.node, buf.bytesused, len(mem))
	}
	return capturedFrame{
		Data:           mem[:buf.bytesused],
		MonotonicNanos: uint64(buf.timestamp.sec)*1e9 + uint64(buf.timestamp.usec)*1e3,
		Sequence:       buf.sequence,
		index:          buf.index,
	}, nil
}

// release 把缓冲还给驱动。【必须调用】，否则缓冲池耗尽后再也取不到帧。
func (d *v4l2Device) release(frame capturedFrame) error {
	buf := v4l2Buffer{
		index:  frame.index,
		typ:    v4l2BufTypeVideoCapture,
		memory: v4l2MemoryMMAP,
	}
	if err := ioctlPtr(d.fd, vidiocQbuf, unsafe.Pointer(&buf)); err != nil {
		return fmt.Errorf("camerad: QBUF %s[%d]: %w", d.node, frame.index, err)
	}
	return nil
}

// waitFrame 等到有帧可取或超时。
func (d *v4l2Device) waitFrame(timeoutMillis int) error {
	fds := []unix.PollFd{{Fd: int32(d.fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, timeoutMillis)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("camerad: poll %s: %w", d.node, err)
		}
		if n == 0 {
			return os.ErrDeadlineExceeded
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			// 设备掉线。继续 DQBUF 只会拿到无意义的错误码。
			return fmt.Errorf("camerad: %s 设备已断开", d.node)
		}
		return nil
	}
}

// ---- 可调项 -----------------------------------------------------------------

type controlInfo struct {
	ID       uint32
	Min      int32
	Max      int32
	Step     int32
	Default  int32
	ReadOnly bool
}

// queryControl 读一个可调项的范围。不支持时返回 ok=false。
func (d *v4l2Device) queryControl(id uint32) (controlInfo, bool) {
	query := v4l2QueryCtrl{id: id}
	if err := ioctlPtr(d.fd, vidiocQueryctrl, unsafe.Pointer(&query)); err != nil {
		return controlInfo{}, false
	}
	if query.flags&v4l2CtrlFlagDisabled != 0 {
		return controlInfo{}, false
	}
	return controlInfo{
		ID:      id,
		Min:     query.minimum,
		Max:     query.maximum,
		Step:    query.step,
		Default: query.defaultValue,
		// GRABBED 也算只读：那意味着此刻正在流中，改不了。
		ReadOnly: query.flags&(v4l2CtrlFlagReadOnly|v4l2CtrlFlagGrabbed) != 0,
	}, true
}

func (d *v4l2Device) getControl(id uint32) (int32, error) {
	ctrl := v4l2Control{id: id}
	if err := ioctlPtr(d.fd, vidiocGCtrl, unsafe.Pointer(&ctrl)); err != nil {
		return 0, fmt.Errorf("camerad: G_CTRL %s id=%#x: %w", d.node, id, err)
	}
	return ctrl.value, nil
}

func (d *v4l2Device) setControl(id uint32, value int32) error {
	ctrl := v4l2Control{id: id, value: value}
	if err := ioctlPtr(d.fd, vidiocSCtrl, unsafe.Pointer(&ctrl)); err != nil {
		return fmt.Errorf("camerad: S_CTRL %s id=%#x: %w", d.node, id, err)
	}
	return nil
}
