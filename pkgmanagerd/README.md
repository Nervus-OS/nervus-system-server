# nervus.pkgmanagerd

软件安装系统服务。对 App 提供 IPC 接口，内部经管理通道转发给 nervud。

```text
App (Kotlin)  ──IPC──▶  pkgmanagerd  ──adminwire──▶  nervud
                             │
                        解 .nspkg（防 tar-slip）
```

## 装在哪

```text
/usr/lib/nervus/system-packages/nervus.pkgmanagerd/
├── manifest.json          ← 被签名的那份原始字节
├── manifest.sig           ← platform-release 角色的 Ed25519 签名
└── bin/pkgmanagerd        ← 载荷，必须被 manifest.digests 覆盖
```

目录名**必须**等于 `manifest.json` 里的 `package_id`。内核的启动扫描是

```go
filepath.Glob(filepath.Join("/usr/lib/nervus/system-packages", "*", "manifest.json"))
```

它按 `manifest.json` 的内容认包，目录名只是容器；但两者不一致会让人在排查时
看着目录名找错包，没有任何好处。

**这个位置是只读镜像区**（preflight 校验 `/usr/lib/nervus/system-packages`
为 `0755` 且不可写）。装进去靠刷镜像或 OTA，不能用 `nervusctl install`——
那条路只接受 `SourceDynamicInstall`，系统包走它会被直接拒绝。

运行期的另外两个路径由内核分配，不需要手工建：

| 路径 | 用途 |
|---|---|
| `/var/lib/nervus/package-data/nervus.pkgmanagerd/` | 私有数据目录，属主为本包 UID，`0700` |
| `/var/lib/nervus/registry/nervus.pkgmanagerd.json` | UID 与授权记账，跨重启保持 |

产出这棵树：

```sh
scripts/build-image-tree.sh <输出目录> <签名私钥.pem> [ABI]
cp -r <输出目录>/* /usr/lib/nervus/system-packages/
```

## 它是一座桥，不是一个决策者

**全部安全判定在 nervud 的 pkgregistry**：多角色验签、digest 复核、OEM 副署准入、
Host ABI 匹配、升级裁决（防降级 + 签名血统连续性）、权限交集、UID 分配、原子提交。

本服务一条都不做，也**不允许**做。在这里写出任何形如「如果……就允许」的代码
都是设计错误。

它存在的理由只有一个：App 不可能是 root，连不上 root-only 的管理通道，需要一个
跑在 App UID 段、但被内核显式放行的服务替它转发。

## 唯一的特权，以及它有多窄

这是**唯一**被允许连 `/run/nervus/nervud-admin.sock` 的服务。内核侧对应
`admin.Config.ServiceUID`：装配时从 Registry 查本包 UID，把 socket 的组 chown
成它并放宽到 0660。

放行的是**「谁能连上这条 socket」，不是「连上能做什么」**。

`adminclient.go` 刻意放在本服务自己的 `package main` 里，而不是仓库级的共享包——
这样别的服务想用都 import 不到，「只有 pkgmanagerd 能碰管理通道」这条约束是
**结构性**的，不靠自觉。

## 安装流程

```text
1. CmdBeginStaging ──▶ nervud 建 staging 目录并返回绝对路径
2. Unpack()            把 .nspkg 解进去（本服务唯一有安全责任的一步）
3. CmdInstall     ──▶ nervud 复核签名/digest/裁决，原子提交
```

第 1 步**必须由 nervud 建目录**，不能自己挑一个。它保证三件事：

- 位置与 PackageRoot 同一文件系统 —— 安装期的 `renameat2` 才不会 EXDEV 失败
- 属主与权限受控
- install 时的路径逃逸校验有一个明确的「必须是我发出的目录」判据

第 2 步失败时**不清理 staging**。我们没有删它的权限，也不该有——那个目录属于
nervud 的掌控范围，给转发层删除权等于多开一个可被滥用的口子。nervud 自己会在
下一次 begin-staging 时清扫超过一小时的孤儿目录。

## 解包的安全责任

解包发生在提交给 nervud 复核**之前**，一个恶意 `.nspkg` 若带 `../..` 或绝对路径
条目，解包时就能写到目标目录之外。这一条 nervud 替不了——越界发生在它看到这棵树
之前。

`unpack.go` 的每一条防护都对应一种具体攻击，逻辑照抄内核
`cmd/nervusctl/unpack.go`（那份已在生产路径上验证过），刻意不「改进」：

| 防护 | 挡的是 |
|---|---|
| `safeJoin` 拒绝绝对路径与 `..` 折叠后逃逸 | tar-slip |
| 前缀比较**带分隔符** | `/a/staging-evil` 混过 `/a/staging` |
| 只接受 `TypeDir` / `TypeReg` | 符号链接逃逸、设备节点 |
| 单条目 64 MiB 上限 + `io.CopyN` 兜底 | 解压炸弹（含 `hdr.Size` 谎报） |
| 总条目 10000 上限 | 海量小条目炸弹 |
| 权限只取低 9 位 | setuid 落盘提权 |
| `O_EXCL` | 重复条目名「后写覆盖先写」，替换掉已校验内容 |

内容层面的信任锚仍在 nervud：它会按 `manifest.Digests` 对 staging 里每个文件
重新做 `VerifyDigests`。

## 当前状态

**没有注册任何 method handler**，这是有意的。

`nervus.interface.pkg.manager` 的 `.proto` 还没进 `nervus-ipc`。手搓一套临时的
payload 编码是明确的红线（绝不手搓信封）——临时格式最后都会变成永久格式，
还没有 golden vectors 兜着。

也没有注册一批「返回未实现」的占位 handler：协议的 `StatusCode` 里根本没有
`UNIMPLEMENTED`，硬凑一个语义相近的码只会误导调用方（`UNAVAILABLE` 意味着
「稍后重试」，而这里重试多少次都一样）。SDK 对未注册的 `method_id` 本来就
fail closed 回 `NOT_FOUND`——「这个方法不存在」正是此刻的事实。

所以现在这条链能验到：**服务起得来、握得上手、报得了到、能被 App Resolve 到**，
调用任何方法得到一个准确的 `NOT_FOUND`。

`service.go` 里的业务逻辑（`InstallFromFile` / `Uninstall` / `List` / …）已经
写完并可用，等 proto 落地后在 `registerHandlers` 里逐个 `Handle` 接上即可，
服务骨架不用动。

### proto 落地时要一起做的

- [ ] `method_id` 与 `service.go` 里的 `MethodInstall` 等常量对齐，**以 proto 为准**
- [ ] 按 `method_registry` 机制挂 `method_meta`（权限、风险级、是否需用户确认）
- [ ] `main.go` 的 `RegisterEndpoint` 填上 `SchemaHash`。内核目前
      **只记录不比对**（`register.go` 步骤 5：v1 尚无权威 schema Registry），
      所以现在留空不会被拒；schema Registry 落地后比对会开启，届时不填就注册不上

## 内核凭什么让它注册

`endpoint/register.go` 的准入是七步，前四步会拒绝：

| 步骤 | 要求 | 本服务如何满足 |
|---|---|---|
| 1 | 握手已完成（`caller.ComponentID` 非空） | `declared_component_id = "main"`，经 PID → cgroup → unit 核对 |
| 2 | 接口必须在 `manifest.components[].exports` 里声明 | `manifest.json.in` 已声明 |
| 2 | 组件未被停用 | 在保护名单里，停不掉 |
| 3 | `visibility: public` → 需 `perm.service.register` | `manifest.permissions` 已申请 |
| 4 | `resource_handle` 非空则必须在 Resource Registry 里 | 留空（本接口不绑物理资源） |

第 3 步的 `perm.service.register` 正常需要 OEM+ trust（跨 Package 可见的服务
不能随便注册）。开发期没有平台根时本包 trust 会降为 `Ordinary`，但内核的
`permission.V1GrantAll` 当前把「申请即授予」打开了，所以仍能通过。

**执法恢复后**（`V1GrantAll = false`），本包必须真正拿到 platform 签名才注册得上——
那时开发机上会以 `PERMISSION_DENIED: missing permission perm.service.register` 失败，
这是预期的，不是回归。

## 文件

| 文件 | 职责 |
|---|---|
| `main.go` | 进程入口：握手 → 报到 → Serve |
| `service.go` | 业务实现，全部经 adminclient 转发 |
| `adminclient.go` | 管理通道客户端（**只有本服务可用**） |
| `unpack.go` | `.nspkg` 解包 + tar-slip 防护 |
| `manifest.json.in` | 包清单模板，`digests` 由 `sysmanifest` 填充 |

## 本地运行

```sh
# 需要一个已跑起来的 nervud
go run ./pkgmanagerd -sock /run/nervus/nervud.sock -log-level debug
```

握手会失败（`UNAUTHENTICATED`），除非本进程确实是 nervud 按 manifest 拉起的
那个组件——`declared_component_id` 要经 PID → cgroup → systemd unit 核对。
这是预期行为，不是 bug。
