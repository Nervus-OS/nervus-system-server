package main

import (
	"bytes"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

// 运行时报到用的 hash 必须与 providergen 落盘、最终进 Catalog 的那一份相等。
//
// 这两个值来自两个独立的 main 包，靠「都调用 BuildSchemaBundle」这个约定保持
// 一致——约定没有编译期保障，断在这里才有。曾经 main.go 干脆没填 SchemaHash，
// 表现是 pkgmanagerd 每两秒握手、报到被拒、退出一次，而调用方看到的却是
// ResolveEndpoint 的 RESOURCE_NOT_FOUND，与真实原因毫无关系。
func TestSchemaHashMatchesProvidergenArtifacts(t *testing.T) {
	runtimeHash, err := schemaHash()
	if err != nil {
		t.Fatalf("schemaHash: %v", err)
	}
	if len(runtimeHash) != sha256.Size {
		t.Fatalf("运行时 hash 有 %d 字节, want %d", len(runtimeHash), sha256.Size)
	}

	// 直接跑 providergen 而不是在这里重抄一遍它的构造逻辑：重抄出来的副本会跟着
	// 被一起改，那样这个测试就永远不会红。
	dir := t.TempDir()
	cmd := exec.Command("go", "run", "./providergen", "-out", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go run ./providergen: %v\n%s", err, out)
	}

	artifacts, err := ipcregistry.ParseProviderArtifacts(
		readArtifact(t, dir, "provider.binpb"),
		readArtifact(t, dir, "schemas.binpb"))
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}

	schema, ok := artifacts.Schemas.Lookup(interfaceID, interfaceMajor)
	if !ok {
		t.Fatalf("providergen 产物里没有 %s@%d", interfaceID, interfaceMajor)
	}
	if !bytes.Equal(runtimeHash, schema.Hash()) {
		t.Errorf("运行时 hash 与 providergen 产物不符\n runtime = %x\n artifact = %x",
			runtimeHash, schema.Hash())
	}
}

func readArtifact(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("读 %s: %v", name, err)
	}
	return b
}
