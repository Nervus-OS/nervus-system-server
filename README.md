# nervus-system-server

System-Server for Nervus OS —— Nervus OS 的系统服务仓库。

## 1. 这个仓库装什么

1. **能力提供者**：把硬件或平台能力做成接口，供应用与其它服务调用。
2. **需要常驻后台的系统服务**。

系统服务是**一个二进制文件**，推荐用 Go 写（本仓库全部是 Go）。写完打包、签名，装进系统镜像。

**服务与内核之间一律走 proto IPC**（`nervus-ipc/sdk`）——身份、注册、解析、权限、数据面授权都在这条线上，没有第二条路。

**服务与服务之间、服务与应用之间的数据，走自己的协议**。内核只负责证明你是谁、允不允许、能开多大的管子；管子里流什么它不看也不解析。

> **要新写一个服务？** 看 **[WRITING-A-SERVICE.md](WRITING-A-SERVICE.md)** ——
> 从建目录到部署上机的完整流程，附按症状查的失败对照表。

## 2. 三条线

```text
控制面（proto IPC，必经内核）
  App / 服务  ──Envelope──▶  nervud  ──Dispatch──▶  服务
              握手 · 注册 · 解析 · 权限 · 租约 · 数据面授权

数据面（内核只签发授权，不看内容）
  服务  ◀──Transfer（不透明字节，两端自己的协议）──▶  App / 服务

文件面（内核只管挂载与属主）
  /run/nervus/shared/<pkg>/      tmpfs，运行期交换
  /var/lib/nervus/shared/<pkg>/  磁盘，持久
```

## 3. 布局：一个服务一个目录

**根目录下每一个目录就是一个系统服务**，代码直接放在里面。唯一的例外是
`scripts/`（构建工具）。

```text
nervus-system-server/
├── pkgmanagerd/            ← 服务
│   ├── main.go             进程入口
│   ├── service.go          业务实现
│   ├── handlers.go         method 分派
│   ├── adminclient.go      管理通道客户端
│   ├── unpack.go           .nspkg 解包 + tar-slip 防护
│   ├── providergen/        ← 产出 Provider 契约（见第 4 节）
│   ├── manifest.json.in    包清单模板
│   └── README.md
├── safetyrecoveryd/        ← 服务（Client 范例）
└── scripts/                构建工具，不是服务
    ├── build-image-tree.sh
    ├── sync-ipc.sh
    └── sysmanifest/        digest 计算 + manifest 填充 + Ed25519 签名
```

**服务目录名不得冲突**：它同时是二进制名、Go 包路径、以及 `build-image-tree.sh`
里的构建目标。

### 服务之间不能互相 import —— 由编译器保证

本仓库**每一个 `.go` 文件都是 `package main`**。Go 禁止 import `package main`，
所以服务 A 想 import 服务 B **编译期就过不去**。

这不是靠约定或 code review 维持的，是语言层挡着。需要共享代码时，放进
`nervus-ipc`（协议与 SDK 的家），**不要**在本仓库开共享包。

> `providergen/` 是子目录里的另一个 `package main`，同样 import 不进来。

## 4. Provider 契约：导出接口的包必须带

**这是本仓库最容易踩的一条。**

内核的 `loadRequiredProviderArtifacts` 对「manifest 有 `exports` 却没有 `provider` 段」
直接返回 `ErrProviderArtifactsRequired`。**没有任何例外通道**——曾经有一条只给
`nervus.pkgmanagerd` 的兼容桥，已随本仓库的打包链落地而整段移除。

契约由服务自己的 `providergen/` 产出两个文件：

```text
provider.binpb   ProviderDescriptor 的确定性 protobuf bytes
schemas.binpb    InterfaceSchemaBundleSet 的确定性 protobuf bytes
```

`build-image-tree.sh` 在编译之后、`sysmanifest` 之前跑它，两个文件因此被算进
`digests`，manifest 里写：

```json
"provider": {
  "descriptor": "provider.binpb",
  "schemas": "schemas.binpb"
}
```

### 为什么每个服务各有一份，而不是打包器统一生成

生成 schema 需要 import 该接口的生成代码。若由 `sysmanifest` 统一做，它就得 import
全部接口的生成包——那等于把「加一个能力」变成「改打包工具」，正是数据驱动
Catalog 要消灭的耦合。

### 能力接口可以完全没有 protobuf 消息

`ProvidedInterfaceVersion.methods` 允许内联方法元数据（method_id、权限、风险、
Transfer 预算），此时**不需要 schema bundle，也不需要任何 `.proto` 消息**。

摄像头帧、麦克风采样、雷达点云本来就走 Transfer 的不透明字节——为它们编一套永远
用不上的 Request/Response，只是让「加一个能力」平白多一份 `.proto` 要维护、要生成、
要在多方之间对齐。

内核真正需要知道的只有三件事：**谁在调、允不允许、能开多大的管子**。这三件都在
`MethodMeta` 里，与消息形状无关。

契约身份仍然存在，只是由 `registry.MethodsHash(methods)` 算出而不是 schema 哈希。
因此「多个厂商实现同一个标准接口必须逐字节一致」这条保证**完全保留**。

## 5. 权限与信任

**声明了才可能拿到。** manifest 的 `permissions` 是申请，实际授予由内核按
来源 + 信任 + 签名角色 + GrantMode 求交（`permission.Registry.IntersectAt`）。

几个本仓库会用到的：

| 权限 | 门槛 | 用途 |
|---|---|---|
| `perm.service.register` | OEM 信任 + PRIVILEGED | 注册公共接口 |
| `perm.service.register.private` | Ordinary + NORMAL | 只在包内可见的接口 |
| `perm.pkg.admin` | Platform 信任 + SYSTEM_ONLY + `platform-release` 签名 | 连管理通道、拿 staging 可写 |
| `perm.storage.shared` | Ordinary + NORMAL | 服务间共享区 |
| `perm.storage.user` | Ordinary + USER_CONSENT | 用户文档区 |

> **内核不认识任何具体的 Package ID。** 装包服务能连管理通道，靠的是它持有
> `perm.pkg.admin`，不是因为它叫 `nervus.pkgmanagerd`。`main.go` 里那个包名常量
> 已经删掉。唯一剩下的包名硬编码是 `pkgregistry/lifecycle.go` 的不可停用保护名单，
> 那管的是「用户不能停用关键组件」，不是授予能力。

### 开发构建：必须开 `-dev-trust-system-packages`

生产二进制用 `-ldflags` 注入平台根公钥；开发构建没有，`LoadTrustStore` 失败，
于是每个系统包 fail-closed 到 `Ordinary`——而 `perm.service.register` 要 OEM。
**结果是开发机上任何导出公共接口的服务都注册不了。**

```sh
nervud -dev-skip-preflight -dev-allow-sched-degrade -dev-trust-system-packages
```

这个开关只放松一件事：**这把签名密钥是否由内嵌平台根授权**。签名本身仍逐条做
真实 Ed25519 验签，key_id 仍必须等于公钥 sha256，digest 仍是硬校验。改了二进制、
改了 manifest、想冒充某个 key_id——三种都照样拒。

## 6. 服务间共享区

```text
/run/nervus/shared/<package_id>/      tmpfs，运行期交换
/var/lib/nervus/shared/<package_id>/  磁盘，持久
```

属主 = 包的 UID，模式 **0755**。持有 `perm.storage.shared` 的包，**自己那个子目录可写，
别人的目录可读**。

**按需创建**：只有拿到 `perm.storage.shared` 的包才有这两个目录。多数服务用不上
共享区，给每个包都建等于在 tmpfs 上白占一批 inode，还让目录列表与「谁真的在用」
对不上。

### 五个刻意的决定

**① 根由 nervud 独占，包只写自己的子目录。**

给包「根可写」不只是绕开「一个包一个目录、属主即写权」这条结构，更直接的风险是
**抢先占名**：恶意包先建 `nervus.camerad/` 并占为己有，camerad 起来时发现自己的
路径写不进去。sticky 位防的是「删别人的」，防不了这个。根保持 nervud 独占可写，
这条攻击就不存在。

> Android 走得更远：应用**根本没有 `/tmp`**，`java.io.tmpdir` 指向应用自己的私有
> 目录。共享 `/tmp` 是符号链接攻击与抢占的经典温床。我们保留一个共享根是因为它
> 有 memfd 替代不了的用途（见第 7 节的 UDS），但根必须是内核独占写。

**② 0755，不是 0700 也不是 0777。**
0700 退化成第二个私有目录，共享就没了；0777 则任何包都能篡改别人放出来的配置和
模型。0755 恰好是「谁都能读、只有属主能写」——不需要把写权限敞开给所有人。
按包细粒度授予写权限（ACL）留待 v2。

**③ 与用户文档区（`perm.storage.user`）刻意分开。**
那是「用户的文档」，语义面向 App 与文件选择器，因此是 `USER_CONSENT` +
`PRIVACY_SENSITIVE`。两个服务想放个中间文件，不该变成「要用户同意访问他的文档」。

**④ tmpfs 那个根每次开机重建。**
私有数据目录只在首装时建，但 `/run` 重启即空——不在启动扫描里补齐的话，服务第一次
写就 `ENOENT`。

**⑤ 判据是裁决后的 `GrantedPermissions`，不是 manifest 里的申请。**
按申请建目录等于让任何包写一行 manifest 就占一个位置。

### ⚠ 边界：敏感数据不能放这里

跨包读隔离在本系统里**只靠数据目录的 0700** 实现（组件沙箱的
`InaccessiblePaths` 只列了 registry 目录）。因此 0755 的共享区是**全系统可读的**。

**共享区只放「拿到 `perm.storage.shared` 就有资格看」的东西。** 摄像头帧这类需要
真实权限门槛的数据必须走 Transfer 的内存句柄——句柄本身就是凭证，没有文件系统
路径可绕。把它们写进共享区等于让任何应用绕过 `perm.camera.capture` 直接读文件。

### 服务自建 UDS 放这里

想用自己的协议跟另一个服务通信，把 socket 建在 `/run/nervus/shared/<你的包>/` 下：
目录 0755，别的服务读得到路径也连得上。这是「服务与服务之间用自己的协议」最直接的
落地方式，不需要 memfd。

**UDS 适合控制与小消息；大流量走第 7 节的 Transfer。**

## 7. 高吞吐数据：走 Transfer，不要走文件

控制面的 Envelope 不承载大数据。方法在 `MethodMeta.transfer` 里声明预算之后，
Provider 才有资格创建 Transfer；nervud 按权威元数据、调用者权限、连接预算和 route
生命周期收紧方向、模式、包大小与速率，签发两张短期一次性句柄。

两端附着到内核独占的 Transfer socket 之后，**里面流什么由两端自己决定**——
内核只做分帧与限速，不解析内容。

> **不要用磁盘文件传视频流。** 1080p30 裸流约 93 MiB/s，持续写 eMMC 是在烧闪存
> 寿命。运行期大流量要么走 Transfer，要么落在 tmpfs 的共享区。

## 8. 部署位置

系统包装在**只读镜像区**：

```text
/usr/lib/nervus/system-packages/
├── nervus.pkgmanagerd/
│   ├── manifest.json
│   ├── manifest.sig
│   ├── provider.binpb
│   ├── schemas.binpb
│   └── bin/pkgmanagerd
└── <下一个服务>/
```

目录名必须等于 `manifest.json` 里的 `package_id`。内核的启动扫描是
`Glob("/usr/lib/nervus/system-packages/*/manifest.json")`，**没有版本子目录**——
系统包跟随整镜像 OTA，不存在多版本并存。

**装进去靠刷镜像或 OTA，不能用 `nervusctl install`**——那条路只接受
`SourceDynamicInstall`，系统包走它会被直接拒绝（`ErrSystemPackageImmutable`）。

运行期路径由内核分配，不需要手工建：

| 路径 | 用途 |
|---|---|
| `/var/lib/nervus/package-data/<pkg>/` | 私有数据目录，属主为包 UID，`0700` |
| `/run/nervus/shared/<pkg>/` | 共享区（tmpfs），`0755` |
| `/var/lib/nervus/shared/<pkg>/` | 共享区（磁盘），`0755` |
| `/var/lib/nervus/registry/<pkg>.json` | UID 与授权记账，跨重启保持 |

构建：

```sh
scripts/build-image-tree.sh <输出目录> <签名私钥.pem> [ABI]
cp -r <输出目录>/* /usr/lib/nervus/system-packages/
```

产物是**解包后的目录树，不是 `.nspkg`**。后者是动态安装用的 zstd+tar 格式，
两条路不能混。

**发布形态是二进制，不是源码。** 目标机不需要 Go 工具链、不需要源码、不需要联网——
只有静态链接的二进制（`CGO_ENABLED=0`）加元数据。这也是 digest 与签名必须在构建期
算好的原因：目标机没有任何东西可以拿来重新计算。

## 9. 启动顺序：随机，服务必须自己扛住

`service.Manager.Start()` 遍历的是一个 Go map，而 **Go 运行时刻意随机化 map 迭代
顺序**——系统服务的启动顺序不是「碰巧无序」，而是每次开机都不同。内核也没有依赖
声明机制（没有 `After=`）。

因此每个服务的正确写法是：**连不上控制面就直接失败退出**，让 supervisor 按指数退避
重启。不要自己写重试循环——那会掩盖「nervud 没起来」这个事实，让进程看起来健康。

崩溃预算烧不完：

```
退避序列 1s → 2s → 4s → 8s，崩溃时刻 ≈ t = 0, 1, 3, 7, 15
熔断阈值 = 10 秒滑动窗口内 5 次

t=7   第 4 次，窗口 [-3,7] 内有 0,1,3,7 → 4 < 5  ✅
t=15  第 5 次，窗口 [5,15] 内只有 7,15  → 2 < 5  ✅
```

**但 `criticality: vital` 的服务要当心**：真熔断了会触发 Safety 锁存让整机停下来。
除非这个服务停了机器就不该动，否则用默认的 `optional`。

### 一个坏包不会拖垮启动

Catalog 层的冲突（资源多管理者、接口契约不一致、命名空间越权）以前会让整个启动
失败。现在启动扫描会**按 source 隔离肇事的那个包**、大声审计、继续启动。

安装路径**不走隔离**，仍是全有全无：新包有冲突就拒绝新包，现有系统一点不动。

## 10. 服务清单

| Package ID | 状态 | 职责 |
|---|---|---|
| `nervus.pkgmanagerd` | 骨架已通 | 软件安装 |
| `nervus.safety.recovery` | 可实现 | Safety re-arm 策略（内核 builtin 已开放 `REARM`） |
| `nervus.camerad` | 计划 | 摄像头通用能力 + V4L2 兼容层 |
| `nervus.sessiond` | 计划 | 控制主体（HUMAN/AI）会话 |
| `nervus.permissionui` | 计划 | 权限确认通道 |

这些 ID 不是本仓库起的名字——它们写在内核 `internal/pkgregistry/lifecycle.go` 的
**不可停用保护名单**里。改名要两边一起改。

`nervus.settings` 也在那份名单里，但它是 Compose Desktop 应用，属应用侧。

## 11. 为什么这些服务不放进 nervud

内核只提供**机制**，策略在服务里（与 Safety 的 re-arm 分工同源：机制在内核、
策略在服务）。把装包流程、会话管理这类会随产品演进的东西塞进内核，等于让
「改一次装包流程」和「改一次内核」变成同一件事。

反过来它们也拿不到内核的权力：都是普通 Package，跑在 App UID 段（20000-59999）、
同样的 systemd 沙箱里。

## 12. 签名

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

## 13. 开发环境

**必须在 Linux 或 WSL 上构建与测试。** 服务本身跨平台，但它们要连的 nervud 是
`//go:build linux`，端到端验证只能在 Linux 上做。

```sh
scripts/sync-ipc.sh    # 同步 nervus-ipc 依赖，必须与 nervud 指向同一 commit
go build ./...
go vet ./...
go test ./...
```

`nervus-ipc` 是本组织自己的模块，刚推的 commit 在 sum.golang.org 上往往还没索引，
直接 `go get` 会撞 500。脚本里带了 `GOPRIVATE`。

## 14. 红线

1. **绝不手搓信封。** 与内核的一切通信经 `nervus-ipc/sdk`，它复用冻结 proto 的
   生成类型。
2. **安全判定不在本仓库。** 授权、验签、裁决全在 nervud。在这里写出任何形如
   「如果……就允许」的代码都是设计错误。
3. **发 body 之前查表。** `nervus-ipc` 根 README 的「实现状态」表是唯一权威依据——
   proto 里冻结了不等于 nervud 支持，发一个未实现的 body 会被直接关连接，
   而 SDK 侧只看到「连接莫名断了」。
4. **敏感数据不进共享文件区。** 那里是全系统可读的（见第 6 节的边界说明）。
