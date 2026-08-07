package boardprofile

import (
	"fmt"
	"strings"
	"testing"
)

const validProfile = `{
  "schema": 1,
  "board": "acme-quadruped-v2",
  "cameras": [
    {
      "role": "cam.front",
      "match": {"usb_path": "1-1.2"},
      "labels": {"nervus.camera.facing": "front", "nervus.camera.kind": "color"}
    },
    {
      "role": "cam.depth",
      "match": {"device_path_link": "platform-csi0-video-index0"},
      "labels": {"nervus.camera.kind": "depth", "nervus.camera.facing": "front"}
    }
  ],
  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
}`

func mustParse(t *testing.T, raw string) *Profile {
	t.Helper()
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func mustReject(t *testing.T, raw, wantSubstring string) {
	t.Helper()
	_, err := Parse([]byte(raw))
	if err == nil {
		t.Fatal("这份配置本该被拒")
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("err = %v, want 含 %q", err, wantSubstring)
	}
}

func TestParse_Valid(t *testing.T) {
	p := mustParse(t, validProfile)
	if p.Board != "acme-quadruped-v2" {
		t.Errorf("board = %q", p.Board)
	}
	if len(p.Cameras) != 2 {
		t.Fatalf("cameras = %d, want 2", len(p.Cameras))
	}
}

// 资源集合【包含当前没插设备的池槽位】。
//
// 资源声明回答的是「这块板上可能有什么」；「此刻有什么」由 RegisterEndpoint
// 表达。反过来（只声明当前插着的）会让插上第二个摄像头需要重新打包。
func TestAllRoles_IncludesEmptyPoolSlots(t *testing.T) {
	p := mustParse(t, validProfile)
	got := p.AllRoles()
	want := []string{"cam.front", "cam.depth", "cam.usb.0", "cam.usb.1"}
	if len(got) != len(want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("roles = %v, want %v", got, want)
		}
	}
}

// 【池槽位没有语义标签】。临时插上来的摄像头朝哪边没人知道，
// 随便给一个 facing 会让 App 按语义选到一个朝向未知的设备。
func TestLabelsFor_PoolSlotsHaveNone(t *testing.T) {
	p := mustParse(t, validProfile)
	if labels := p.LabelsFor("cam.usb.0"); labels != nil {
		t.Fatalf("池槽位带了标签: %v", labels)
	}
	if labels := p.LabelsFor("cam.front"); labels[LabelFacing] != "front" {
		t.Fatalf("cam.front 标签 = %v", labels)
	}
}

// 返回的是副本：调用方改它不该影响 Profile。
func TestLabelsFor_ReturnsCopy(t *testing.T) {
	p := mustParse(t, validProfile)
	labels := p.LabelsFor("cam.front")
	labels[LabelFacing] = "rear"
	if again := p.LabelsFor("cam.front"); again[LabelFacing] != "front" {
		t.Fatalf("Profile 被调用方改坏了: %v", again)
	}
}

// 【未知字段必须报错】。"label" 拼成单数会被静默忽略，结果是摄像头没有标签、
// App 选不到，而配置文件看起来完全正常。
func TestParse_UnknownFieldIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1"}, "label": {"x": "y"}}]
	}`, "unknown field")
}

// 拼错的标签键在构建期就得拒。运行期的表现是「这路摄像头再也选不到」，
// 而没有任何东西会指向那个拼写错误。
func TestParse_UnknownLabelKeyIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1"},
	               "labels": {"nervus.camera.face": "front"}}]
	}`, "未知标签键")
}

// 拼错的标签【值】同样致命，而且更隐蔽：facing="Front" 在 JSON 里看不出问题。
func TestParse_UnknownLabelValueIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1"},
	               "labels": {"nervus.camera.facing": "Front"}}]
	}`, "不是已知朝向")
}

// camerad 自己命名空间下的私有标签不受白名单约束。
func TestParse_PrivateLabelNamespaceIsFree(t *testing.T) {
	p := mustParse(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1"},
	               "labels": {"nervus.camerad.tuning_profile": "indoor"}}]
	}`)
	if got := p.LabelsFor("cam.a")["nervus.camerad.tuning_profile"]; got != "indoor" {
		t.Fatalf("私有标签 = %q", got)
	}
}

// 位置必须【恰好】指定一种。两种都给意味着有歧义，一种都不给意味着这个 role
// 永远绑不到设备。
func TestParse_MatchMustBeExactlyOne(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1", "device_path_link": "x"}}]
	}`, "只能选一个")

	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {}}]
	}`, "没有说明设备位置")
}

func TestParse_DuplicateRoleIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [
	    {"role": "cam.a", "match": {"usb_path": "1-1"}},
	    {"role": "cam.a", "match": {"usb_path": "1-2"}}
	  ]
	}`, "重复")
}

// 池槽位与固定 role 撞名会让两路摄像头声称是同一个资源，
// 最终生效的那个取决于装配顺序。
func TestParse_PoolSlotCollidingWithFixedRoleIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.usb.0", "match": {"usb_path": "1-1"}}],
	  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
	}`, "撞名")
}

// 配置面 role 由采集面加后缀生成，它也要进 Catalog 的全局 handle 索引。
//
// 撞名不在这里拒的话，报错会在装配期指向 Catalog（"resource handle collides"），
// 而根因在这份 JSON 里——那时没人会想到回来看板级配置。
func TestParse_ConfigRoleCollisionIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [
	    {"role": "cam.front", "match": {"usb_path": "1-1"}},
	    {"role": "cam.front.config", "match": {"usb_path": "1-2"}}
	  ]
	}`, "配置面 role 撞名")
}

// 池槽位的配置面 role 同样参与碰撞检查。
func TestParse_PoolConfigRoleCollisionIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.usb.0.config", "match": {"usb_path": "1-9"}}],
	  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
	}`, "配置面 role 撞名")
}

func TestAllConfigRoles_MirrorsAllRoles(t *testing.T) {
	p := mustParse(t, validProfile)
	capture, config := p.AllRoles(), p.AllConfigRoles()
	if len(capture) != len(config) {
		t.Fatalf("采集面 %d 路、配置面 %d 路，必须一一对应", len(capture), len(config))
	}
	for i := range capture {
		if config[i] != capture[i]+ConfigRoleSuffix {
			t.Fatalf("config[%d] = %q, want %q", i, config[i], capture[i]+ConfigRoleSuffix)
		}
	}
}

// schema 版本不匹配直接拒，不做「尽力而为」的解析。
func TestParse_SchemaVersionMustMatch(t *testing.T) {
	mustReject(t, `{"schema": 2, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1"}}]}`, "只支持")
}

// 空配置没有意义，多半是文件写错了位置。
func TestParse_EmptyProfileIsRejected(t *testing.T) {
	mustReject(t, `{"schema": 1, "board": "b"}`, "没有意义")
}

func TestParse_PoolBoundsAreChecked(t *testing.T) {
	base := `{"schema": 1, "board": "b", "usb_pool": {"role_prefix": %q, "max_slots": %d}}`
	mustReject(t, fmt.Sprintf(base, "", 2), "role_prefix 不能为空")
	mustReject(t, fmt.Sprintf(base, "cam.usb", 0), "必须为正")
	mustReject(t, fmt.Sprintf(base, "cam.usb", 65), "超过 64")
}

func TestIndexes_MapPositionToRole(t *testing.T) {
	p := mustParse(t, validProfile)
	if got := p.FixedByUSBPath()["1-1.2"]; got != "cam.front" {
		t.Errorf("USB 索引 = %q, want cam.front", got)
	}
	if got := p.FixedByDeviceLink()["platform-csi0-video-index0"]; got != "cam.depth" {
		t.Errorf("链接索引 = %q, want cam.depth", got)
	}
}
