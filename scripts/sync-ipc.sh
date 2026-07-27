#!/usr/bin/env bash
# 把本仓库依赖的完整 nervus-ipc module 同步到指定版本（默认 master 最新）。
#
# 与 nervud/scripts/sync-ipc.sh 是同一份逻辑。两个仓库都依赖 nervus-ipc，
# 两边必须指向【同一个 commit】——否则内核与服务对同一份 Envelope 的理解
# 会悄悄分叉，而两边都能编译、都能跑，只在某个字段的行为上不一致。
#
# 为什么必须带 GOPRIVATE：nervus-ipc 是本组织自己的模块，刚推的 commit 在
# sum.golang.org 上往往还没索引，直接 go get 会撞 500。go.sum 仍然记录哈希，
# 首次拉取之后的任何篡改照样会被发现——关掉的是「第三方为我背书」，
# 不是「校验」本身。
#
# 用法：
#   scripts/sync-ipc.sh              同步到 master 最新
#   scripts/sync-ipc.sh <commit|tag> 同步到指定版本
#
# nervus-ipc 的 go.mod 位于仓库根目录，tag 不再使用旧的 go/ 子目录前缀。

set -euo pipefail

REF="${1:-master}"
MODULE="github.com/nervus-os/nervus-ipc"

cd "$(dirname "$0")/.."

echo "==> 同步 ${MODULE}@${REF}"
GOPRIVATE="github.com/nervus-os/*" GOPROXY=direct GOFLAGS=-mod=mod \
	go get "${MODULE}@${REF}"

echo "==> go mod tidy"
GOPRIVATE="github.com/nervus-os/*" GOPROXY=direct go mod tidy

echo "==> 当前版本"
grep nervus-ipc go.mod

echo "==> 编译与静态检查"
go build ./...
go vet ./...

echo "==> 测试"
go test ./...

echo
echo "⚠ 记得让 nervud 同步到【同一个 commit】："
echo "   cd ../nervud && scripts/sync-ipc.sh ${REF}"
echo "✅ 同步完成"
