// Package boardprofile 解析板级摄像头配置。
//
// # 它被读两次，这是刻意的
//
//	构建期  providergen 读它 → 生成 ProviderArtifacts 里的 nervus.resource.camera 声明
//	运行期  camerad 读它     → 把 role 映射到真实设备节点
//
// 【同一份文件、同一个解析器】。分成两份配置的话，「Catalog 里有 cam.front」
// 和「运行时 cam.front 指向哪个设备」会变成两个可以各自漂移的事实，而漂移之后
// 两边都不报错——只是 App 请求前视摄像头时拿到一个不存在的 endpoint。
//
// # 为什么资源声明必须在构建期定死
//
// ProviderArtifacts 进 manifest 的 digests，随镜像签名。如果资源集合是运行期
// 从 JSON 现读的，那么改一行 JSON 就能给自己加一路「前视摄像头」——签名覆盖
// 不到的东西就不是契约。
//
// 代价是换板要重新打包。那是对的：板级配置本来就是镜像的一部分。
package boardprofile

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// SchemaVersion 是本包能解析的板级配置版本。
const SchemaVersion = 1

// ConfigRoleSuffix 是配置面资源在采集面 role 上加的后缀。
//
// # 为什么配置面不能复用同一个 role
//
// 一路摄像头对应【两个资源】：nervus.resource.camera（采集，共享观察）与
// nervus.resource.camera.config（配置，独占租约）。两者资源类型不同，但
// Catalog 的 handle 索引是【跨类型全局唯一】的——同名 handle 会在装配期
// 直接报 "resource handle collides"，整份 Catalog 构建失败。
//
// 所以配置面另起一个 role：cam.front 的配置面是 cam.front.config。
const ConfigRoleSuffix = ".config"

// 平台语义标签键。【白名单不是洁癖】：一个拼错的键（nervus.camera.face）不会
// 报错，只会让这路摄像头再也选不到——App 按 facing 查，查不到，然后有人花
// 半天去查 IPC。构建期拒掉它，代价是一次编译失败。
const (
	LabelFacing = "nervus.camera.facing"
	LabelKind   = "nervus.camera.kind"
	LabelMount  = "nervus.camera.mount"

	// privateLabelPrefix 下的键不受白名单约束：那是 camerad 自己的私有命名空间，
	// 内核的命名空间规则允许本包自由使用。
	privateLabelPrefix = "nervus.camerad."
)

// 语义取值同样白名单。拼错的【值】和拼错的键一样致命，而且更隐蔽：
// facing="Front" 与 facing="front" 在 JSON 里看起来没差别。
var (
	knownFacing = map[string]bool{
		"front": true, "rear": true, "left": true, "right": true,
		"up": true, "down": true,
	}
	knownKind = map[string]bool{
		"color": true, "depth": true, "ir": true, "fisheye": true,
	}
)

// Profile 是一块板子的摄像头布局。
type Profile struct {
	Schema int    `json:"schema"`
	Board  string `json:"board"`

	// Cameras 是【位置固定】的摄像头：焊在板上的 MIPI 模组，或者装在机身里、
	// 接在确定 USB 口上的模组。它们的 role 与语义由板级事实决定，不随插拔变。
	Cameras []Camera `json:"cameras"`

	// USBPool 是【临时插上来的】USB 摄像头的落脚处。可选。
	USBPool *USBPool `json:"usb_pool,omitempty"`
}

// Camera 是一路位置固定的摄像头。
type Camera struct {
	// Role 是稳定句柄，如 "cam.front"。它进 Catalog，也是 ResourceSelector.role。
	Role string `json:"role"`

	// Match 说明这个 role 对应板上的哪个物理位置。
	Match Match `json:"match"`

	// Labels 是语义标签，App 按它选设备而不是按 role。
	//
	// 【这才是 App 该依赖的东西】：role 是板级命名（这块板叫 cam.front，
	// 下一块可能叫 camera0），标签是语义（朝前）。
	Labels map[string]string `json:"labels,omitempty"`
}

// Match 是设备定位方式。两者必须【恰好指定一个】。
type Match struct {
	// USBPath 是 USB 拓扑路径，如 "1-1.2"：控制器 1 的 1 号口下的 2 号口。
	//
	// 【用拓扑路径而不是 /dev/videoN】：videoN 的编号由驱动加载顺序决定，
	// 换个内核版本、多插一个设备，编号就变了。拓扑路径描述的是「插在哪个
	// 物理口上」，只要不重新布线就不会变。
	//
	// 【也不用序列号】：换一个同型号摄像头修机器，序列号变了，role 就丢了。
	// 板级配置描述的是位置，不是某一个具体的模组。
	USBPath string `json:"usb_path,omitempty"`

	// DevicePathLink 是 /dev/v4l/by-path/ 下的稳定符号链接名，用于 MIPI/CSI
	// 等不走 USB 的摄像头。
	DevicePathLink string `json:"device_path_link,omitempty"`
}

// USBPool 声明一段动态 USB 摄像头槽位。
//
// # 只支持冷插拔
//
// 槽位在【服务启动的那一刻】按 USB 拓扑路径排序一次性分配，之后不再变。
// 运行中插入的摄像头不会自动出现，拔掉的会让对应 endpoint 失效但槽位不回收。
//
// 热插拔要做的是：新设备出现 → 动态注册 endpoint → Catalog 里凭空多一个资源。
// 而 Catalog 的资源集合是构建期签名的，运行期加资源等于绕开签名。真要支持，
// 得先解决「动态资源如何进入受签名的契约」，那不是摄像头一个能力的事。
type USBPool struct {
	// RolePrefix 生成的 role 形如 "<prefix>.0"、"<prefix>.1"。
	RolePrefix string `json:"role_prefix"`

	// MaxSlots 是槽位数上限，构建期定死。
	//
	// 【必须有上限】：资源声明在构建期生成，数量得是确定的。插上第 MaxSlots+1
	// 个摄像头时它不会出现——这比让它出现在一个签名覆盖不到的资源上好。
	MaxSlots int `json:"max_slots"`
}

// Load 读并校验一份板级配置。
func Load(path string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("boardprofile: 读 %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse 解析并校验一份板级配置。
func Parse(raw []byte) (*Profile, error) {
	var p Profile
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	// 【拒绝未知字段】：一个拼错的键（"labels" 写成 "label"）会被静默忽略，
	// 结果是摄像头上没有标签、App 选不到，而配置文件看起来完全正常。
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return nil, fmt.Errorf("boardprofile: 解析失败: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Profile) validate() error {
	if p.Schema != SchemaVersion {
		return fmt.Errorf("boardprofile: schema = %d, 本构建只支持 %d", p.Schema, SchemaVersion)
	}
	if p.Board == "" {
		return fmt.Errorf("boardprofile: board 不能为空")
	}
	if len(p.Cameras) == 0 && p.USBPool == nil {
		return fmt.Errorf("boardprofile: 既没有固定摄像头也没有 USB 池，这份配置没有意义")
	}

	seen := make(map[string]struct{}, len(p.Cameras))
	for i := range p.Cameras {
		cam := &p.Cameras[i]
		if err := cam.validate(); err != nil {
			return err
		}
		if _, dup := seen[cam.Role]; dup {
			return fmt.Errorf("boardprofile: role %q 重复", cam.Role)
		}
		seen[cam.Role] = struct{}{}
	}

	if p.USBPool != nil {
		if err := p.USBPool.validate(); err != nil {
			return err
		}
		// 池槽位与固定 role 撞名会让两路摄像头声称是同一个资源，而最终生效的
		// 那个取决于装配顺序。构建期拒掉。
		for _, role := range p.USBPool.Roles() {
			if _, dup := seen[role]; dup {
				return fmt.Errorf("boardprofile: USB 池槽位 %q 与固定摄像头 role 撞名", role)
			}
			seen[role] = struct{}{}
		}
	}

	// 配置面 role 由采集面 role 加后缀生成，它也要进 Catalog 的全局 handle
	// 索引。一个叫 cam.front 的摄像头会生成 cam.front.config，此时若另有一路
	// 摄像头本来就叫 cam.front.config，两者会在装配期撞 handle——那时报错指向
	// Catalog，看不出根因在这份 JSON 里。
	for role := range seen {
		configRole := role + ConfigRoleSuffix
		if _, dup := seen[configRole]; dup {
			return fmt.Errorf(
				"boardprofile: role %q 与 %q 的配置面 role 撞名（配置面 role 是采集面加 %q）",
				configRole, role, ConfigRoleSuffix)
		}
	}
	return nil
}

func (c *Camera) validate() error {
	if c.Role == "" {
		return fmt.Errorf("boardprofile: 有一路摄像头没有 role")
	}
	hasUSB := c.Match.USBPath != ""
	hasLink := c.Match.DevicePathLink != ""
	switch {
	case hasUSB && hasLink:
		return fmt.Errorf("boardprofile: %q 同时给了 usb_path 与 device_path_link，只能选一个",
			c.Role)
	case !hasUSB && !hasLink:
		return fmt.Errorf("boardprofile: %q 没有说明设备位置（usb_path 或 device_path_link）",
			c.Role)
	}
	return validateLabels(c.Role, c.Labels)
}

func validateLabels(role string, labels map[string]string) error {
	for key, value := range labels {
		if strings.HasPrefix(key, privateLabelPrefix) {
			continue
		}
		switch key {
		case LabelFacing:
			if !knownFacing[value] {
				return fmt.Errorf("boardprofile: %q 的 %s = %q 不是已知朝向（%s）",
					role, key, value, sortedKeys(knownFacing))
			}
		case LabelKind:
			if !knownKind[value] {
				return fmt.Errorf("boardprofile: %q 的 %s = %q 不是已知类型（%s）",
					role, key, value, sortedKeys(knownKind))
			}
		case LabelMount:
			if value == "" {
				return fmt.Errorf("boardprofile: %q 的 %s 为空", role, key)
			}
		default:
			return fmt.Errorf(
				"boardprofile: %q 用了未知标签键 %q；平台语义标签限于 %s/%s/%s，"+
					"私有标签须以 %s 开头",
				role, key, LabelFacing, LabelKind, LabelMount, privateLabelPrefix)
		}
	}
	return nil
}

func (u *USBPool) validate() error {
	if u.RolePrefix == "" {
		return fmt.Errorf("boardprofile: usb_pool.role_prefix 不能为空")
	}
	if strings.ContainsAny(u.RolePrefix, " \t\n") {
		return fmt.Errorf("boardprofile: usb_pool.role_prefix %q 含空白字符", u.RolePrefix)
	}
	if u.MaxSlots <= 0 {
		return fmt.Errorf("boardprofile: usb_pool.max_slots = %d，必须为正", u.MaxSlots)
	}
	// 上限不是为了省内存，是为了让「板上到底可能有几个摄像头」这件事有个
	// 说得出口的答案。64 路已经远超任何机器人形态。
	if u.MaxSlots > 64 {
		return fmt.Errorf("boardprofile: usb_pool.max_slots = %d，超过 64", u.MaxSlots)
	}
	return nil
}

// Roles 给出池里全部槽位的 role，顺序即槽位序号。
func (u *USBPool) Roles() []string {
	if u == nil {
		return nil
	}
	roles := make([]string, 0, u.MaxSlots)
	for i := 0; i < u.MaxSlots; i++ {
		roles = append(roles, fmt.Sprintf("%s.%d", u.RolePrefix, i))
	}
	return roles
}

// AllRoles 给出本配置声明的全部 role，固定摄像头在前、池槽位在后。
//
// 这就是 providergen 要写进 ProviderArtifacts 的资源集合——【包括当前没插
// 设备的池槽位】。资源声明是「这块板上可能有什么」，不是「此刻有什么」；
// 后者由 RegisterEndpoint 表达。
func (p *Profile) AllRoles() []string {
	roles := make([]string, 0, len(p.Cameras)+p.USBPool.slots())
	for _, cam := range p.Cameras {
		roles = append(roles, cam.Role)
	}
	return append(roles, p.USBPool.Roles()...)
}

func (u *USBPool) slots() int {
	if u == nil {
		return 0
	}
	return u.MaxSlots
}

// ConfigRole 给出某个采集面 role 对应的配置面 role。见 ConfigRoleSuffix。
func ConfigRole(role string) string { return role + ConfigRoleSuffix }

// AllConfigRoles 是 AllRoles 的配置面对应物，顺序一致。
func (p *Profile) AllConfigRoles() []string {
	roles := p.AllRoles()
	out := make([]string, len(roles))
	for i, role := range roles {
		out[i] = ConfigRole(role)
	}
	return out
}

// LabelsFor 返回某个 role 的语义标签。池槽位【没有标签】。
//
// 这不是遗漏：临时插上来的 USB 摄像头朝哪边、是什么类型，没有任何人知道。
// 随便给一个 facing 会让 App 按语义选到一个朝向未知的设备——那比选不到更糟。
func (p *Profile) LabelsFor(role string) map[string]string {
	for _, cam := range p.Cameras {
		if cam.Role != role {
			continue
		}
		if len(cam.Labels) == 0 {
			return nil
		}
		out := make(map[string]string, len(cam.Labels))
		for k, v := range cam.Labels {
			out[k] = v
		}
		return out
	}
	return nil
}

// FixedByUSBPath 与 FixedByDeviceLink 是运行期做 role 映射用的索引。
func (p *Profile) FixedByUSBPath() map[string]string {
	out := make(map[string]string)
	for _, cam := range p.Cameras {
		if cam.Match.USBPath != "" {
			out[cam.Match.USBPath] = cam.Role
		}
	}
	return out
}

func (p *Profile) FixedByDeviceLink() map[string]string {
	out := make(map[string]string)
	for _, cam := range p.Cameras {
		if cam.Match.DevicePathLink != "" {
			out[cam.Match.DevicePathLink] = cam.Role
		}
	}
	return out
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "/")
}
