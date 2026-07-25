# nervus.safety.recovery

Safety 恢复策略服务。**机制在内核、策略在这里。**

```text
safetyrecoveryd  ──IPC──▶  nervud 的内建 endpoint
                            nervus.interface.safety.control
```

## 装在哪

```text
/usr/lib/nervus/system-packages/nervus.safety.recovery/
├── manifest.json
├── manifest.sig
└── bin/safetyrecoveryd
```

## 与 pkgmanagerd 正好相反：它是 Client 不是 Provider

pkgmanagerd `RegisterEndpoint` 报到，等 nervud 转发 Dispatch 过来。
本服务 `ResolveEndpoint` 然后 `Request`——它是**消费方**。

它消费的那个接口是**内建 endpoint**：实现者是 nervud 自己，不是任何 Package。
但调用形态与任何 Provider 提供的接口完全一样，本服务不知道也不需要知道对面
是内核。

## 内核只守一条硬前置

`safety.Rearm()` 复核的唯一一件事：**停止进度必须已经落定**——

- 正常链已到 `OUTPUT_DISABLED` / `STANDSTILL_CONFIRMED`（输出确认关闭），或
- 已入 `DELIVERY_FAULT` / `STANDSTILL_TIMEOUT` 这类必须人工处置的终态旁支

停止还在途中（`REQUESTED` / `SENT` / `PROVIDER_ACCEPTED` / `MCU_ACKED`）就解开
latch，等于在**还不知道输出有没有关掉**的情况下重新放行运动。

「此刻该不该 re-arm」不在内核：要看设备状态、故障原因、用户是否在场、触发停机
的那个条件是否已经排除。这些会随产品演进，塞进内核就等于「改一次恢复策略」
和「改一次内核」变成同一件事。

## 它现在做什么：观察

周期性读安全态，记录状态迁移。进入 `REARM_REQUIRED` 的时刻与**当时的停止相位**
用 WARN 级别记——那是事后复盘「为什么停机、停到哪一步」的第一手材料。

三个字段都比对才算变化。`motion_epoch` 单独变化（抢占、Provider 重启）不改变
顶层状态，但它意味着**所有在途运动命令已被废止**，是必须记下来的事件。

### 为什么是轮询而不是订阅

Subscribe 组（envelope 40-45）nervud 尚未实现，发过去会被**直接关连接**。
见 `nervus-ipc` 根 README 的「实现状态」表。

轮询周期 2 秒是权衡：安全态迁移是低频事件（正常运行时根本不变），太密只是白烧
CPU 与审计条目；但停机之后运维盯着 UI 等状态更新，太慢会显得系统没反应。

## 它现在【不】做什么：自动 re-arm

**没有实现任何自动解除停机的逻辑，这是刻意的。**

自动 re-arm 必须先回答「触发停机的条件是否已经消失」，而那个判断依赖 Provider
的设备状态上报——`safety.proto` 的五条消息（`SafetyHalt` / `HaltAccepted` /
`StopProgress` / `StandstillConfirmed` / `ProviderFault`）目前**没有承载通道**：
内核侧投递端是 `NopPath`，上报端是 `NopReports`。

在拿不到设备真实状态之前写一个「等 N 秒就自动 re-arm」的策略，等于用一个计时器
决定机器什么时候可以重新动起来。**那比不做更危险**——它会让系统看起来有恢复
能力，而实际上那个能力建立在一个与现实无关的假设上。

### 落地顺序

1. [ ] 内核冻结 Safety Path 的承载方式（专用高优先级通道还是 Dispatch，
       `safety.proto` 注释里标着「留待冻结」）
2. [ ] 内核把 `NopPath` / `NopReports` 换成真实实现
3. [ ] 本服务据 Provider 上报的设备状态实现恢复策略
4. [ ] `BuiltinHandler` 扩签名以透传 typed error_detail，让
       `STOP_NOT_SETTLED`（等一会儿）与 `WRONG_STATE`（别试了）能被区分

第 4 条现在就影响本服务：re-arm 失败时只看得到 `FAILED_PRECONDITION`，
分不清该重试还是该放弃。内核侧已经把 reason 算出来了，只是传不出来
（见 `nervud/internal/safety/builtin.go` 的 `TODO(builtin-detail)`）。

## 权限

manifest 申请两项：

| 权限 | trust 门槛 | 用途 |
|---|---|---|
| `perm.safety.observe` | OEM | 读安全态 |
| `perm.safety.rearm` | Platform + `platform-release` 签名 | 解开停机锁存 |

后者是**全系统风险最高的权限**：运动权限误用只让机器动一下，它误用让整套安全
防护失效。因此与 `perm.authority.reboot` 同级。

**执法恢复后**（内核的 `permission.V1GrantAll = false`），本包必须真正拿到
platform-release 签名才能 Resolve 到这个接口——开发机上会以
`PERMISSION_DENIED` 失败，这是预期的，不是回归。
