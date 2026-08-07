//go:build linux

// 本文件把「板级配置里的 role」映射到「板上真实存在的 V4L2 设备」。
//
// 两条路径：
//
//	固定摄像头  按 usb_path / device_path_link 精确匹配，位置不变则 role 不变
//	USB 冷插拔池 按 USB 拓扑路径排序后依次占槽
//
// # 为什么不用 /dev/videoN
//
// videoN 的编号由驱动加载顺序决定。同一台机器换个内核版本、多插一个设备、
// 甚至只是启动时序抖动，编号就能变。用它做 role 映射的表现是：机器重启一次，
// 「前视」变成了后视——而没有任何日志会提到这件事。
//
// USB 拓扑路径（"1-1.2"）描述的是【插在哪个物理口上】。只要不重新布线就不会变。
//
// # 一个物理摄像头为什么可能有多个 videoN
//
// UVC 设备常常注册两个节点：一个采集、一个 metadata。只有带
// V4L2_CAP_VIDEO_CAPTURE 的那个能取流，另一个必须跳过——否则会有一半设备
// 打开成功却永远出不了帧。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

// sysV4L2Root 是 V4L2 设备在 sysfs 里的入口。
const sysV4L2Root = "/sys/class/video4linux"

// Device 是一个已发现的采集设备。
type Device struct {
	// Node 是 /dev/videoN。只在本次启动内有意义。
	Node string
	// USBPath 是 USB 拓扑路径，如 "1-1.2"。非 USB 设备为空。
	USBPath string
	// PathLink 是 /dev/v4l/by-path/ 下的链接名。没有则为空。
	PathLink string
	// Name 是驱动上报的设备名，仅供日志。
	Name string
}

// Binding 是一个 role 与设备的绑定。
type Binding struct {
	Role   string
	Device Device
}

// deviceLister 让发现逻辑可测：真实实现读 sysfs，测试给一份固定清单。
//
// 抽出来不是为了「解耦」，是因为槽位分配规则的正确性【只能靠构造设备清单来验】，
// 而在 CI 上插四个摄像头是不现实的。
type deviceLister func() ([]Device, error)

// Bind 把板级配置映射到实际设备。
//
// 返回的绑定【只包含真实存在的设备】。板级配置里声明了但没插的 role 不出现——
// 那正是「只为实际存在的设备 RegisterEndpoint」的含义：资源在 Catalog 里存在
// （构建期声明），endpoint 不存在（运行期没插）。App 会拿到一个明确的
// NOT_FOUND，而不是一个连上去永远不出帧的 endpoint。
func Bind(profile *boardprofile.Profile, list deviceLister) ([]Binding, error) {
	devices, err := list()
	if err != nil {
		return nil, err
	}

	byUSB := profile.FixedByUSBPath()
	byLink := profile.FixedByDeviceLink()

	var bindings []Binding
	claimed := make(map[string]bool, len(devices)) // node -> 已被固定 role 占用
	seenRole := make(map[string]string)            // role -> node，查重复匹配

	for _, dev := range devices {
		role, ok := matchFixed(dev, byUSB, byLink)
		if !ok {
			continue
		}
		// 同一个 role 匹配到两个节点：UVC 的采集节点与 metadata 节点都挂在同一个
		// USB 路径下。列表已按节点号排序，取第一个能采集的即可——而不能采集的
		// 节点根本不会出现在 devices 里（见 lister 的过滤）。
		if prev, dup := seenRole[role]; dup {
			return nil, fmt.Errorf(
				"camerad: role %q 同时匹配到 %s 与 %s，板级配置的位置描述不够精确",
				role, prev, dev.Node)
		}
		seenRole[role] = dev.Node
		claimed[dev.Node] = true
		bindings = append(bindings, Binding{Role: role, Device: dev})
	}

	bindings = append(bindings, assignPool(profile.USBPool, devices, claimed)...)
	return bindings, nil
}

func matchFixed(dev Device, byUSB, byLink map[string]string) (string, bool) {
	if dev.USBPath != "" {
		if role, ok := byUSB[dev.USBPath]; ok {
			return role, true
		}
	}
	if dev.PathLink != "" {
		if role, ok := byLink[dev.PathLink]; ok {
			return role, true
		}
	}
	return "", false
}

// assignPool 把没被固定 role 认领的 USB 设备按拓扑顺序填进池槽位。
//
// # 排序即稳定性
//
// 按 USB 拓扑路径排序，意味着【同一个口上的设备永远拿到同一个槽位】——只要
// 插着的设备集合不变。这就是「冷插拔」的全部含义：开机前插好，开机后不变。
//
// 【但它对集合变化不稳定】：拔掉 1-1.2 上的摄像头，原本在 1-1.3 的会从
// usb.1 挪到 usb.0。这不是缺陷，是 USB 池这个机制的固有性质——「第几个」
// 本来就是相对的。需要绝对稳定的设备必须写进 cameras 用 usb_path 精确匹配。
//
// 超出 max_slots 的设备被丢弃。资源声明是构建期签名的，运行期多出来的设备
// 没有对应资源可绑；静默丢弃并记日志，好过绑到一个签名覆盖不到的 role 上。
func assignPool(pool *boardprofile.USBPool, devices []Device, claimed map[string]bool) []Binding {
	if pool == nil {
		return nil
	}

	var pending []Device
	for _, dev := range devices {
		if claimed[dev.Node] || dev.USBPath == "" {
			continue
		}
		pending = append(pending, dev)
	}
	sort.Slice(pending, func(i, j int) bool {
		return lessUSBPath(pending[i].USBPath, pending[j].USBPath)
	})

	roles := pool.Roles()
	if len(pending) > len(roles) {
		pending = pending[:len(roles)]
	}
	bindings := make([]Binding, 0, len(pending))
	for i, dev := range pending {
		bindings = append(bindings, Binding{Role: roles[i], Device: dev})
	}
	return bindings
}

// lessUSBPath 比较两条 USB 拓扑路径。
//
// 【必须按段做数值比较】。字典序会把 "1-10" 排在 "1-2" 前面，于是插满 10 个
// 口的机器上，槽位顺序与物理顺序对不上——而这种错位只在设备数跨过 10 时才
// 出现，测试环境几乎不可能碰到。
func lessUSBPath(a, b string) bool {
	sa, sb := usbSegments(a), usbSegments(b)
	for i := 0; i < len(sa) && i < len(sb); i++ {
		if sa[i] != sb[i] {
			return sa[i] < sb[i]
		}
	}
	if len(sa) != len(sb) {
		// 前缀相同时短的在前："1-1" 在 "1-1.2" 之前，即上游口先于它下面的 hub 口。
		return len(sa) < len(sb)
	}
	// 段完全相同（同一个口上的两个接口），退回字符串比较保证全序。
	return a < b
}

// usbSegments 把 "1-1.2" 拆成 [1, 1, 2]。
//
// 无法解析成数字的段记为 -1 而不是跳过：跳过会让 "1-a.2" 与 "1-2" 比较时
// 错位，把两条毫无关系的路径判成相等。
func usbSegments(path string) []int {
	fields := strings.FieldsFunc(path, func(r rune) bool {
		return r == '-' || r == '.' || r == ':'
	})
	out := make([]int, 0, len(fields))
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil {
			n = -1
		}
		out = append(out, n)
	}
	return out
}

// listSysfsDevices 枚举 sysfs 里能采集的 V4L2 设备。
func listSysfsDevices() ([]Device, error) {
	entries, err := os.ReadDir(sysV4L2Root)
	if err != nil {
		if os.IsNotExist(err) {
			// 没有 video4linux 子系统 = 这台机器上没有摄像头驱动。
			// 【不是错误】：一台不带摄像头的机器上 camerad 应当正常启动，
			// 只是没有任何 endpoint。回错误会让它进入重启循环。
			return nil, nil
		}
		return nil, fmt.Errorf("camerad: 读 %s: %w", sysV4L2Root, err)
	}

	links := deviceLinkIndex()

	var devices []Device
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "video") {
			continue
		}
		node := filepath.Join("/dev", name)
		if !canCapture(node) {
			// UVC 的 metadata 节点走这条。放进来的话，一半设备会打开成功
			// 却永远出不了帧。
			continue
		}
		devices = append(devices, Device{
			Node:     node,
			USBPath:  usbPathOf(filepath.Join(sysV4L2Root, name)),
			PathLink: links[node],
			Name:     readSysAttr(filepath.Join(sysV4L2Root, name, "name")),
		})
	}

	// 按节点号排序，让「同一个 role 匹配到多个节点」的报错稳定指向同一对设备。
	sort.Slice(devices, func(i, j int) bool { return devices[i].Node < devices[j].Node })
	return devices, nil
}

// usbPathOf 从 sysfs 条目反查 USB 拓扑路径。
//
// /sys/class/video4linux/videoN/device 指向 USB interface（如 "1-1.2:1.0"），
// 取冒号之前的部分就是设备的拓扑路径。非 USB 设备（MIPI/CSI）解析不出来，
// 返回空字符串——它们靠 device_path_link 定位。
func usbPathOf(sysEntry string) string {
	target, err := filepath.EvalSymlinks(filepath.Join(sysEntry, "device"))
	if err != nil {
		return ""
	}
	base := filepath.Base(target)
	if idx := strings.IndexByte(base, ':'); idx >= 0 {
		base = base[:idx]
	}
	// USB 拓扑路径形如 "<bus>-<port>[.<port>...]"。没有连字符说明这不是 USB。
	if !strings.ContainsRune(base, '-') {
		return ""
	}
	return base
}

// deviceLinkIndex 建立 /dev/videoN → by-path 链接名的反向索引。
func deviceLinkIndex() map[string]string {
	const byPath = "/dev/v4l/by-path"
	entries, err := os.ReadDir(byPath)
	if err != nil {
		return nil
	}
	index := make(map[string]string, len(entries))
	for _, entry := range entries {
		target, err := filepath.EvalSymlinks(filepath.Join(byPath, entry.Name()))
		if err != nil {
			continue
		}
		// 一个节点可能有多个链接。取字典序最小的一个，保证映射确定。
		if prev, ok := index[target]; !ok || entry.Name() < prev {
			index[target] = entry.Name()
		}
	}
	return index
}

func readSysAttr(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
