# nervus.camerad

Nervus OS 的通用摄像头服务。把「一堆 `/dev/videoN`」变成「这块板上有哪几路摄像头，各自朝哪边」。

## 它解决的问题

V4L2 只回答「这个节点能出什么格式」。它答不了两件事：

- **哪个是前视**——那是板级事实，不在驱动里
- **重启之后还是不是同一个**——`videoN` 的编号由驱动加载顺序决定

本服务用一份**板级 JSON** 回答第一个问题，用 **USB 拓扑路径**回答第二个。

## 一路摄像头 = 两个资源

```
nervus.resource.camera          SHARED_OBSERVE      采集，多方共享
nervus.resource.camera.config   EXCLUSIVE_CONTROL   配置，独占租约
```

拆开是因为两者的语义相反：导航、录制、AI 检测可以同时看同一路画面；而两个进程同时改曝光，最后谁说了算取决于时序，画面会在两个值之间跳。

内核租约按资源发、一个 endpoint 绑一个资源，所以必须是两个资源、两个接口、两个 endpoint。好处很直接：**只看画面的 App 永远不需要碰租约**，也就永远不会因为别人在调参数而被拒绝。

配置面的 `stable_role` 是采集面加 `.config` 后缀——Catalog 的 handle 索引跨资源类型全局唯一，同名会在装配期直接撞 handle。

## 板级配置

`boards/<board>.json`，构建时由 `NERVUS_BOARD` 选择（默认 `reference`）。

```json
{
  "schema": 1,
  "board": "acme-quadruped-v2",
  "cameras": [
    {
      "role": "cam.front",
      "match": {"usb_path": "1-1.1"},
      "labels": {
        "nervus.camera.facing": "front",
        "nervus.camera.kind": "color",
        "nervus.camera.mount": "head"
      }
    },
    {
      "role": "cam.depth",
      "match": {"device_path_link": "platform-csi0-video-index0"},
      "labels": {"nervus.camera.kind": "depth"}
    }
  ],
  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
}
```

### 这份文件被读两次，这是刻意的

| 时机 | 谁读 | 用来干什么 |
|---|---|---|
| 构建期 | `providergen` | 生成 `ProviderArtifacts` 里的资源声明 |
| 运行期 | `camerad` | 把 role 映射到真实设备节点 |

**必须是同一份文件。** 分成两份的话，「Catalog 里有 cam.front」和「运行时 cam.front 指向哪个设备」会变成两个可以各自漂移的事实——而漂移之后两边都不报错，只是 App 请求前视摄像头时拿到一个不存在的 endpoint。

### 为什么资源集合在构建期定死

`ProviderArtifacts` 进 manifest 的 `digests`，随镜像签名。如果资源是运行期从 JSON 现读的，**改一行 JSON 就能给自己加一路「前视摄像头」**——签名覆盖不到的东西就不是契约。

代价是换板要重新打包。那是对的：板级配置本来就是镜像的一部分。

### 设备定位：用位置，不用编号也不用序列号

`usb_path`（如 `1-1.2`）描述的是**插在哪个物理口上**。只要不重新布线就不会变。

- 不用 `/dev/videoN`：编号随驱动加载顺序变。用它做映射的表现是「机器重启一次，前视变成了后视」，而没有任何日志会提到这件事
- 不用序列号：换一个同型号摄像头修机器，序列号变了 role 就丢了。板级配置描述的是**位置**，不是某一个具体的模组

MIPI/CSI 摄像头没有 USB 拓扑路径，用 `/dev/v4l/by-path/` 下的稳定链接名（`device_path_link`）。

### 标签白名单

平台语义标签限于三个键，值也在白名单内：

| 键 | 取值 |
|---|---|
| `nervus.camera.facing` | front / rear / left / right / up / down |
| `nervus.camera.kind` | color / depth / ir / fisheye |
| `nervus.camera.mount` | 自由字符串 |

私有标签必须以 `nervus.camerad.` 开头。

拼错的键（`nervus.camera.face`）或值（`Front`）**在构建期直接报错**。不拒的话运行期表现是「这路摄像头再也选不到」，而没有任何东西会指向那个拼写错误。

### USB 冷插拔池

`usb_pool` 给临时插上来的 USB 摄像头一个落脚处。槽位在**服务启动的那一刻**按 USB 拓扑路径排序一次性分配。

排序是按段做**数值**比较，不是字典序——后者会把 `1-10` 排在 `1-2` 前面，于是插满 10 个口的机器上槽位顺序与物理顺序对不上，而这种错位只在设备数跨过 10 时才出现。

**它对设备集合的变化不稳定**：拔掉 `1-1.2` 上的摄像头，原本在 `1-1.3` 的会从 `usb.0` 挪到 `usb.1`。这不是缺陷，是「第几个」这件事的固有性质。需要绝对稳定的设备必须写进 `cameras` 用 `usb_path` 精确匹配。

**池槽位没有语义标签**。临时插上来的摄像头朝哪边没人知道，随便给一个 `facing` 会让 App 按语义选到一个朝向未知的设备——那比选不到更糟。

### 只支持冷插拔

运行中插入的摄像头不会自动出现。热插拔要求「新设备出现 → 动态注册 endpoint → Catalog 里凭空多一个资源」，而资源集合是构建期签名的，运行期加资源等于绕开签名。真要支持，得先解决「动态资源如何进入受签名的契约」——那不是摄像头一个能力的事。

## 数据面不经过 nervud

```
App --Request(OpenStream)--> nervud --Dispatch--> camerad
camerad --BeginTransfer--> nervud                    建管子
camerad --AttachTransfer--> nervud-transfer.sock     接上生产端
camerad --CommitTransfer--> nervud                   放行消费端
camerad --OpenStreamResponse(caller handle)--> App   交出凭证
App --AttachTransfer--> nervud-transfer.sock         接上消费端
                ↓
    此后帧在共享内存环里流动，nervud 不在数据路径上
```

顺序不能变：Commit 之前 caller ticket 附不上；而 Commit 必须在业务响应确定能返回之后——先 Commit 再失败的话，App 拿不到 handle，那条 Transfer 却已经放行，成了一条谁都关不掉的悬空管子。

**控制面上没有逐帧事件**。环自带 eventfd 唤醒，再推一条「有新帧」等于把每帧一次的流量搬回控制面，正是共享内存数据面存在的理由。控制面事件只有两种，都不按帧发生：

| 事件 | 类别 | 含义 |
|---|---|---|
| `StreamStateChanged` | STATE | 流开了/停了/坏了。中间态合并掉没有损失 |
| `DeviceError` | RELIABLE | 掉线、缓冲溢出、驱动错误。不能合并也不能丢 |

## 格式不做就近匹配

请求 1920×1080 而设备只有 1280×720 时返回 `FAILED_PRECONDITION` + `FORMAT_NOT_SUPPORTED`，**不会悄悄给一个更小的**。静默降级会让 App 按错误的尺寸解析共享内存，画面撕裂却查不出原因。

`S_FMT` 是协商而非设置——驱动会把不支持的请求改成最近的支持值并**不报错**。所以设完必须回读，对不上就失败。

## 厂商怎么接进来

厂商**不实现标准接口**，而是实现自己命名空间下的私有接口（`vendor.acme.camerad.interface.source@1` 一类），由本服务转成标准接口。

这样标准语义（`facing=front`）由平台的板级配置给出，厂商无法自封——内核的命名空间规则会拒绝一个 `oem-service` 签名的包声明 `nervus.*` 标签或接口。厂商提供的是**一路画面**，不是「这路画面朝哪边」。

数据面仍然直达：camerad 把厂商给的 `TransferHandle` 转交给 App，帧不经过本进程复制一次。

**这条路径尚未接线**，当前只支持 V4L2 设备。接厂商源需要一个真实的厂商 Provider 来定义私有契约的形状。

## 板级集成

```sh
camerad -profile boards/acme.json -dry-run
```

打印 role → 设备的映射后退出，不连控制面。声明了但没插的 role 会单独用 WARN 报出来——板级集成阶段最常见的问题就是「配置里写了前视，但线没插上」，而那时日志里只是**少了一行**。

## 布局

```
camerad/
├── boardprofile/     板级 JSON 解析与校验（构建期与运行期共用）
├── boards/           板级配置，按板一份
├── providergen/      构建期：板级 JSON → ProviderArtifacts
├── contract.go       接口常量与 schema hash
├── discover.go       设备发现、固定 role 匹配、USB 池分配
├── v4l2_linux.go     V4L2 兼容层（ioctl 号由 unsafe.Sizeof 算出）
├── stream.go         取流生命周期与帧泵
├── config.go         配置面 endpoint（可调项）
├── transfer.go       Transfer Control 三步封装
├── events.go         控制面事件推送
├── errors.go         带 typed detail 的失败
├── pixfmt.go         V4L2 fourcc ↔ 平台 PixelFormat
└── main.go           装配
```
