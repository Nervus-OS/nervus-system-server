// Package main 是 pkgmanagerd 的 providergen：产出本服务的数据驱动 Provider 契约
// （provider.binpb + schemas.binpb），供 sysmanifest 计入 digests 并写进 manifest。
//
// # 为什么每个服务各有一份，而不是打包器统一生成
//
// 生成 schema bundle 需要 import 该接口的【生成代码】。如果由 sysmanifest 统一做，
// 它就得 import 全部接口的生成包——那等于把「加一个能力」变成「改打包工具」，
// 正是数据驱动 Catalog 要消灭的那种耦合。nervus-ipc 的 BuildSchemaBundle 注释
// 写着「打包器使用这个函数即可」，这个职责本来就在服务侧。
//
// # 它与内核 bootstrap 的关系
//
// nervud 的 catalog bootstrap 也定义了 nervus.interface.pkg.manager@1。当同一个
// 接口已存在于 Catalog 时，Builder 会用 sameInterfaceContract 逐字段比对——
// 【本文件产出的契约必须与内核那份完全一致】，任何一个字段不同都会让整份
// Catalog 构建失败，错误是 "interface ... conflicts with definition owned by ..."。
//
// 因此下面的 requiredPermission / 风险等级 / 资源字段都不是可以随手改的配置，
// 它们是在复刻 nervud internal/catalog/bootstrap.go 里那次 bootstrapInterface 调用。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

const (
	packageID   = "nervus.pkgmanagerd"
	interfaceID = "nervus.interface.pkg.manager"
	major       = 1

	// requiredPermission 必须与 nervud catalog bootstrap 对同一接口的声明一致。
	// 内核那边是 bootstrapInterface(InterfacePackageManager, ..., "perm.pkg.query", ...)
	requiredPermission = "perm.pkg.query"

	// 文件名必须与 manifest.json.in 的 provider 段一致
	descriptorFileName = "provider.binpb"
	schemasFileName    = "schemas.binpb"
)

func main() {
	out := flag.String("out", "", "输出目录（写入 provider.binpb 与 schemas.binpb）")
	flag.Parse()

	if err := run(*out); err != nil {
		_, err := fmt.Fprintf(os.Stderr, "providergen(%s): %v\n", packageID, err)
		if err != nil {
			return
		}
		os.Exit(1)
	}
}

func run(out string) error {
	if out == "" {
		return fmt.Errorf("必须指定 -out")
	}

	descriptorWire, schemaWire, err := buildArtifacts()
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

	fmt.Printf("✅ %s provider 契约\n", packageID)
	fmt.Printf("   %-16s %d 字节\n", descriptorFileName, len(descriptorWire))
	fmt.Printf("   %-16s %d 字节\n", schemasFileName, len(schemaWire))
	return nil
}

// buildArtifacts 单独拆出来，让测试能在不落盘的情况下断言契约形状。
func buildArtifacts() (descriptorWire, schemaWire []byte, err error) {
	// method enum 取自生成代码：method_id 与 MethodMeta 都挂在它的枚举值上，
	// 本地重抄一份的代价不是重复，而是它会悄悄过期
	bundle, err := ipcregistry.BuildSchemaBundle(
		interfaceID, major, pkgv1.PackageManagerMethod(0).Descriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("构造 schema bundle: %w", err)
	}

	descriptor := &ipcv1.ProviderDescriptor{
		PackageId: packageID,
		Interfaces: []*ipcv1.ProvidedInterface{{
			InterfaceId: interfaceID,
			InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
				Major:      major,
				SchemaHash: append([]byte(nil), bundle.GetSchemaHash()...),
			}},
			RequiredPermission: requiredPermission,
			// 装包接口不绑物理资源：风险等级留 UNSPECIFIED，资源字段全部留空。
			// 这三项也是 sameInterfaceContract 的比对对象，与内核 bootstrap 一致
		}},
		// 不声明 Resources：装包不占用任何 ManagedResource
		// 不声明 Permissions：perm.pkg.query / perm.pkg.install 都是平台权限，
		// 由 nervud bootstrap 定义。非 platform-release 的包声明 perm.* 会被
		// authorizePermissionNamespace 拒绝；本包虽是 platform-release，但重复
		// 定义同一权限会与内核那份冲突
	}

	set := &ipcv1.InterfaceSchemaBundleSet{
		Bundles: []*ipcv1.InterfaceSchemaBundle{bundle},
	}
	return ipcregistry.MarshalProviderArtifacts(descriptor, set)
}
