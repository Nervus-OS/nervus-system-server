//go:build linux

// 本文件是本服务的契约常量与 schema hash。
//
// # 为什么 hash 是【算】出来的而不是构建期传进来的
//
// RegisterEndpoint 要带 schema_hash，nervud 会与 Catalog 里登记的那份逐字节
// 比对，不符即拒。两个值都来自同一段生成代码：
//
//	providergen  BuildSchemaBundle(...) → 写进 ProviderArtifacts → 进 Catalog
//	本服务       BuildSchemaBundle(...) → 报到时带上
//
// 把 hash 写成常量或从文件读，就多了一个可以与生成代码漂移的副本；漂移之后
// 服务报到会被拒，而错误信息只说「schema hash 不符」，不会说是谁过期了。
//
// 现在两边都从 camerav1 的枚举描述符算，编译期就绑在同一份 proto 上。
package main

import (
	"fmt"

	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

// 本服务的身份与接口。componentID 必须与 manifest.json 的 components[].id 一致——
// nervud 会用对端 PID → cgroup → systemd unit 核对它，对不上直接拒绝握手。
const (
	packageID   = "nervus.camerad"
	componentID = "main"

	interfaceCamera       = "nervus.interface.camera"
	interfaceCameraConfig = "nervus.interface.camera.config"

	interfaceMajor = 1
)

// 平台资源类型与权限。与 providergen 声明的必须一致。
const (
	resourceCamera       = "nervus.resource.camera"
	resourceCameraConfig = "nervus.resource.camera.config"

	permissionCapture   = "perm.camera.capture"
	permissionConfigure = "perm.camera.configure"
)

// schemaHashes 算出两个接口的 schema hash。
func schemaHashes() (capture, config []byte, err error) {
	// 与 providergen 走【同一个构造函数、同一批枚举】。事件枚举不影响
	// schema_hash（它与方法枚举同文件，本来就在那份 descriptor set 里），
	// 但两处保持一致才不会有人以后改了一边忘了另一边。
	captureBundle, err := ipcregistry.BuildSchemaBundleWithEvents(
		interfaceCamera, interfaceMajor,
		camerav1.CameraMethod(0).Descriptor(),
		camerav1.CameraEvent(0).Descriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("camerad: 构造 %s schema bundle: %w", interfaceCamera, err)
	}
	configBundle, err := ipcregistry.BuildSchemaBundleWithEvents(
		interfaceCameraConfig, interfaceMajor,
		camerav1.CameraConfigMethod(0).Descriptor(),
		camerav1.CameraConfigEvent(0).Descriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("camerad: 构造 %s schema bundle: %w", interfaceCameraConfig, err)
	}
	return captureBundle.GetSchemaHash(), configBundle.GetSchemaHash(), nil
}
