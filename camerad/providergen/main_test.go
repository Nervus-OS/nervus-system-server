package main

import (
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

const testProfile = `{
  "schema": 1, "board": "test-board",
  "cameras": [
    {"role": "cam.front", "match": {"usb_path": "1-1.1"},
     "labels": {"nervus.camera.facing": "front", "nervus.camera.kind": "color"}}
  ],
  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
}`

func buildTestArtifacts(t *testing.T) *ipcregistry.ProviderArtifacts {
	t.Helper()
	profile, err := boardprofile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	descriptorWire, schemaWire, err := BuildArtifacts(profile)
	if err != nil {
		t.Fatalf("BuildArtifacts: %v", err)
	}
	// 【走真的 ParseProviderArtifacts】而不是只断言结构体字段：那个函数带着
	// 一整套包内不变量（接口引用的资源类型必须由本包管理、风险下限不得高于
	// 资源风险、default 资源必须已声明）。绕过它就等于把这些检查推迟到装机现场。
	artifacts, err := ipcregistry.ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}
	return artifacts
}

func TestBuildArtifacts_PassesRegistryInvariants(t *testing.T) {
	buildTestArtifacts(t)
}

// 每个 role 产出两个资源：采集面 + 配置面。
//
// 配置面的 stable_role 必须【不同】：Catalog 的 handle 索引跨资源类型全局唯一，
// 同名会在装机时撞 handle，而那时的错误信息指向 Catalog，不指向这份代码。
func TestBuildArtifacts_EachRoleYieldsCaptureAndConfig(t *testing.T) {
	profile, err := boardprofile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	resources := resourcesOf(profile)

	// 1 路固定 + 2 个池槽位 = 3 个 role，各出两个资源
	if len(resources) != 6 {
		t.Fatalf("资源数 = %d, want 6", len(resources))
	}

	handles := make(map[string]bool, len(resources))
	for _, r := range resources {
		if handles[r.GetStableRole()] {
			t.Fatalf("stable_role %q 重复——Catalog 的 handle 索引会直接拒绝",
				r.GetStableRole())
		}
		handles[r.GetStableRole()] = true
	}

	byRole := make(map[string]*ipcv1.ManagedResource, len(resources))
	for _, r := range resources {
		byRole[r.GetStableRole()] = r
	}

	capture := byRole["cam.front"]
	if capture.GetResourceType() != resourceCamera {
		t.Errorf("cam.front 类型 = %q", capture.GetResourceType())
	}
	// 【共享观察】：多个 App 同时看一路画面是常态。
	if capture.GetAccessMode() != ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_SHARED_OBSERVE {
		t.Errorf("cam.front access_mode = %v, want SHARED_OBSERVE", capture.GetAccessMode())
	}

	config := byRole["cam.front.config"]
	if config == nil {
		t.Fatal("缺少 cam.front 的配置面资源")
	}
	// 【独占控制】：两个进程同时改曝光，结果取决于时序。
	if config.GetAccessMode() != ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL {
		t.Errorf("cam.front.config access_mode = %v, want EXCLUSIVE_CONTROL", config.GetAccessMode())
	}
	if config.GetResourceType() != resourceCameraConfig {
		t.Errorf("cam.front.config 类型 = %q", config.GetResourceType())
	}
}

// 语义标签只挂在采集面。
//
// 两边都挂会让 facing=front 同时命中两个不同类型的资源，而 ResourceSelector
// 默认是 REQUIRE_UNIQUE——App 按标签选设备会因此直接失败。
func TestBuildArtifacts_LabelsOnlyOnCaptureSide(t *testing.T) {
	profile, err := boardprofile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, r := range resourcesOf(profile) {
		switch r.GetResourceType() {
		case resourceCamera:
			if r.GetStableRole() == "cam.front" &&
				r.GetLabels()["nervus.camera.facing"] != "front" {
				t.Errorf("cam.front 丢了 facing 标签: %v", r.GetLabels())
			}
		case resourceCameraConfig:
			if len(r.GetLabels()) != 0 {
				t.Errorf("配置面资源 %q 带了标签: %v", r.GetStableRole(), r.GetLabels())
			}
		}
	}
}

// 【池槽位没有语义标签】：临时插上来的摄像头朝哪边没人知道。
func TestBuildArtifacts_PoolSlotsAreUnlabelled(t *testing.T) {
	profile, err := boardprofile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, r := range resourcesOf(profile) {
		if r.GetStableRole() == "cam.usb.0" && len(r.GetLabels()) != 0 {
			t.Fatalf("池槽位带了标签: %v", r.GetLabels())
		}
	}
}

// 【不设 default_resource_type/role】。
//
// 一台机器上有几路摄像头是常态，隐式默认会让「我要摄像头」静默解析到某一路——
// 而 v2 刚刚才把底盘的那条隐式默认删掉，不该在这里种一条新的。
func TestBuildArtifacts_NoImplicitDefaultResource(t *testing.T) {
	artifacts := buildTestArtifacts(t)
	for _, iface := range artifacts.Descriptor.GetInterfaces() {
		if iface.GetDefaultResourceType() != "" || iface.GetDefaultResourceRole() != "" {
			t.Errorf("%s 声明了隐式默认资源 %q/%q",
				iface.GetInterfaceId(),
				iface.GetDefaultResourceType(), iface.GetDefaultResourceRole())
		}
	}
}

// 两个接口都必须带上事件元数据，【而且要带 payload_type】。
//
// 漏了的话服务能报到成功，然后每一条 PublishEvent 都被内核拒掉——而拒绝
// 只进内核审计，服务侧看不到任何回音，表现是「订阅了但永远收不到事件」。
//
// 事件走 schema bundle 而不是 descriptor 的内联字段：内联那条路是给元数据
// 接口用的，它不允许 payload_type（没有 descriptor set 就无从校验类型名）。
func TestBuildArtifacts_InterfacesCarryTypedEvents(t *testing.T) {
	artifacts := buildTestArtifacts(t)

	for _, interfaceID := range []string{interfaceCamera, interfaceCameraConfig} {
		schema, ok := artifacts.Schemas.Lookup(interfaceID, interfaceMajor)
		if !ok {
			t.Fatalf("%s 没有 schema", interfaceID)
		}
		events := schema.Events()
		if len(events) == 0 {
			t.Fatalf("%s 没有声明任何事件", interfaceID)
		}
		for id, meta := range events {
			if meta.GetPayloadType() == "" {
				t.Errorf("%s 的事件 %d 没有载荷类型", interfaceID, id)
			}
			// delivery_class 不能留空：留空时 nervud fail closed 按 RELIABLE
			// 处理，而一路视频的状态流按 RELIABLE 会在消费方稍慢时被断开。
			if meta.GetDeliveryClass() == ipcv1.DeliveryClass_DELIVERY_CLASS_UNSPECIFIED {
				t.Errorf("%s 的事件 %d 没有声明 delivery_class", interfaceID, id)
			}
		}
	}
}

// 描述符里【不该】内联事件：那条路是给元数据接口用的，两处都写会被
// registry 当成「同一个接口既内联又带 bundle」直接拒绝。
func TestBuildArtifacts_DescriptorDoesNotInlineEvents(t *testing.T) {
	artifacts := buildTestArtifacts(t)
	for _, iface := range artifacts.Descriptor.GetInterfaces() {
		for _, version := range iface.GetInterfaceVersions() {
			if len(version.GetEvents()) != 0 {
				t.Errorf("%s@%d 在 descriptor 里内联了事件",
					iface.GetInterfaceId(), version.GetMajor())
			}
		}
	}
}

// 两条权限都是 USER_CONSENT：摄像头开始拍你这件事不该由安装动作代为同意。
func TestBuildArtifacts_CameraPermissionsRequireUserConsent(t *testing.T) {
	for _, permission := range permissions() {
		if permission.GetGrantMode() != ipcv1.GrantMode_GRANT_MODE_USER_CONSENT {
			t.Errorf("%s grant_mode = %v, want USER_CONSENT",
				permission.GetId(), permission.GetGrantMode())
		}
		if permission.GetRiskClass() != ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE {
			t.Errorf("%s risk_class = %v, want PRIVACY_SENSITIVE",
				permission.GetId(), permission.GetRiskClass())
		}
	}
}

// 仓库里的参考板级配置必须能构建出合法契约——它是新板子的抄写模板，
// 模板本身坏掉会让每一块新板都从一个错误开始。
func TestBuildArtifacts_ReferenceBoardIsValid(t *testing.T) {
	profile, err := boardprofile.Load("../boards/reference.json")
	if err != nil {
		t.Fatalf("加载参考板级配置: %v", err)
	}
	descriptorWire, schemaWire, err := BuildArtifacts(profile)
	if err != nil {
		t.Fatalf("BuildArtifacts: %v", err)
	}
	if _, err := ipcregistry.ParseProviderArtifacts(descriptorWire, schemaWire); err != nil {
		t.Fatalf("参考板级配置产出的契约不合法: %v", err)
	}
}
