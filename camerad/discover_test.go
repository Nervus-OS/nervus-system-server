//go:build linux

package main

import (
	"sort"
	"testing"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

func profileOf(t *testing.T, raw string) *boardprofile.Profile {
	t.Helper()
	p, err := boardprofile.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func fixedDevices(devices ...Device) deviceLister {
	return func() ([]Device, error) { return devices, nil }
}

func bindMap(t *testing.T, p *boardprofile.Profile, list deviceLister) map[string]string {
	t.Helper()
	bindings, err := Bind(p, list)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	out := make(map[string]string, len(bindings))
	for _, b := range bindings {
		out[b.Role] = b.Device.Node
	}
	return out
}

const poolProfile = `{
  "schema": 1, "board": "b",
  "cameras": [{"role": "cam.front", "match": {"usb_path": "1-1.1"},
               "labels": {"nervus.camera.facing": "front"}}],
  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 3}
}`

// 固定 role 按物理位置匹配，与 /dev/videoN 的编号无关。
//
// 编号由驱动加载顺序决定，用它做映射的表现是：机器重启一次，「前视」变成了
// 后视——而没有任何日志会提到这件事。
func TestBind_FixedRoleIgnoresNodeNumber(t *testing.T) {
	p := profileOf(t, poolProfile)
	// 前视摄像头这次拿到了 video7，不是 video0
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video7", USBPath: "1-1.1"},
	))
	if got["cam.front"] != "/dev/video7" {
		t.Fatalf("cam.front = %q, want /dev/video7", got["cam.front"])
	}
}

// 【池槽位按 USB 拓扑路径排序】：同一个口上的设备永远拿到同一个槽位。
func TestBind_PoolOrdersByUSBTopology(t *testing.T) {
	p := profileOf(t, poolProfile)
	// 故意让节点号顺序与拓扑顺序相反
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video0", USBPath: "1-3"},
		Device{Node: "/dev/video1", USBPath: "1-2"},
	))
	if got["cam.usb.0"] != "/dev/video1" {
		t.Errorf("cam.usb.0 = %q, want /dev/video1（1-2 在 1-3 之前）", got["cam.usb.0"])
	}
	if got["cam.usb.1"] != "/dev/video0" {
		t.Errorf("cam.usb.1 = %q, want /dev/video0", got["cam.usb.1"])
	}
}

// 【段必须按数值比较】。字典序会把 "1-10" 排在 "1-2" 前面，于是插满 10 个口的
// 机器上槽位顺序与物理顺序对不上——而这种错位只在设备数跨过 10 时才出现。
func TestLessUSBPath_ComparesSegmentsNumerically(t *testing.T) {
	paths := []string{"1-10", "1-2", "1-1.10", "1-1.2", "2-1", "1-1"}
	sort.Slice(paths, func(i, j int) bool { return lessUSBPath(paths[i], paths[j]) })

	want := []string{"1-1", "1-1.2", "1-1.10", "1-2", "1-10", "2-1"}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("排序 = %v, want %v", paths, want)
		}
	}
}

// 固定 role 已认领的设备不再进池——否则同一个摄像头会同时是 cam.front
// 和 cam.usb.0，两个 endpoint 抢同一个 /dev/videoN。
func TestBind_FixedDeviceIsNotAlsoPooled(t *testing.T) {
	p := profileOf(t, poolProfile)
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video0", USBPath: "1-1.1"}, // 固定：cam.front
		Device{Node: "/dev/video1", USBPath: "1-2"},   // 入池
	))
	if got["cam.front"] != "/dev/video0" {
		t.Fatalf("cam.front = %q", got["cam.front"])
	}
	if got["cam.usb.0"] != "/dev/video1" {
		t.Fatalf("cam.usb.0 = %q, want /dev/video1", got["cam.usb.0"])
	}
	if len(got) != 2 {
		t.Fatalf("绑定 = %v, want 恰好两条", got)
	}
}

// 【没插的 role 不产生绑定】。资源在 Catalog 里存在（构建期声明），
// endpoint 不存在（运行期没插）——App 拿到明确的 NOT_FOUND，
// 而不是一个连上去永远不出帧的 endpoint。
func TestBind_AbsentRolesProduceNoBinding(t *testing.T) {
	p := profileOf(t, poolProfile)
	got := bindMap(t, p, fixedDevices())
	if len(got) != 0 {
		t.Fatalf("绑定 = %v, want 空", got)
	}
}

// 超出 max_slots 的设备被丢弃：资源声明是构建期签名的，
// 多出来的设备没有对应资源可绑。
func TestBind_PoolOverflowIsDropped(t *testing.T) {
	p := profileOf(t, poolProfile)
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video0", USBPath: "1-2"},
		Device{Node: "/dev/video1", USBPath: "1-3"},
		Device{Node: "/dev/video2", USBPath: "1-4"},
		Device{Node: "/dev/video3", USBPath: "1-5"}, // 第四个，池只有三格
	))
	if len(got) != 3 {
		t.Fatalf("绑定 = %v, want 三条", got)
	}
	if got["cam.usb.2"] != "/dev/video2" {
		t.Errorf("cam.usb.2 = %q, want /dev/video2（最后一格给 1-4）", got["cam.usb.2"])
	}
}

// 非 USB 设备不进池：它们没有拓扑路径，排序无从谈起。
// MIPI/CSI 摄像头必须写进 cameras 用 device_path_link 定位。
func TestBind_NonUSBDevicesStayOutOfPool(t *testing.T) {
	p := profileOf(t, poolProfile)
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video0", PathLink: "platform-csi0-video-index0"},
	))
	if len(got) != 0 {
		t.Fatalf("绑定 = %v, want 空（CSI 设备不该进池）", got)
	}
}

func TestBind_DeviceLinkMatchesFixedRole(t *testing.T) {
	p := profileOf(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.depth",
	               "match": {"device_path_link": "platform-csi0-video-index0"},
	               "labels": {"nervus.camera.kind": "depth"}}]
	}`)
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video4", PathLink: "platform-csi0-video-index0"},
	))
	if got["cam.depth"] != "/dev/video4" {
		t.Fatalf("cam.depth = %q, want /dev/video4", got["cam.depth"])
	}
}

// 一个 role 匹配到两个设备说明板级配置的位置描述不够精确。
//
// 【必须报错而不是随便挑一个】：挑错的后果是画面朝向不对，而这在集成阶段
// 极难归因——两个设备都在同一个 USB 路径下，看起来配置完全正确。
func TestBind_AmbiguousFixedMatchIsAnError(t *testing.T) {
	p := profileOf(t, poolProfile)
	_, err := Bind(p, fixedDevices(
		Device{Node: "/dev/video0", USBPath: "1-1.1"},
		Device{Node: "/dev/video1", USBPath: "1-1.1"},
	))
	if err == nil {
		t.Fatal("同一 role 匹配到两个设备却没报错")
	}
}

// 上游口排在它下面 hub 口之前："1-1" 在 "1-1.2" 之前。
func TestLessUSBPath_ShorterPrefixComesFirst(t *testing.T) {
	if !lessUSBPath("1-1", "1-1.2") {
		t.Error("1-1 应当排在 1-1.2 之前")
	}
	if lessUSBPath("1-1.2", "1-1") {
		t.Error("比较不是反对称的")
	}
}

// 解析不出数字的段记为 -1 而不是跳过：跳过会让 "1-a.2" 与 "1-2" 错位比较，
// 把两条毫无关系的路径判成相等。
func TestUSBSegments_KeepsUnparsableSegments(t *testing.T) {
	if got := usbSegments("1-a.2"); len(got) != 3 || got[1] != -1 {
		t.Fatalf("segments = %v, want 三段且中间为 -1", got)
	}
}
