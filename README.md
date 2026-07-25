# nervus-system-server

Nervus OS（NSOS）的**系统服务层**。这些服务随系统镜像发布，配合 `nervud` 内核
提供软件安装等系统能力。

```text
App (Kotlin/Compose)  ──IPC──▶  系统服务 (本仓库, Go)  ──adminwire──▶  nervud
                                        │
                                   nervus-ipc/go/sdk
```

## 布局：一个服务一个目录

**根目录下每一个目录就是一个系统服务**，目录里直接放该服务的代码。
唯一的例外是 `scripts/`，它装构建工具。

```text
nervus-system-server/
├── pkgmanagerd/            ← 服务
│   ├── main.go             进程入口
│   ├── service.go          业务实现
│   ├── adminclient.go      管理通道客户端（只有本服务可用）
│   ├── unpack.go           .nspkg 解包 + tar-slip 防护
│   ├── manifest.json.in    包清单模板
│   └── README.md
├── sessiond/               ← 服务（待落地）
└── scripts/                构建工具，不是服务
    ├── build-image-tree.sh
    ├── sync-ipc.sh
    └── sysmanifest/        digest 计算 + manifest 填充 + Ed25519 签名
```

**服务目录名不得冲突**：它同时是二进制名、Go 包路径、以及 `build-image-tree.sh`
里的构建目标。

### 服务之间不能互相 import —— 由编译器保证

本仓库**每一个 `.go` 文件都是 `package main`**，包括 `scripts/sysmanifest`。
Go 禁止 import `package main`，所以：

- 服务 A 想 import 服务 B —— **编译期就过不去**
- 谁想 import 构建工具 —— 同样过不去

这不是靠约定或 code review 维持的，是语言层挡着。一个服务的一切都在它自己
那个目录里：代码、manifest 模板、README。

这条性质有个具体的安全收益：`pkgmanagerd/adminclient.go` 是管理通道的客户端，
而内核只放行 pkgmanagerd 一个 UID 连那条 socket。放在服务自己的包里，别的服务
**想用都 import 不到**——「只有 pkgmanagerd 能碰管理通道」因此是结构性约束。
如果它在一个共享包里，别的服务就能写出编译通过、运行时被内核拒的代码，
而那种失败排查起来远比编译错误费劲。

需要共享代码时，正确做法是把它放进 `nervus-ipc`（协议与 SDK 的家）或另起一个
被明确设计成公共依赖的仓库，**不是**在本仓库里开一个共享包。

`scripts/` 装构建工具，不是服务，也不参与镜像产物。`nervud` 仓库同样有一个
`scripts/`，此处沿用同一约定。

（与 `nervud` 的目录布局不同是有原因的：那个仓库有一个主二进制，所以 `main.go`
在根、`cmd/` 放附属工具。这里没有主次之分，是 N 个平级服务。）

## 部署位置

系统包装在**只读镜像区**：

```text
/usr/lib/nervus/system-packages/
├── nervus.pkgmanagerd/
│   ├── manifest.json
│   ├── manifest.sig
│   └── bin/pkgmanagerd
└── <下一个服务>/
```

目录名必须等于 `manifest.json` 里的 `package_id`。内核的启动扫描是
`Glob("/usr/lib/nervus/system-packages/*/manifest.json")`。

**装进去靠刷镜像或 OTA，不能用 `nervusctl install`**——那条路只接受
`SourceDynamicInstall`，系统包走它会被直接拒绝（`ErrSystemPackageImmutable`）。

运行期路径由内核分配，不需要手工建：

| 路径 | 用途 |
|---|---|
| `/var/lib/nervus/package-data/<package_id>/` | 私有数据目录，属主为包 UID，`0700` |
| `/var/lib/nervus/registry/<package_id>.json` | UID 与授权记账，跨重启保持 |

构建：

```sh
scripts/build-image-tree.sh <输出目录> <签名私钥.pem> [ABI]
cp -r <输出目录>/* /usr/lib/nervus/system-packages/
```

产物是**解包后的目录树，不是 `.nspkg`**。后者是动态安装用的 zstd+tar 格式，
两条路不能混。

## 服务清单

| Package ID | 状态 | 职责 |
|---|---|---|
| `nervus.pkgmanagerd` | 骨架已通 | 软件安装。等接口 proto 落地 |
| `nervus.sessiond` | 计划 | 控制主体（HUMAN/AI）会话，配合 ControlLease 的 `controller_class` |
| `nervus.safety.recovery` | **阻塞** | Safety re-arm 策略。内核尚未给 `safety.Rearm()` 开对外通道 |
| `nervus.permissionui` | 计划 | 权限确认通道 |

这四个 ID 不是本仓库起的名字——它们写在内核
`internal/pkgregistry/lifecycle.go` 的**不可停用保护名单**里。改名要两边一起改。

`nervus.settings` 也在那份名单里，但它是 Compose Desktop 应用，属应用侧，不在本仓库。

## 为什么这些服务不放进 nervud

内核只提供**机制**，策略在服务里（与 Safety 的 re-arm 分工同源：机制在内核、
策略在服务）。把装包流程、会话管理这类会随产品演进的东西塞进内核，等于让
「改一次装包流程」和「改一次内核」变成同一件事。

反过来它们也拿不到内核的权力：都是普通 Package，跑在 App UID 段（20000-59999）、
同样的 systemd 沙箱里。唯一的例外是 pkgmanagerd 能连管理通道，见它的 README——
那放行的是「谁能连上」，不是「连上能做什么」。

## 签名

- 算法只接受 `ed25519`，其它一律拒绝，不做「尽量兼容」
- 签名覆盖 `"nervus-pkg-manifest-v1\x00" || manifest.json 的原始字节`
- 角色 `platform-release`（内核 `sigblock.go`：「平台：nervud + 核心系统服务」）
- `key_id` 是公钥的 `sha256:` 十六进制摘要

```sh
openssl genpkey -algorithm ed25519 -out signing/platform-release.pem
```

`scripts/sysmanifest` 用 Go 写而不是调 `nervus-packaging` 的 Kotlin `signing-lib`：
本仓库是纯 Go，`manifest.sig` 只是 JSON + Ed25519，标准库直接能产，没必要为签名
往构建里拖一条 JVM 工具链。代价是格式定义有两份，靠
`scripts/sysmanifest/manifest_test.go` 的断言锁住漂移。

### 开发期没有平台根也能跑

`LoadTrustStore` 在开发构建（`embeddedPlatformRootB64` 为空）下会失败，于是所有
系统包的 trust 降为 `Ordinary`。**这不阻断开发**：

- 包照常装载运行（`scanSystemImage` 只在 digest 不符时才硬 fail）
- 设备访问按 `Source == SourceSystemImage` 判定，与 trust 无关，仍然放开
- `perm.service.register` 原本需要 OEM trust，但已被内核的 `permission.V1GrantAll` 放行

digest **永远**是硬校验：对不上说明镜像损坏或被篡改，不降级、不放行。

## 开发环境

**必须在 Linux 或 WSL 上构建与测试。**服务本身跨平台，但它们要连的 nervud 是
`//go:build linux`，端到端验证只能在 Linux 上做。

```sh
scripts/sync-ipc.sh    # 同步 nervus-ipc 依赖
go build ./...
go test ./...
```

`nervus-ipc` 是本组织自己的模块，刚推的 commit 在 sum.golang.org 上往往还没索引，
直接 `go get` 会撞 500。脚本里带了 `GOPRIVATE`。

## 红线

1. **绝不手搓信封。** IPC 一律经 `nervus-ipc/go/sdk`，它复用冻结 proto 的生成类型。
2. **安全判定不在本仓库。** 授权、验签、裁决全在 nervud。在这里写出任何形如
   「如果……就允许」的代码都是设计错误。
3. **发 body 之前查表。** `nervus-ipc` 根 README 的「实现状态」表是唯一权威依据——
   proto 里冻结了不等于 nervud 支持，发一个未实现的 body 会被直接关连接，
   而 SDK 侧只看到「连接莫名断了」。
