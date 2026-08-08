// 本文件是本服务的契约常量与 schema hash。
//
// # 为什么 hash 是【算】出来的而不是常量
//
// RegisterEndpoint 要带 schema_hash，nervud 会与 Catalog 里登记的那份逐字节
// 比对，不符即拒。本接口的 Catalog 那一份有两个来源，两边算法完全相同：
//
//	nervud   bootstrap.go buildBootstrapArtifacts → BuildSchemaBundle(...) → Catalog
//	providergen                                   → BuildSchemaBundle(...) → provider.binpb
//	本服务                                         → BuildSchemaBundle(...) → 报到时带上
//
// 把 hash 写成常量或从文件读，就多了一个可以与生成代码漂移的副本；漂移之后
// 服务报到会被拒，而错误信息只说「schema hash 不符」，不会说是谁过期了。
//
// # hash 只取决于 descriptor set
//
// BuildSchemaBundle 对 FileDescriptorSet 取 sha256，interfaceID 与 major 都不
// 参与。因此这里与 providergen 的常量即使写岔也不会表现为 hash 不符——那种
// 错误会在 Catalog 构建时以 sameInterfaceContract 冲突的形式暴露。真正要求
// 一致的是【三个仓库 pin 同一个 nervus-ipc commit】：descriptor 变了 hash 就变。
package main

import (
	"fmt"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

// interfaceMajor 必须与 providergen 声明的 major 以及 manifest 导出的接口一致。
const interfaceMajor = 1

// schemaHash 算出本接口的 schema hash。
//
// 与 providergen 走【同一个构造函数、同一个方法枚举】——那是两边不漂移的唯一
// 保证，任何一边改用别的枚举都会立刻表现为报到被拒。
func schemaHash() ([]byte, error) {
	bundle, err := ipcregistry.BuildSchemaBundle(
		interfaceID, interfaceMajor, pkgv1.PackageManagerMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("pkgmanagerd: 构造 %s schema bundle: %w", interfaceID, err)
	}
	return bundle.GetSchemaHash(), nil
}
