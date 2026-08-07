# How to write a Nervus system server

1.教程使用go

2.禁止修改nervus-ipc，请通过ipc的附加功能实现相关功能

3.服务与服务之间可以通过ipc连接

4.系统服务可以创建一个UDS用于与被调用的应用或者服务之间进行通信

从零做出一个能被 `nervud` 拉起、能被应用调到的系统服务。

本文的每一条「注意」都对应一次真实踩过的坑，不是预防性提醒。文末的
[常见失败](#常见失败) 是按退出码和错误信息查的对照表。

---

## 0. 先搞清楚你要写的是哪一种

系统服务在 IPC 上只有两种角色，代码形态差别很大：

| | **Provider（提供方）** | **Client（消费方）** |
|---|---|---|
| 做什么 | 对外提供一个接口 | 调别人的接口 |
| 关键调用 | `RegisterEndpoint` | `ResolveEndpoint` |
| SDK 类型 | `sdk.NewServiceHost` | `sdk.Connect` |
| manifest 字段 | `components[].exports` | `components[].interfaces` |
| 本仓库范例 | `pkgmanagerd` | `safetyrecoveryd` |

一个服务可以同时是两者（既提供接口又调别人的），但先想清楚主要身份是哪个——
它决定了 manifest 怎么写，以及内核会用哪套准入规则查你。

> **提示**：如果你只是想调内核已有的接口（比如 safety），你是 Client，
> 不需要碰内核代码。如果你要提供一个**新接口**，见 [第 5 节](#5-新接口要改内核)。

---

## 1. 建目录

**根目录下每一个目录就是一个服务，代码直接放在里面。** 唯一的例外是
`scripts/`（构建工具）。

```sh
mkdir myserviced
```

命名规则：

- 目录名同时是**二进制名**、**Go 包路径**、**构建目标名**，三者不可分离
- **不得与已有服务重名**
- 惯例以 `d` 结尾（daemon）

```text
myserviced/
├── main.go            进程入口：连接、报到、Serve
├── service.go         业务实现
├── manifest.json.in   包清单模板
└── README.md          说明 + 部署路径
```

### 服务之间不能互相 import —— 由编译器保证

本仓库**每一个 `.go` 文件都是 `package main`**。Go 禁止 import `package main`，
所以服务 A 想 import 服务 B **编译期就过不去**。

这不是约定，是语言层挡着。需要共享代码时，放进 `nervus-ipc`（协议与 SDK 的家），
**不要**在本仓库开共享包。

---

## 2. 写 manifest.json.in

这是内核认识你的唯一依据。字段写错的典型症状是「服务起来了但连不上」或
「注册被拒」，而错误信息往往指向别处。

```json
{
  "schema": 1,
  "package_id": "nervus.myservice",
  "label": "My Service",
  "version": "0.1.0",
  "version_code": 1,
  "min_nervus_api": 1,
  "target_nervus_api": 1,
  "supported_abis": ["linux-arm64"],
  "permissions": [
    "perm.service.register"
  ],
  "components": [
    {
      "id": "main",
      "type": "service",
      "runtime": "native",
      "entry": "bin/myserviced",
      "launch_mode": "always-on",
      "criticality": "optional",
      "exports": [
        { "interface": "nervus.interface.my.thing", "visibility": "public" }
      ],
      "limits": { "memory_max_mb": 128, "tasks_max": 64 }
    }
  ],
  "digests": {}
}
```

### 逐字段

| 字段 | 说明 |
|---|---|
| `package_id` | 反写域名。系统服务用 `nervus.` 前缀。**它决定 UID 分配与数据目录名**，改了等于换了一个包 |
| `version_code` | 单调递增整数，升级连续性靠它，不是靠 `version` 字符串 |
| `supported_abis` | 只认 `linux-arm64` / `linux-armv7` / `linux-x86_64` 三个 canonical token。不接受 `arm64-v8a`、`aarch64`，也**不做归一化** |
| `permissions` | **申请了才可能拿到。** 见下方警告 |
| `components[].id` | 必须与代码里的 `componentID` 常量一致 |
| `components[].entry` | 相对包根的路径。native 必须是包内 ELF |
| `launch_mode` | `always-on` 随内核启动拉起；`on-demand` 被解析时拉起；`manual` 只能显式启动。`type: "app"` 不能 `always-on` |
| `criticality` | `optional` 崩了只重启；`vital` 崩溃会升级到 Safety。**先用 `optional`** |
| `exports` | Provider 用。`visibility: "public"` 任何包可解析，`"private"` 只有同签名的包可以 |
| `interfaces` | Client 用。声明你要**消费**哪些接口 |
| `digests` | 留空 `{}`，由 `sysmanifest` 在构建期填 |

> ⚠️ **`permissions` 必须显式声明,即使 v1 全授予**
>
> 内核当前的 `permission.V1GrantAll = true` 让「申请即授予」、跳过运行期确认，
> 但**仍然要求 manifest 里声明过**。没声明的权限拿不到，症状是
> `missing permission perm.xxx`。
>
> Provider 需要 `perm.service.register`（或 `perm.service.register.private`）。
> Client 需要目标接口对应的权限——查
> `nervud/internal/endpoint/catalog.go`。

---

## 3. 写 main.go

### 两个必须对齐的常量

```go
// 必须与 manifest.json 的 components[].id 一致。
//
// 它是握手时的 declared_component_id：nervud 会用对端 PID → cgroup →
// systemd unit → 启动记录去核对。填错不会让你冒充别人，只会让你连不上——
// 而错误是 UNAUTHENTICATED，看起来像权限问题。
const componentID = "main"

// 必须与 manifest 的 exports[].interface（或 interfaces[]）一致。
const interfaceID = "nervus.interface.my.thing"
```

### Provider 骨架

```go
host, err := sdk.NewServiceHost(sdk.Config{
    SockPath:    sockPath,
    ComponentID: componentID,
    Log:         log,
})
if err != nil {
    return fmt.Errorf("连接控制面: %w", err)  // 直接失败退出，不要自己重试
}
defer func() { _ = host.Close() }()

// 【必须在报到之前把 handler 全部注册完】
registerHandlers(host, svc, log)

epID, err := host.RegisterEndpoint(ctx, sdk.RegisterRequest{
    InterfaceID: interfaceID,
    Major:       1,
})
```

> ⚠️ **handler 必须在 `RegisterEndpoint` 之前注册完**
>
> 报到成功那一刻 nervud 就可能转发 Dispatch。此时还没注册的 method 会被回
> `NOT_FOUND`——调用方会以为**这个方法不存在**，而不是「服务还没准备好」。
> 这种错误很难往回追。

> ⚠️ **连不上控制面就直接退出，不要写重试循环**
>
> systemd 会按退避重启，nervud 的监督链会在反复崩溃时按 `criticality` 处置。
> 自己重试会掩盖「nervud 没起来」这个事实，让本进程看起来健康。

`RegisterRequest` 的两个留空字段：

- **`ResourceHandle`** 留空 = 未指定。内核只在非空时校验它必须是 Resource
  Registry 里的已知句柄。不绑物理资源的接口**填了反而会被
  `INVALID_ARGUMENT` 拒掉**
- **`SchemaHash`** 留空。内核 v1 **只记录不比对**（尚无权威 schema Registry）。
  比对开启后必须填真实 hash

### Client 骨架

```go
c, err := sdk.Connect(sdk.Config{ComponentID: componentID, Log: log})
if err != nil {
    return err
}
defer func() { _ = c.Close() }()

ep, err := c.ResolveEndpoint(ctx, sdk.ResolveRequest{
    InterfaceID: interfaceID, MinMajor: 1, MaxMajor: 1,
})
```

> ⚠️ **Resolve 要重试**
>
> 组件由 service 模块**并发拉起，没有启动顺序保证**。第一次 Resolve 很可能
> 撞在目标服务 `RegisterEndpoint` 之前。失败就退出会让 systemd 重启你，
> 日志里堆一串 `NOT_FOUND`——看起来像故障，其实只是竞态。
>
> 退避重试 10~20 次、每次 500ms 是合适的量级。

### 优雅退出

```go
// 主动撤下 endpoint，让 nervud 把这件事当可预期事件处理
// （向持有 binding 的调用方发 EndpointDied{SERVICE_SHUTTING_DOWN}），
// 而不是与「进程崩了」混为一谈。
_ = host.UnregisterEndpoint(unregCtx, epID, true)
```

### 日志写 stderr

systemd 会收进 journal，不需要自己管轮转。

```go
slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
```

---

## 4. 注册进构建脚本

`scripts/build-image-tree.sh` 的 `SERVICES` 数组，加一行 `<package_id>:<目录名>`：

```sh
SERVICES=(
	"nervus.pkgmanagerd:pkgmanagerd"
	"nervus.safety.recovery:safetyrecoveryd"
	"nervus.myservice:myserviced"        # ← 新增
)
```

---

## 5. 新接口要改内核

**如果你只是消费已有接口，跳过本节。**

提供一个**新接口**时，光在自己的 manifest 里 `exports` 是不够的——调用方能不能
解析到你，由内核的两张编译期表决定。这两张表在 `nervud` 仓库：

### 5.1 接口 → 权限门槛

`nervud/internal/endpoint/catalog.go`：

```go
{InterfaceID: "nervus.interface.my.thing", RequiredPermission: "perm.my.thing"},
```

> ⚠️ **漏登记不是 fail-closed，是 fail-open**
>
> `resolve.go` 在目录未命中时把 `requiredPermission` 取**空串**，也就是
> **不设门槛**——任意应用都能解析到你的服务。
>
> 这个方向是有意的（私有接口不该被要求登记进内核编译期表），但代价是
> 漏登记一个标准接口**不会有任何症状**，直到有人发现门是开的。
>
> `nervus.interface.pkg.manager` 就漏过一次：任意 Ordinary 应用都能让
> pkgmanagerd 装包，而装包能往系统里放任意可执行文件。

### 5.2 权限定义

`nervud/internal/permission/catalog.go`：

```go
{
    ID:          "perm.my.thing",
    MinTrust:    identity.TrustOrdinary,   // Ordinary / OEM / Platform
    Mode:        GrantInstall,             // GrantInstall / GrantUser / GrantSignature
    Description: "...",
},
```

| `Mode` | 何时用 |
|---|---|
| `GrantInstall` | 安装时授予，不打扰用户 |
| `GrantUser` | 危险操作，用户必须当场知情（装包、运动控制） |
| `GrantSignature` | 只给特定 trust 的包 |

再危险的，加 `RequireSignerRole: "platform-release"`——只给平台发布密钥签的包
（`perm.safety.rearm`、`perm.authority.reboot` 是这一档）。

### 5.3 proto 定义

接口的消息与 method ID 定义在 `nervus-ipc`。**method ID 常量必须从生成代码取，
不要在本地重抄一份**——抄一份的代价不是重复，是它会悄悄过期：

```go
// 对的
methodInstall = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_INSTALL)

// 错的
const methodInstall = 1
```

`nervud`、`nervus-system-server`、`nervus-ipc` 三者**必须指向同一个 commit**，
否则两侧对同一份 Envelope 的理解会悄悄分叉——而两边都能编译、都能跑。
用各自仓库的 `scripts/sync-ipc.sh` 同步。

---

## 6. 构建、签名、部署

### 发布形态是二进制，不是源码

```sh
scripts/build-image-tree.sh ./out signing/platform-release.pem linux-arm64
```

这一步做四件事：交叉编译（`CGO_ENABLED=0` 静态链接）→ 算 digest → 填 manifest
→ Ed25519 签名。

产物：

```text
out/nervus.myservice/
├── bin/myserviced      静态二进制
├── manifest.json       digests 已填好
└── manifest.sig        platform-release 角色签名
```

目标机上**不需要** Go 工具链、源码、网络。这也是为什么 digest 与签名必须在
构建期算好——目标机没有任何东西可以拿来重新计算。

### 部署路径

```sh
cp -r ./out/* <镜像挂载点>/usr/lib/nervus/system-packages/
```

**目录名必须等于 `package_id`。** 内核按 manifest 内容认包，目录名只是容器，
但两者不一致会让人排查时看着目录名找错包。

内核启动扫描 glob 的是 `/usr/lib/nervus/system-packages/*/manifest.json`，
**没有版本子目录**——系统包跟随整镜像 OTA，不存在多版本并存。

> ⚠️ **系统镜像包与动态安装包是两条路，不能混**
>
> | | 系统镜像包 | 动态安装包 |
> |---|---|---|
> | 位置 | `/usr/lib/nervus/system-packages/<pkg>/` | `/var/lib/nervus/packages/<pkg>/<version>/` |
> | 形态 | **解包后的目录树** | `.nspkg`（zstd + tar） |
> | 签名角色 | `platform-release` | `developer`（自锚定） |
> | 设备访问 | 可以碰 `/dev` | 拿不到 |

### 签名角色选错的后果

`platform` / `oem` 角色要在 trust bundle 里查得到 key_id 才算数。**开发构建
没有内嵌 platform 根**，于是一律 fail-closed：

```
signer key not trusted for its role: "sha256:..."
```

`developer` 角色是**自锚定**的——公钥内嵌在签名块里，验签方不查信任库。
第三方应用走这条路。

---

## 7. 内核为你做了什么

服务启动前，内核在**启动扫描**里自动补齐运行前置（`pkgregistry`）：

| 步骤 | 产物 | 少了会怎样 |
|---|---|---|
| 分配稳定 UID | 20000–59999，**永不回收** | — |
| `EnsureAppUser` | `/etc/passwd` 里的 `nervus-app-<uid>` | `217/USER` |
| `CreatePrivateDataDirectory` | `/var/lib/nervus/package-data/<pkg>`，0700 | `226/NAMESPACE` |
| `arbitrateSystemGrants` | `GrantedPermissions` | `missing permission` |

然后经 `authority` → systemd 拉起一个瞬态 unit：

```
nervus-<package_id>-<component_id>.service
```

沙箱固定包含：`NoNewPrivileges`、`ProtectSystem=strict`、
`SystemCallFilter=@system-service`、`BindsTo=nervud.service`（nervud 死了你也停）。

**可写的只有自己的数据目录。** 别的位置即便属主与权限都对，写进去也是
`read-only file system`——**权限与挂载是两道独立的门**。

---

## 8. 调试

### 起 nervud（开发机）

```sh
systemd-run --unit=nervud --collect \
  --property=StandardOutput=append:/tmp/nervud.log \
  --property=StandardError=append:/tmp/nervud.log \
  ./nervud -dev-skip-preflight -dev-allow-sched-degrade -log-level debug
```

> ⚠️ **必须以 systemd unit 运行,不能裸跑二进制**
>
> 组件的瞬态 unit 带 `BindsTo=nervud.service`。裸跑时那个 unit 不存在，
> `StartTransientUnit` 会以 `Unit nervud.service not found` 失败。

### 看你的服务

```sh
journalctl -u nervus-nervus.myservice-main.service -n 50
systemctl show nervus-nervus.myservice-main.service --property=ActiveState,SubState
```

### 看内核怎么看你

```sh
grep -E "handshake complete|RegisterEndpoint|ResolveEndpoint|provision" /tmp/nervud.log
nervusctl list
```

---

## 常见失败

按你看到的**症状**查。

### systemd 退出码

| 症状 | 原因 | 修 |
|---|---|---|
| `217/USER` | UID 没进 `/etc/passwd`，NSS 解析不了 | 内核 `provision.go` 应自动建。没建说明启动扫描没跑到 |
| `226/NAMESPACE` | 私有数据目录不存在，`WorkingDirectory` 指向它 | 同上 |
| `203/EXEC` | 找不到可执行文件 | 检查 `entry` 路径；确认代码根对不对（系统包在 `/usr/lib/nervus/system-packages/`，**没有版本子目录**） |
| `Unit ... was already loaded` | 上一次的瞬态 unit 没回收 | 内核已加 `CollectMode=inactive-or-failed` + `ResetFailedUnit`。手动清：`systemctl reset-failed <unit>` |

### 握手与注册

| 症状 | 原因 |
|---|---|
| `UNAUTHENTICATED` | `componentID` 与 manifest 的 `components[].id` 对不上。**不是权限问题** |
| `interface not declared in manifest exports` | `interfaceID` 与 manifest 的 `exports[].interface` 对不上 |
| `missing permission perm.service.register` | manifest 的 `permissions` 里没申请 |
| `RegisterEndpoint` 返回 `INVALID_ARGUMENT` | `ResourceHandle` 填了但不在 Resource Registry 里。不绑资源就**留空** |

### 解析与调用

| 症状 | 原因 |
|---|---|
| Resolve `NOT_FOUND` | 目标服务还没 `RegisterEndpoint`（竞态）→ **重试**；或它根本没起来 → 看它的 journal |
| Resolve `PERMISSION_DENIED` | 缺接口对应的权限。查 `nervud/internal/endpoint/catalog.go` |
| Resolve `FAILED_PRECONDITION` | 版本协商失败（`MinMajor`/`MaxMajor` 不含服务注册的 major），或 resource 找不到 |
| 调用返回 `NOT_FOUND` 但方法明明有 | handler 注册晚于 `RegisterEndpoint` |

### 文件系统

| 症状 | 原因 |
|---|---|
| `permission denied` 写自己数据目录之外 | 沙箱只给数据目录可写。**这是设计** |
| `read-only file system` 而属主权限都对 | `ProtectSystem=strict`。属主是一道门，挂载是另一道，**两道都要过** |

### 签名与安装

| 症状 | 原因 |
|---|---|
| `signer key not trusted for its role` | 用 `platform` 角色签了，但没有内嵌 platform 根（开发构建）。第三方包用 `developer` 角色 |
| `system package signature not verified; downgrading to Ordinary` | 开发构建的**正常现象**。系统包验不过降级为 Ordinary 但仍装载 |
| `digest mismatch` | 二进制改了但没重跑 `sysmanifest`。**改了二进制必须重签** |

---

## 检查清单

新服务上线前逐条过：

- [ ] 目录名不与已有服务冲突
- [ ] `componentID` 常量 == manifest `components[].id`
- [ ] `interfaceID` 常量 == manifest `exports[].interface` / `interfaces[]`
- [ ] `permissions` 里申请了需要的权限（**v1 全授予也要声明**）
- [ ] handler 在 `RegisterEndpoint` **之前**全部注册完
- [ ] Client 的 Resolve 有重试
- [ ] 退出时 `UnregisterEndpoint`
- [ ] 加进 `scripts/build-image-tree.sh` 的 `SERVICES`
- [ ] 新接口：改了 nervud 的 `endpoint/catalog.go` 与 `permission/catalog.go`
- [ ] 三个仓库指向同一个 nervus-ipc commit
- [ ] README 里写了部署路径
- [ ] `go vet ./... && go test ./... && go test ./... -race`

---

## 延伸阅读

| 想知道 | 看 |
|---|---|
| 协议字段与 Envelope 结构 | `nervus-ipc/README.md`、`proto/nervus/ipc/v1/envelope.proto` |
| 内核已知缺口 | `nervud/README.md` 的「已知缺口」 |
| endpoint 注册的 7 步准入 | `nervud/internal/endpoint/register.go` |
| 沙箱属性全集 | `nervud/internal/authority/systemd/props.go` |
| 包的安装事务 | `nervud/internal/pkgregistry/install.go` |
