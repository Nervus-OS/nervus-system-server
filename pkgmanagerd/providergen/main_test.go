package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

// 本文件产出的契约必须与 nervud internal/catalog/bootstrap.go 里
// bootstrapInterface(InterfacePackageManager, ..., "perm.pkg.query", UNSPECIFIED, nil, "", "")
// 逐字段一致，否则内核 Builder 的 sameInterfaceContract 会让整份 Catalog 构建失败。
//
// 这些断言是那份「远处的约定」在本仓库里的锚点：改动任何一项都会在这里响，
// 而不是等到目标机启动时报一句 "conflicts with definition owned by ..."。
func TestBuildArtifacts_MatchesKernelBootstrapContract(t *testing.T) {
	descriptorWire, schemaWire, err := buildArtifacts()
	if err != nil {
		t.Fatalf("buildArtifacts: %v", err)
	}

	artifacts, err := ipcregistry.ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}

	if got := artifacts.Descriptor.GetPackageId(); got != packageID {
		t.Errorf("package_id = %q, want %q", got, packageID)
	}
	ifaces := artifacts.Descriptor.GetInterfaces()
	if len(ifaces) != 1 {
		t.Fatalf("接口数 = %d, want 1", len(ifaces))
	}
	iface := ifaces[0]

	if iface.GetInterfaceId() != interfaceID {
		t.Errorf("interface_id = %q, want %q", iface.GetInterfaceId(), interfaceID)
	}
	if iface.GetRequiredPermission() != requiredPermission {
		t.Errorf("required_permission = %q, want %q（内核 bootstrap 用的是这个）",
			iface.GetRequiredPermission(), requiredPermission)
	}
	// 装包接口不绑物理资源。这三项非空会与内核 bootstrap 那份不一致
	if iface.GetResourceRiskFloor() != ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED {
		t.Errorf("resource_risk_floor = %v, want UNSPECIFIED", iface.GetResourceRiskFloor())
	}
	if len(iface.GetCompatibleResourceTypes()) != 0 {
		t.Errorf("compatible_resource_types = %v, want 空", iface.GetCompatibleResourceTypes())
	}
	if iface.GetDefaultResourceType() != "" || iface.GetDefaultResourceRole() != "" {
		t.Errorf("default resource = (%q, %q), want 两个都空",
			iface.GetDefaultResourceType(), iface.GetDefaultResourceRole())
	}

	// 不自行定义 perm.*：那些是平台权限，由 nervud bootstrap 拥有，
	// 重复定义会撞 "conflicts with definition owned by"
	if n := len(artifacts.Descriptor.GetPermissions()); n != 0 {
		t.Errorf("permissions 数 = %d, want 0（平台权限由内核定义）", n)
	}
	if n := len(artifacts.Descriptor.GetResources()); n != 0 {
		t.Errorf("resources 数 = %d, want 0（装包不占用 ManagedResource）", n)
	}

	// 声明的 schema_hash 必须真的是 bundle 的 hash
	versions := iface.GetInterfaceVersions()
	if len(versions) != 1 || versions[0].GetMajor() != major {
		t.Fatalf("interface_versions = %+v, want 单个 major=%d", versions, major)
	}
	schema, ok := artifacts.Schemas.Lookup(interfaceID, major)
	if !ok {
		t.Fatalf("schema set 里找不到 %s@%d", interfaceID, major)
	}
	if !bytes.Equal(schema.Hash(), versions[0].GetSchemaHash()) {
		t.Error("声明的 schema_hash 与 bundle 实际 hash 不符")
	}
}

// digest 在构建期算好、目标机无法重算，所以同一份输入必须每次产出相同字节。
func TestBuildArtifacts_IsDeterministic(t *testing.T) {
	firstDesc, firstSchema, err := buildArtifacts()
	if err != nil {
		t.Fatalf("buildArtifacts: %v", err)
	}
	secondDesc, secondSchema, err := buildArtifacts()
	if err != nil {
		t.Fatalf("buildArtifacts again: %v", err)
	}
	if !bytes.Equal(firstDesc, secondDesc) || !bytes.Equal(firstSchema, secondSchema) {
		t.Fatal("provider 契约的编码不确定，digest 会在两次构建间漂移")
	}
}

// 文件名必须与 manifest.json.in 的 provider 段一致；对不上时 sysmanifest 会以
// 「未被 digests 覆盖」失败，而那个错误不会指出是文件名写岔了。
func TestRun_WritesBothArtifactFiles(t *testing.T) {
	dir := t.TempDir()
	if err := run(dir); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, name := range []string{descriptorFileName, schemasFileName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("缺少 %s: %v", name, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s 是空文件", name)
		}
	}
}

func TestRun_RequiresOutDir(t *testing.T) {
	if err := run(""); err == nil {
		t.Fatal("空 -out 被接受，应当报错")
	}
}
