# 写一个 Nervus 系统服务

从零做出一个能被 `nervud` 拉起、能被应用调到的系统服务。

## 四条规矩

1. **教程使用 Go。**
2. **不要改 `nervus-ipc`。** 加一个能力不需要动协议——用 ProviderArtifacts 随包
   声明即可（第 5 节）。IPC 只是线；扩展要改线，说明方向错了。
3. **服务与服务之间可以通过 IPC 互相连接**（Resolve + Request，与应用调服务同路）。
4. **与内核一律 proto IPC；与对端的数据用自己的协议。** 内核负责证明你是谁、
   允不允许、能开多大的管子；管子里流什么它不看（第 7 节）。

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
| **是否需要 Provider 契约** | **必须有** | 不需要 |
| 本仓库范例 | `pkgmanagerd` | `safetyrecoveryd` |

一个服务可以同时是两者，但先想清楚主要身份是哪个——它决定了 manifest 怎么写，
以及内核会用哪套准入规则查你。

`camerad` 是同时兼两者的范例：它对外提供两个接口（采集 + 配置），同时又要
去 `ResolveEndpoint` 调内核的 Transfer Control 建数据面管子。它还示范了三件
只有它做过的事——**一个包导出多个接口**、**资源声明来自数据文件而非代码常量**、
**推送事件**。

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
├── providergen/       Provider 契约生成器（Provider 才需要，见第 5 节）
│   └── main.go
├── manifest.json.in   包清单模板
└── README.md          说明 + 部署路径
```

### 服务之间不能互相 import —— 由编译器保证

本仓库**每一个 `.go` 文件都是 `package main`**。Go 禁止 import `package main`，
所以服务 A 想 import 服务 B **编译期就过不去**。

这不是约定，是语言层挡着。需要共享代码时，放进 `nervus-ipc`（协议与 SDK 的家），
**不要**在本仓库开共享包。

> `providergen/` 是子目录里的另一个 `package main`，同样 import 不进来。

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
  "provider": {
    "descriptor": "provider.binpb",
    "schemas": "schemas.binpb"
  },
  "digests": {}
}
```

### 逐字段

| 字段 | 说明 |
|---|---|
| `package_id` | 反写域名。系统服务用 `nervus.` 前缀。**它决定 UID 分配与数据目录名**，改了等于换了一个包 |
| `version_code` | 单调递增整数，升级连续性靠它，不是靠 `version` 字符串 |
| `supported_abis` | 只认 `linux-arm64` / `linux-armv7` / `linux-x86_64` 三个 canonical token。不接受 `arm64-v8a`、`aarch64`，也**不做归一化** |
| `permissions` | **申请了才可能拿到**，见下方警告 |
| `components[].id` | 必须与代码里的 `componentID` 常量一致 |
| `components[].entry` | 相对包根的路径。native 必须是包内 ELF |
| `launch_mode` | `always-on` 随内核启动拉起；`on-demand` 被解析时拉起；`manual` 只能显式启动。`type: "app"` 不能 `always-on` |
| `criticality` | `optional` 崩了只重启；`vital` 崩溃会升级到 Safety。**先用 `optional`** |
| `exports` | Provider 用。`visibility: "public"` 任何包可解析，`"private"` 只有同签名的包可以 |
| `interfaces` | Client 用。声明你要**消费**哪些接口 |
| `provider` | **有 `exports` 就必须有**，见第 5 节 |
| `digests` | 留空 `{}`，由 `sysmanifest` 在构建期填 |

> ⚠️ **`permissions` 必须显式声明**
>
> manifest 里的是**申请**，实际授予由内核按「来源 + 信任 + 签名角色 + GrantMode」
> 求交（`permission.Registry.IntersectAt`）。没申请的一定拿不到，症状是
> `missing permission perm.xxx`。
>
> 申请了也不一定拿到：`perm.service.register` 要求 OEM 信任 + 系统镜像来源；
> `perm.pkg.admin` 还要 `platform-release` 签名角色。**开发构建下这些会全部被拒**，
> 除非开 `-dev-trust-system-packages`（见第 8 节）。

> ⚠️ **`exports` 与 `provider` 必须同时出现**
>
> 内核对「有 `exports` 却没有 `provider`」直接返回 `ErrProviderArtifactsRequired`。
> 没有任何例外通道——曾经有一条只给 `nervus.pkgmanagerd` 的兼容桥，已整段移除。

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
    SchemaHash:  contractHash,  // 必须与 provider.binpb 里声明的一致
})
```

> ⚠️ **handler 必须在 `RegisterEndpoint` 之前注册完**
>
> 报到成功那一刻 nervud 就可能转发 Dispatch。此时还没注册的 method 会被回
> `NOT_FOUND`——调用方会以为**这个方法不存在**，而不是「服务还没准备好」。

> ⚠️ **`SchemaHash` 现在是必填且被逐字节比对**
>
> 它必须等于你的 `providergen` 写进 descriptor 的那个值。曾经有一条允许
> `nervus.pkgmanagerd` 交空 hash 的兼容桥，已经移除——**所有 Provider 一视同仁**。
> 对不上是 `FAILED_PRECONDITION`。

> ⚠️ **连不上控制面就直接退出，不要写重试循环**
>
> systemd 会按退避重启，nervud 的监督链会在反复崩溃时按 `criticality` 处置。
> 自己重试会掩盖「nervud 没起来」这个事实，让本进程看起来健康。

`RegisterRequest.ResourceHandle` 留空 = 未指定。内核只在非空时校验它必须是
Resource Registry 里的已知句柄。不绑物理资源的接口**填了反而会被
`INVALID_ARGUMENT` 拒掉**。

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

### 按语义选设备，不要按名字

绑物理资源的接口，用 `ResourceSelector` 的标签而不是硬编码 role：

```go
ep, err := c.ResolveEndpoint(ctx, sdk.ResolveRequest{
    InterfaceID: "nervus.interface.camera", MinMajor: 1, MaxMajor: 1,
    Selector: &ipcv1.ResourceSelector{
        Type:   "nervus.resource.camera",
        Labels: map[string]string{"nervus.camera.facing": "front"},
        // 不写 Policy = REQUIRE_UNIQUE：命中多个时报错而不是随便给一个
    },
})
```

`stable_role` 是**板级配置的产物**（这块板上前视摄像头叫 `cam.front` 还是
`camera0`），依赖它换块板就得改代码。

> ⚠️ **多候选默认 fail closed**
>
> 不指定 `Policy` 等于 `REQUIRE_UNIQUE`：命中多个直接失败。要系统替你挑，必须
> 显式写 `SYSTEM_PREFERRED`。
>
> 这是有意的——「我要一个摄像头，系统随便给了一个」在候选里混着前视和后视
> （或左臂和右臂）时，比一个明确的错误危险得多。

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
	"nervus.camerad:camerad"
	"nervus.myservice:myserviced"        # ← 新增
)
```

有 `providergen/` 目录的服务，脚本会自动在 `sysmanifest` 之前跑它。

### 需要随包发布数据文件时

服务要带配置文件（板级参数、标定数据）时，必须在 `providergen` **之前**拷进包
目录——它要读，而且 `sysmanifest` 之后才算 `digests`。顺序错了的表现是内核以
「文件未被 digest 覆盖」拒绝整个包，而那个错误看起来像镜像损坏。

`camerad` 的板级 JSON 是范例（脚本里那段 `if [[ "$svc" == "camerad" ]]`）。

**同一份文件由 `providergen` 与服务本体共读**是刻意的：camerad 的资源声明从它
生成，运行期又用它做 role → 设备映射。拆成两份的话，「Catalog 里有 cam.front」
和「运行时 cam.front 指向哪个设备」会变成两个可以各自漂移的事实——而漂移之后
两边都不报错。

---

## 5. 新接口：写 Provider 契约，不改内核

**如果你只是消费已有接口，跳过本节。**

> **这一节以前教人去改 `nervud/internal/endpoint/catalog.go` 和
> `permission/catalog.go`。那两个文件已经不存在了**，取而代之的是数据驱动的
> `internal/catalog`——接口、权限、资源全部随包分发，加能力不改内核。

### 5.1 最省事的形态：元数据接口，零 protobuf 消息

能力接口的载荷常常本来就不是 protobuf（摄像头帧、麦克风采样、雷达点云都走
Transfer 的不透明字节）。这类接口**不需要任何 `.proto` 消息**：

```go
// myserviced/providergen/main.go
methods := []*ipcv1.MethodMeta{
    {
        MethodId:           1,   // OpenStream
        RequiredPermission: "perm.my.thing",
        RiskClass:          ipcv1.RiskClass_RISK_CLASS_NORMAL,
        IsReadOnly:         true,
        // request_type / response_type 留空：没有控制面消息
        Transfer: &ipcv1.TransferPolicy{
            Direction:         ipcv1.TransferDirection_TRANSFER_DIRECTION_PROVIDER_TO_CALLER,
            MaxStreams:        1,
            MaxPacketBytes:    4 << 20,
            MaxBytesPerSecond: 64 << 20,
        },
    },
    {MethodId: 2, RequiredPermission: "perm.my.thing"},  // CloseStream
}

hash, _ := ipcregistry.MethodsHash(methods)
descriptor := &ipcv1.ProviderDescriptor{
    PackageId: "nervus.myservice",
    Interfaces: []*ipcv1.ProvidedInterface{{
        InterfaceId: interfaceID,
        InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
            Major: 1, SchemaHash: hash, Methods: methods,
        }},
        RequiredPermission: "perm.my.thing",
    }},
}
// 空的 bundle set —— 没有任何 schema
descriptorWire, schemaWire, err := ipcregistry.MarshalProviderArtifacts(
    descriptor, &ipcv1.InterfaceSchemaBundleSet{})
```

内核真正需要知道的只有三件事：**谁在调、允不允许、能开多大的管子**。这三件都在
`MethodMeta` 里，与消息形状无关。

> ⚠️ **元数据接口不得声明 `request_type` / `response_type` / `error_detail_type`**
>
> 没有 schema bundle 可供解析它们。打包期就会被拒。

> ⚠️ **`Transfer` 预算的三项都不能为 0**
>
> `MaxStreams` / `MaxPacketBytes` / `MaxBytesPerSecond` 任一为 0，nervud 会以
> `method transfer policy is unbounded` 拒绝——而那时错误出现在第一次开流，
> 离「manifest 里少写了一项」很远。打包期已经会拦下。

### 5.2 需要结构化消息时：带 schema bundle

控制面确实要传结构化数据时，才编 `.proto`，用 `BuildSchemaBundle` 从生成的
method enum 构造 bundle（`pkgmanagerd/providergen/main.go` 是范例）。

**要推事件就改用 `BuildSchemaBundleWithEvents`**，多传一个 event enum
（`camerad/providergen/main.go` 是范例）：

```go
bundle, err := ipcregistry.BuildSchemaBundleWithEvents(
    interfaceID, 1,
    myv1.MyMethod(0).Descriptor(),
    myv1.MyEvent(0).Descriptor())
```

> ⚠️ **带载荷的事件必须走 bundle，不能内联到 descriptor**
>
> `ProvidedInterfaceVersion.events` 那条路是给**元数据接口**用的，它不允许
> `payload_type`——没有 FileDescriptorSet 就无从校验那个类型名指向的消息是否
> 真的存在。而带 schema 的接口正相反，它的事件几乎总是有载荷。
>
> 两处都写会被 registry 当成「同一个接口既内联又带 bundle」直接拒绝。
>
> 事件枚举**必须与方法枚举同文件**，否则它的传递依赖不在 bundle 的
> descriptor set 里。它不影响 `schema_hash`（本来就在同一份 descriptor set
> 里），所以给一个已有接口补上事件枚举**不会让已装的 Provider 报到失败**。

> ⚠️ **`delivery_class` 漏填会 fail closed 成 RELIABLE**
>
> 那是最严的一档（不允许任何丢弃）。一路遥测流按 RELIABLE 走，消费方稍慢
> 就会被内核断开订阅。想要「只要最新值」写 `STATE`，想要「丢了就丢了」写
> `LOSSY`——两者的语义相反，而漏填时你拿到的是第三种。

**method ID 常量必须从生成代码取，不要在本地重抄一份**——抄一份的代价不是重复，
是它会悄悄过期：

```go
methodInstall = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_INSTALL)  // 对
const methodInstall = 1                                                            // 错
```

### 5.3 权限与命名空间

| 你想定义 | 规则 |
|---|---|
| `perm.*` 平台权限 | 只有 `platform-release` 签名的包能定义 |
| 私有权限 | 必须在 `<package_id>.*` 之下 |
| `nervus.interface.*` 标准接口 | 只有 `platform-release` 能**定义**；`oem-service` 可以**实现** |
| 私有接口 | 必须在 `<package_id>.*` 之下 |
| `nervus.resource.*` 标准资源类型 | 只有 `platform-release` 能定义；OEM 只能绑定已存在的 |
| 资源标签键 | 同上：平台标签 `nervus.*` 只有 platform-release 能定义，私有标签在自己命名空间下 |

> ⚠️ **多个 Provider 实现同一标准接口时，契约必须逐字节一致**
>
> `sameInterfaceContract` 会比对 method_id、required_permission、risk_class、
> Transfer 预算。任何一项不同，第二个 Provider 会以
> `interface ... conflicts with definition owned by ...` 被拒。
>
> 这正是「厂商可互换」成立的原因：App 解析到哪一家，拿到的语义都一样。

### 5.4 三仓库必须指向同一个 ipc commit

`nervud`、`nervus-system-server`、`nervus-ipc` 三者**必须指向同一个 commit**，
否则两侧对同一份 Envelope 的理解会悄悄分叉——而两边都能编译、都能跑。
用各自仓库的 `scripts/sync-ipc.sh` 同步。

---

## 6. 服务间共享文件区

申请 `perm.storage.shared` 才会拿到两个目录（**按需创建，不申请就没有**）：

```text
/run/nervus/shared/<package_id>/      tmpfs，重启即失。运行期交换
/var/lib/nervus/shared/<package_id>/  磁盘，持久。配置/模型/缓存
```

属主 = 你的 UID，模式 0755：**自己的目录可写，别人的目录可读**。目录由内核建——
根是 nervud 独占可写的，所以没有包能抢先占用别人的目录名。

### 想用自己的协议跟另一个服务通信？把 UDS 建在这里

```go
sock := filepath.Join("/run/nervus/shared", myPackageID, "control.sock")
ln, err := net.Listen("unix", sock)
```

目录 0755，别的服务读得到路径也连得上。**这是「服务之间用自己的协议」最直接的
落地方式**——内核不参与，也不解析你说什么。

UDS 适合控制与小消息；大流量走第 7 节的 Transfer。

> ⚠️ **敏感数据不能放这里**
>
> 共享区是**全系统可读的**（跨包读隔离只靠数据目录的 0700 实现，共享区是 0755）。
> 只放「拿到 `perm.storage.shared` 就有资格看」的东西。
>
> 需要真实权限门槛的数据走 Transfer 的内存句柄——句柄本身就是凭证，没有文件
> 系统路径可绕。把摄像头帧写进共享区等于让任何应用绕过 `perm.camera.capture`。

> ⚠️ **不要用磁盘文件传视频流**
>
> 1080p30 裸流约 93 MiB/s，持续写 eMMC 是在烧闪存寿命。运行期大流量要么走
> Transfer，要么落在 tmpfs 那个根。

---

## 7. 高吞吐数据：Transfer

控制面 Envelope 不承载大数据。流程：

1. 方法在 `MethodMeta.transfer` 声明预算（第 5.1 节）
2. Provider handler 从 `CallContext.RouteID` 取当前 route，在**同一条**
   `ServiceHost` 连接上调 `nervus.interface.transfer.control@1`
3. nervud 按权威元数据、调用者权限、连接预算和 route 生命周期收紧方向、模式、
   包大小与速率，返回 Provider/Caller 两张短期一次性 `TransferHandle`
4. 两端 `sdk.AttachTransfer` 附着到内核独占的 Transfer socket

**附着之后，管子里流什么由两端自己决定**——内核只做分帧与限速，不解析内容。
这就是「与对端的数据用自己的协议」的落点。

---

## 7.5 长任务：Operation

一次调用要花 10 秒（机械臂轨迹、回零、导航到点）时，普通 `Request/Response`
不够用。

### 先判断你是不是真的需要它

普通调用的隐含约定是**响应到达 = 事情做完了**。设置速度、读状态这类瞬间完成
的事完全够用，不要套 Operation。

| 你想让调用方知道 | 用 |
|---|---|
| 设备现在什么样（在哪、速度多少） | 普通只读方法 + 状态事件 |
| 我那次调用跑完没有、成没成 | Operation |

两者互补，不是替代。`basemotion.proto` 里 `GetMotionState` 与轨迹类
Operation 会同时存在。

**如果一个能力只有一个控制方、动作不需要取消、失败原因不要紧**，状态方法就够
了。Operation 是为「多控制方 + 需要取消 + 需要可靠的失败原因」准备的。

那三条为什么绑在一起，看这个场景：A 发起一条 10 秒轨迹，第 3 秒操作员（人，
优先级更高）抢占租约并发起另一条动作。只有状态方法时 A 看到的是
`MOVING → MOVING → READY`——它无从知道自己那次失败了，甚至会以为成功了。

### 时序

```
App --Request(声明了 returns_operation 的方法)--> nervud
                nervud 建 Operation(PENDING)，operation_id 进 ExecutionContext
                nervud --Dispatch--> 你的服务
                nervud <--DispatchResult{ACCEPTED}-- 你的服务
App <--Response{ACCEPTED, OperationHandle}-- nervud
                nervud <--ReportProgress / CompleteOperation-- 你的服务
App <--OperationChanged(订阅事件)-- nervud
```

**Operation 在 Dispatch 之前就存在**，所以你收到 Dispatch 时
`ExecutionContext.operation_id` 已经有效，可以直接回 `ACCEPTED`。

### 提供侧要做什么

1. 方法的 `MethodMeta` 声明 `returns_operation: true`
2. handler 收下请求、**立刻返回**（回 `ACCEPTED`，`payload` 留空），把活交给
   自己的 goroutine
3. 真正开始动时调 `AcceptOperation`（PENDING → RUNNING）
4. 中途调 `ReportProgress`
5. 结束时调 `CompleteOperation`

三个上报方法都在 `nervus.interface.operation.control@1` 上，Resolve 一次即可，
与 Transfer Control 同形。

> ⚠️ **`Success.payload` 必须留空**
>
> `operation_id` 由 nervud 分配、状态机也归 nervud。你自己写一个句柄，调用方
> 拿到的编号 nervud 不认识——取消永远取消不到、进度永远收不到，**而两边都
> 不报错**。内核会拒掉带 payload 的 ACCEPTED。

> ⚠️ **`AcceptOperation` 要带回 `motion_epoch`**
>
> 从收到 Dispatch 到真正开始动之间可能隔着几百毫秒的准备时间，而那段时间足以
> 发生一次 Safety 停机。不带回来校验的话，你会基于一份已经作废的授权开始运动。

> ⚠️ **终态只写一次**
>
> 重复 `CompleteOperation` 返回 `FAILED_PRECONDITION` + `ALREADY_TERMINAL`。
> 幂等地静默接受第二次会掩盖一类真实故障：你的服务里有两条路径都认为自己该
> 结束这次执行，而它们可能给出相反的结论。

### 消费侧要做什么

`Response` 的 `payload` 是 `OperationHandle`。拿到 `operation_id` 之后：

- **订阅** `OperationChanged` 持续观察（正常路径）
- `GetOperation` 拉一次（补两个窗口：刚拿到句柄还没订阅那一小段，以及断线
  重连后「我走的时候它还在跑」）
- `CancelOperation` 请求取消

> ⚠️ **订阅必须带 `scope = operation_id`**
>
> 本接口只有一个内建 endpoint，全机所有 operation 的事件都从它出来。不指定就
> 会被拒——nervud 在 Subscribe 时就裁决可见性，订一个不属于自己的直接失败。
> 这是下一节那套通用机制的一个实例，只不过 operation 的归属由内核自己知道，
> 不用你登记。

> ⚠️ **`CancelOperation` 是请求，不是命令**
>
> 返回成功只表示 nervud 已把状态置为 `CANCEL_REQUESTED` 并通知了你的服务。
> 机械臂物理上停下来需要时间，真正的终态（`CANCELLED`，或者「来不及了，已经
> `SUCCEEDED`」）由后续事件给出。做成同步的「取消完成」会是一句谎话。

### 它不是「高速通道」

Operation 跟速度、优先级都没关系，也不是专线。所有东西走同一条 UDS。真正管
「快」的是另外三样：

| 机制 | 管什么 |
|---|---|
| Safety Lane | 急停。独立通道，不排队 |
| Transfer 数据面 | 大数据。摄像头帧不进控制面（第 7 节） |
| ControlLease + 抢占 | 谁有资格动机器 |

---

## 7.6 一个 endpoint 多实例：事件的实例作用域

一个 endpoint 常常同时管着好几件东西：一路摄像头上开着 4 条 stream，一个机械
臂上跑着几段轨迹。这些实例的事件全从**同一个 endpoint** 出来。

不做处理的话，订了 `StreamStateChanged` 的 App 会收到这个 endpoint 上**所有**
流的状态——包括别的 App 开的流。它拿到的是分辨率、帧率、故障时刻，足够拼出
「另一个应用正在用前置摄像头做什么」。

### 三步

**1. 事件声明 `scoped: true`**

```protobuf
option (nervus.ipc.v1.event_meta) = {
  event_id: 1
  delivery_class: DELIVERY_CLASS_STATE
  scoped: true            // 按实例分发，不广播
};
```

声明了之后，`Subscribe` 不带 scope 会被 nervud 拒；没声明却带了 scope 也会被
拒。**两个方向都 fail closed**——静默忽略 scope 才是真正危险的那种：调用方以为
自己在看一条流，实际收到的是全部，而事件本身看起来完全正常，它永远不会发现。

**2. Provider 造出实例时登记归属**

```go
// openStream：先 Commit 拿到 stream_id，再登记这条流归谁
if err := c.ep.BindEventScope(streamID, cc.RouteID); err != nil {
    c.log.Warn("登记事件归属失败，本流的状态事件将订阅不到", "err", err)
}
```

`cc.RouteID` 是 `CallContext` 里的当前 route——跟 Transfer 用的是同一个东西。
nervud 由它反查出调用方是谁，把 `(你, endpoint, scope) -> 调用方` 记下来。

`scope` 用什么值由你定，只要在**这个 endpoint 内**唯一。stream_id 这类你本来
就要发给调用方的句柄是最自然的选择——调用方不用再学一套编号。

**3. 关掉实例时撤销**

```go
// 【先发终态事件，再撤归属】
c.publishState(streamID, camerav1.StreamPhase_STREAM_PHASE_STOPPED)
c.releaseScope(streamID)   // -> c.ep.ReleaseEventScope(streamID)
```

顺序反了的话，订阅方收不到 STOPPED——而那正是它最需要的一条。

> ⚠️ **异常路径也要撤**
>
> 采集线程遇到 FAULT 直接结束、服务收到关停信号批量关流，这些路径都得走到
> `ReleaseEventScope`。连接断开和 endpoint 撤下时 nervud 会连带清干净，但那兜
> 的是崩溃；正常运行期间反复开关流不撤销，归属表会无界增长。

### 消费侧

```go
sub, err := host.Subscribe(ctx, sdk.SubscribeRequest{
    EndpointID: epID,
    EventID:    1,
    Scope:      streamID,     // 我开的那条流
})
```

订一个不属于自己的实例返回 `NOT_FOUND`，**不是** `PERMISSION_DENIED`——后者
等于告诉调用方「这个实例存在，只是不归你」，那本身就是信息。

### 内核只认指针，不解析你的协议

nervud 不知道 `stream_id` 是什么，也不会去解 `OpenStreamResponse`。它只维护
一张 `(Provider 连接, endpoint, scope) -> 所有者连接` 的表，`Subscribe` 时查
一下。所以：

- **归属是连接级的**。调用方断开，它的实例登记随之消失；重连之后要重新开流。
- **登记未到达 = 订不上**。`BindEventScope` 是单向的，失败只体现为后续订阅
  返回 `NOT_FOUND`。所以要么在返回句柄前登记，要么把失败告诉调用方，别让它
  拿着一个永远收不到事件的 stream_id。
- **重复登记直接覆盖**，不报错。关掉再开同一个编号是你自己的事。

---

## 8. 构建、签名、部署

```sh
scripts/build-image-tree.sh ./out signing/platform-release.pem linux-arm64
```

这一步做五件事：交叉编译（`CGO_ENABLED=0` 静态链接）→ **产出 Provider 契约** →
算 digest → 填 manifest → Ed25519 签名。

产物：

```text
out/nervus.myservice/
├── bin/myserviced      静态二进制
├── provider.binpb      Provider 契约
├── schemas.binpb       schema（元数据接口时为空集）
├── manifest.json       digests 已填好
└── manifest.sig        platform-release 角色签名
```

### 部署路径

```sh
cp -r ./out/* <镜像挂载点>/usr/lib/nervus/system-packages/
```

**目录名必须等于 `package_id`。** 内核启动扫描 glob 的是
`/usr/lib/nervus/system-packages/*/manifest.json`，**没有版本子目录**——系统包跟随
整镜像 OTA，不存在多版本并存。

> ⚠️ **系统镜像包与动态安装包是两条路，不能混**
>
> | | 系统镜像包 | 动态安装包 |
> |---|---|---|
> | 位置 | `/usr/lib/nervus/system-packages/<pkg>/` | `/var/lib/nervus/packages/<pkg>/<version>/` |
> | 形态 | **解包后的目录树** | `.nspkg`（zstd + tar） |
> | 签名角色 | `platform-release` | `developer`（自锚定） |
> | 设备访问 | 可以碰 `/dev` | 拿不到 |

### 开发构建必须开 `-dev-trust-system-packages`

开发二进制没有内嵌平台根，`LoadTrustStore` 失败，于是每个系统包 fail-closed 到
`Ordinary`——而 `perm.service.register` 要 OEM。**不开这个开关，你的服务注册不了。**

```sh
systemd-run --unit=nervud --collect \
  --property=StandardOutput=append:/tmp/nervud.log \
  --property=StandardError=append:/tmp/nervud.log \
  ./nervud -dev-skip-preflight -dev-allow-sched-degrade \
           -dev-trust-system-packages -log-level debug
```

它只放松「这把密钥是否由平台根授权」一件事。验签、key_id、digest 全部照做——
改了二进制、改了 manifest、想冒充某个 key_id，三种都照样拒。

> ⚠️ **必须以 systemd unit 运行，不能裸跑二进制**
>
> 组件的瞬态 unit 带 `BindsTo=nervud.service`。裸跑时那个 unit 不存在，
> `StartTransientUnit` 会以 `Unit nervud.service not found` 失败。

---

## 9. 内核为你做了什么

服务启动前，内核在**启动扫描**里自动补齐运行前置（`pkgregistry`）：

| 步骤 | 产物 | 少了会怎样 |
|---|---|---|
| 分配稳定 UID | 20000–59999，**永不回收** | — |
| `EnsureAppUser` | `/etc/passwd` 里的 `nervus-app-<uid>` | `217/USER` |
| 私有数据目录 | `/var/lib/nervus/package-data/<pkg>`，0700 | `226/NAMESPACE` |
| 共享区子目录 | `/run/nervus/shared/<pkg>` 与磁盘那个，0755 | 写入 `ENOENT` |
| 权限裁决 | `GrantedPermissions` | `missing permission` |

然后经 `authority` → systemd 拉起一个瞬态 unit：

```
nervus-<package_id>-<component_id>.service
```

沙箱固定包含：`NoNewPrivileges`、`ProtectSystem=strict`、
`SystemCallFilter=@system-service`、`BindsTo=nervud.service`（nervud 死了你也停）。

**可写的只有自己的数据目录（加共享区自己那个子目录）。** 别的位置即便属主与权限
都对，写进去也是 `read-only file system`——**权限与挂载是两道独立的门**。

### 一个坏包不会拖垮启动

Catalog 层的冲突以前会让整个内核启动失败。现在启动扫描会**按包隔离肇事者**、
大声审计、继续启动。你的服务被隔离时，journal 里会有
`pkgregistry: package quarantined at boot`。

---

## 10. 调试

```sh
# 看你的服务
journalctl -u nervus-nervus.myservice-main.service -n 50
systemctl show nervus-nervus.myservice-main.service --property=ActiveState,SubState

# 看内核怎么看你
grep -E "handshake complete|RegisterEndpoint|ResolveEndpoint|provision|quarantined" /tmp/nervud.log
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
| `Unit ... was already loaded` | 上一次的瞬态 unit 没回收 | 手动清：`systemctl reset-failed <unit>` |

### 装载与契约

| 症状 | 原因 |
|---|---|
| `ErrProviderArtifactsRequired` | manifest 有 `exports` 却没有 `provider` 段。**没有例外通道** |
| `provider.descriptor 未被 digests 覆盖` | `providergen` 没在 `sysmanifest` 之前跑 |
| `interface ... conflicts with definition owned by ...` | 与已有 Provider 的契约不一致（method_id / 权限 / 风险 / Transfer 预算任一项） |
| `package quarantined at boot` | 你的包被 Catalog 层拒了，整机照常启动。看同一行的 `err` |
| `declares inline methods and a schema bundle` | 同一个接口版本既内联 `methods` 又带 bundle，两份契约 |

### 握手与注册

| 症状 | 原因 |
|---|---|
| `UNAUTHENTICATED` | `componentID` 与 manifest 的 `components[].id` 对不上。**不是权限问题** |
| `interface not declared in manifest exports` | `interfaceID` 与 manifest 的 `exports[].interface` 对不上 |
| `missing permission perm.service.register` | manifest 里没申请，**或**开发构建下没开 `-dev-trust-system-packages` |
| `interface schema hash does not match the catalog` | `RegisterEndpoint` 的 `SchemaHash` 与 descriptor 里的不一致。空 hash 的兼容桥已移除 |
| `RegisterEndpoint` 返回 `INVALID_ARGUMENT` | `ResourceHandle` 填了但不在 Resource Registry 里。不绑资源就**留空** |

### 解析与调用

| 症状 | 原因 |
|---|---|
| Resolve `NOT_FOUND` | 目标服务还没 `RegisterEndpoint`（竞态）→ **重试**；或它根本没起来 → 看它的 journal |
| Resolve `PERMISSION_DENIED` | 缺接口对应的权限 |
| Resolve `FAILED_PRECONDITION` | 版本协商失败；或 selector 命中 0 个；或**命中多个而策略是 `REQUIRE_UNIQUE`**（默认） |
| 调用返回 `NOT_FOUND` 但方法明明有 | handler 注册晚于 `RegisterEndpoint` |
| `method transfer policy is unbounded` | `MethodMeta.transfer` 的三项预算有 0 |

### 文件系统

| 症状 | 原因 |
|---|---|
| `permission denied` 写自己数据目录之外 | 沙箱只给数据目录 + 共享区自己那个子目录可写。**这是设计** |
| `read-only file system` 而属主权限都对 | `ProtectSystem=strict`。属主是一道门，挂载是另一道，**两道都要过** |
| 写共享区 `ENOENT` | 没申请 `perm.storage.shared`，子目录没被建出来 |

### 签名与安装

| 症状 | 原因 |
|---|---|
| `signer key not trusted for its role` | 用 `platform` 角色签了但没有内嵌 platform 根 → 开发构建请开 `-dev-trust-system-packages` |
| `system package signature not verified; downgrading to Ordinary` | 同上。降级后 `perm.service.register` 会被拒 |
| `digest mismatch` | 二进制或 provider 契约改了但没重跑 `sysmanifest`。**改了就必须重签** |

---

## 检查清单

新服务上线前逐条过：

- [ ] 目录名不与已有服务冲突
- [ ] `componentID` 常量 == manifest `components[].id`
- [ ] `interfaceID` 常量 == manifest `exports[].interface` / `interfaces[]`
- [ ] Provider：有 `providergen/`，manifest 有 `provider` 段
- [ ] Provider：`RegisterEndpoint` 的 `SchemaHash` == descriptor 里声明的
- [ ] `MethodMeta.transfer` 的三项预算都非 0（声明了 transfer 的方法）
- [ ] 一个 endpoint 管多实例的：事件 `scoped: true`，每条正常与异常的关闭路径
      都走到 `ReleaseEventScope`
- [ ] `permissions` 里申请了需要的权限
- [ ] handler 在 `RegisterEndpoint` **之前**全部注册完
- [ ] Client 的 Resolve 有重试
- [ ] 绑资源的接口用 labels 选设备，不硬编码 role
- [ ] 退出时 `UnregisterEndpoint`
- [ ] 加进 `scripts/build-image-tree.sh` 的 `SERVICES`
- [ ] 三个仓库指向同一个 nervus-ipc commit
- [ ] README 里写了部署路径
- [ ] `go vet ./... && go test ./... && go test ./... -race`

---

## 延伸阅读

| 想知道 | 看 |
|---|---|
| 协议字段与 Envelope 结构 | `nervus-ipc/README.md`、`proto/nervus/ipc/v1/envelope.proto` |
| 哪些 body 内核真的支持 | `nervus-ipc/README.md` 的「实现状态」表 |
| 内核已知缺口 | `nervud/agent.md` 的「已知缺口」 |
| 接口/权限/资源怎么进 Catalog | `nervud/internal/catalog/builder.go` |
| endpoint 注册的准入链 | `nervud/internal/endpoint/register.go` |
| 沙箱属性全集 | `nervud/internal/authority/systemd/props.go` |
| 包的安装事务 | `nervud/internal/pkgregistry/install.go` |
