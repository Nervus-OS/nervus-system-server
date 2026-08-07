// Package main 是 camerad 的 providergen：产出本服务的数据驱动 Provider 契约
// （provider.binpb + schemas.binpb），供 sysmanifest 计入 digests 并写进 manifest。
//
// # 与 pkgmanagerd 的 providergen 有一处根本不同
//
// 那一份是【纯静态】的：接口固定、不声明任何资源。本份要读板级 JSON——
// 一块板上有几路摄像头、各自叫什么、朝哪边，是板级事实，不是代码常量。
//
// 于是同一份 board.json 被读两次：
//
//	构建期  本程序读它 → 生成受签名的 nervus.resource.camera 声明
//	运行期  camerad 读它 → 把 role 映射到真实设备节点
//
// 【必须是同一份文件】。分成两份的话，「Catalog 里有 cam.front」和「运行时
// cam.front 指向哪个设备」会变成两个可以各自漂移的事实，而漂移之后两边都不
// 报错——只是 App 请求前视摄像头时拿到一个不存在的 endpoint。
//
// # 为什么资源集合必须在构建期定死
//
// ProviderArtifacts 进 manifest 的 digests，随镜像签名。如果资源是运行期从
// JSON 现读的，改一行 JSON 就能给自己加一路「前视摄像头」——签名覆盖不到的
// 东西就不是契约。
//
// 代价是换板要重新打包。那是对的：板级配置本来就是镜像的一部分。
//
// # 本包声明的东西为什么不需要与内核 bootstrap 比对
//
// pkgmanagerd 那份必须逐字复刻内核 bootstrap 里的同名接口（内核也定义了
// nervus.interface.pkg.manager）。摄像头【内核完全不认识】：没有任何一行
// 内核代码提到 camera。本包是这些定义的唯一来源，这正是数据驱动 Catalog
// 想要的形状。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

const (
	packageID = "nervus.camerad"

	interfaceCamera       = "nervus.interface.camera"
	interfaceCameraConfig = "nervus.interface.camera.config"
	interfaceMajor        = 1

	resourceCamera       = "nervus.resource.camera"
	resourceCameraConfig = "nervus.resource.camera.config"

	permissionCapture   = "perm.camera.capture"
	permissionConfigure = "perm.camera.configure"

	// 文件名必须与 manifest.json.in 的 provider 段一致
	descriptorFileName = "provider.binpb"
	schemasFileName    = "schemas.binpb"
)

func main() {
	out := flag.String("out", "", "输出目录（写入 provider.binpb 与 schemas.binpb）")
	profilePath := flag.String("profile", "", "板级摄像头配置 JSON")
	flag.Parse()

	if err := run(*out, *profilePath); err != nil {
		fmt.Fprintf(os.Stderr, "providergen(%s): %v\n", packageID, err)
		os.Exit(1)
	}
}

func run(out, profilePath string) error {
	if out == "" {
		return fmt.Errorf("必须指定 -out")
	}
	if profilePath == "" {
		return fmt.Errorf("必须指定 -profile")
	}

	profile, err := boardprofile.Load(profilePath)
	if err != nil {
		return err
	}

	descriptorWire, schemaWire, err := BuildArtifacts(profile)
	if err != nil {
		return err
	}

	// 0o644：这两个文件会被 digest 覆盖并随镜像只读发布，不需要可执行位
	if err := os.WriteFile(filepath.Join(out, descriptorFileName), descriptorWire, 0o644); err != nil {
		return fmt.Errorf("写 %s: %w", descriptorFileName, err)
	}
	if err := os.WriteFile(filepath.Join(out, schemasFileName), schemaWire, 0o644); err != nil {
		return fmt.Errorf("写 %s: %w", schemasFileName, err)
	}

	fmt.Printf("✅ %s provider 契约（板级 %s）\n", packageID, profile.Board)
	fmt.Printf("   %-16s %d 字节\n", descriptorFileName, len(descriptorWire))
	fmt.Printf("   %-16s %d 字节\n", schemasFileName, len(schemaWire))
	fmt.Printf("   声明资源       %d 路采集 + %d 路配置\n",
		len(profile.AllRoles()), len(profile.AllConfigRoles()))
	return nil
}

// BuildArtifacts 单独导出，让测试能在不落盘的情况下断言契约形状。
func BuildArtifacts(profile *boardprofile.Profile) (descriptorWire, schemaWire []byte, err error) {
	// method / event enum 都取自生成代码：method_id、event_id 与它们的元数据
	// 全挂在枚举值上，本地重抄一份的代价不是重复，而是它会悄悄过期。
	//
	// 【事件走 bundle 而不是内联到 descriptor】：内联那条路是给元数据接口
	// （没有 schema）用的，它不允许 payload_type——而摄像头的事件都有载荷。
	captureBundle, err := ipcregistry.BuildSchemaBundleWithEvents(
		interfaceCamera, interfaceMajor,
		camerav1.CameraMethod(0).Descriptor(),
		camerav1.CameraEvent(0).Descriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("构造 %s schema bundle: %w", interfaceCamera, err)
	}
	configBundle, err := ipcregistry.BuildSchemaBundleWithEvents(
		interfaceCameraConfig, interfaceMajor,
		camerav1.CameraConfigMethod(0).Descriptor(),
		camerav1.CameraConfigEvent(0).Descriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("构造 %s schema bundle: %w", interfaceCameraConfig, err)
	}

	descriptor := &ipcv1.ProviderDescriptor{
		PackageId: packageID,
		Interfaces: []*ipcv1.ProvidedInterface{
			{
				InterfaceId: interfaceCamera,
				InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
					Major:      interfaceMajor,
					SchemaHash: append([]byte(nil), captureBundle.GetSchemaHash()...),
				}},
				RequiredPermission: permissionCapture,
				// 风险下限：绑到本接口的资源不得低于 PRIVACY_SENSITIVE。
				// 它拦的是「厂商声明一路 NORMAL 的摄像头资源」——那会让取流
				// 绕过隐私敏感这一档的裁决。
				ResourceRiskFloor:       ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
				CompatibleResourceTypes: []string{resourceCamera},
				// 【不设 default_resource_type/role】：一台机器上有几路摄像头是
				// 常态，隐式默认会让「我要摄像头」静默解析到某一路。要哪一路
				// 必须显式说。
			},
			{
				InterfaceId: interfaceCameraConfig,
				InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
					Major:      interfaceMajor,
					SchemaHash: append([]byte(nil), configBundle.GetSchemaHash()...),
				}},
				RequiredPermission:      permissionConfigure,
				ResourceRiskFloor:       ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
				CompatibleResourceTypes: []string{resourceCameraConfig},
			},
		},
		Resources:   resourcesOf(profile),
		Permissions: permissions(),
	}

	set := &ipcv1.InterfaceSchemaBundleSet{
		Bundles: []*ipcv1.InterfaceSchemaBundle{captureBundle, configBundle},
	}
	return ipcregistry.MarshalProviderArtifacts(descriptor, set)
}

// resourcesOf 把板级配置翻成资源声明。
//
// 每个 role 产出【两个】资源：采集面（共享观察）与配置面（独占控制）。
// 两者的 stable_role 不同（配置面加 .config 后缀），因为 Catalog 的 handle
// 索引是跨类型全局唯一的——同名会在装配期直接撞 handle。
//
// 【包含当前没插设备的池槽位】：资源声明回答的是「这块板上可能有什么」，
// 「此刻有什么」由 RegisterEndpoint 表达。反过来会让插上第二个摄像头需要
// 重新打包。
func resourcesOf(profile *boardprofile.Profile) []*ipcv1.ManagedResource {
	roles := profile.AllRoles()
	out := make([]*ipcv1.ManagedResource, 0, len(roles)*2)

	for _, role := range roles {
		out = append(out, &ipcv1.ManagedResource{
			StableRole:   role,
			ResourceType: resourceCamera,
			// 【共享观察】：导航、录制、AI 检测同时看一路画面是常态，
			// 而「独占控制」表达不了那个语义。
			AccessMode: ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_SHARED_OBSERVE,
			RiskClass:  ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
			// 语义标签来自板级配置。池槽位没有标签——临时插上来的摄像头
			// 朝哪边没人知道，随便给一个会让 App 选到一个朝向未知的设备。
			Labels: profile.LabelsFor(role),
		})
		out = append(out, &ipcv1.ManagedResource{
			StableRole:   boardprofile.ConfigRole(role),
			ResourceType: resourceCameraConfig,
			// 【独占控制】：两个进程同时改曝光，最后谁说了算取决于时序，
			// 而画面会在两个值之间跳。
			AccessMode: ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
			RiskClass:  ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
			// 【配置面不带语义标签】：按标签选设备是采集面的事。配置面要
			// 配哪一路，由调用方从采集面 role 推出来（加 .config）。
			// 两边都打标签会让同一个 facing=front 命中两个不同类型的资源，
			// 而 REQUIRE_UNIQUE 会因此失败。
		})
	}
	return out
}

// permissions 声明本服务引入的两条平台权限。
//
// 【本包有资格定义 perm.*】：它签的是 platform-release。厂商包签 oem-service，
// 定义 perm.* 会被 authorizePermissionNamespace 拒绝——这正是「标准语义由平台
// 给出」的执行机制。
func permissions() []*ipcv1.DefinedPermission {
	return []*ipcv1.DefinedPermission{
		{
			Id: permissionCapture,
			// USER_CONSENT：这是【摄像头开始拍你】。没有比这更需要用户点头的
			// 事，也不该由安装动作代为同意。
			GrantMode:    ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
			MinimumTrust: ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			Group:        "camera",
			DisplayName:  &ipcv1.LocalizedText{ZhCn: "使用摄像头", En: "Use the camera"},
			Description: &ipcv1.LocalizedText{
				ZhCn: "拍摄照片与视频", En: "Take pictures and record video",
			},
		},
		{
			Id: permissionConfigure,
			// 同样 USER_CONSENT 而不是更松的一档：能改曝光就能把画面调到
			// 全黑，那对一个靠视觉导航的机器人是安全相关的。
			GrantMode:    ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
			MinimumTrust: ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			Group:        "camera",
			DisplayName:  &ipcv1.LocalizedText{ZhCn: "调整摄像头参数", En: "Adjust camera settings"},
			Description: &ipcv1.LocalizedText{
				ZhCn: "调整曝光、增益、白平衡等", En: "Adjust exposure, gain, white balance",
			},
		},
	}
}
