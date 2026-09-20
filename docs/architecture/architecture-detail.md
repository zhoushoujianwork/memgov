# memgov 架构详细稿：Owner Assistant、任务和记忆治理

主文档：[总体架构](architecture.md)。本文记录实现时必须保持的接口、边界和验证约束；它是目标与实现约束，不是安装或真实平台验收证明。安装版本和运行观察见[能力状态](../implementation-status.md)。

## 模块职责

| 模块 | 职责 |
| --- | --- |
| `cmd/memgov`、`internal/cli` | CLI 入口、JSON envelope、配置计划/应用、记忆和任务命令 |
| `internal/core` | Source、Candidate、Review、Memory、任务、证据、版本、幂等和审计事务 |
| `internal/channel` | DWS、应用机器人和群路由适配；保存入口身份与受众，不把平台字段写进记忆模型 |
| `internal/runtime`、`internal/agent` | Owner 根任务、子 Agent 调度、外部模型调用、工具边界、恢复和确认 |
| `internal/sysprompt` | Owner、后台、群 Jarvis 等入口的基础身份和安全规则；不授予额外 capability |
| `internal/service` | 单一服务生命周期、配置加载、模块监督、停止、重启和 macOS 托管 |
| `internal/console`、`internal/observation`、`internal/runlog` | 本地查询、控制回调、健康/进展观测和有界脱敏日志 |
| `web/` | 管理台静态资源；复用服务业务接口，不另建任务状态机 |
| `memgov-memory` skill adapter | 将 Agent 的记忆请求映射到稳定 CLI/API 契约；不直接打开 SQLite |

一个统一服务可托管 Owner Assistant 和群 Jarvis，但必须按 `channel`、`conversation`、`application`、身份和策略隔离任务。共享进程或数据库不表示共享授权。

## 正式数据模型

SQLite `state.db` 是唯一真相源。现行对象如下：

- `Source`：带 URI、捕获时间、内容指纹和片段位置的不可变证据快照；
- `Candidate`：create/update 建议，更新绑定目标 ID 和期望版本；
- `Review`：对候选及证据的结构和语义复核；
- `Memory`：经过应用的可复用结论，带 workspace、类别、适用条件、有效期、证据、状态和递增版本；
- `Task`/`Attempt`：Owner 根任务、子 Agent 工作、输入版本、结果和恢复关系；
- `Operation`/`Outbox`：关键工具副作用、确认、消息投递、幂等键和平台回执。

任务进度是临时工作事实，不能直接变成 Memory；只有经过 Source → Candidate → Review → Apply 才能进入长期记忆。来源正文、引用文本、群聊天和模型输出都是不可信资料，不能改变执行或披露权限。

## Owner Assistant 任务图

目标任务图如下：

```text
root task (owner-private | proactive)
  ├─ context snapshot
  ├─ researcher       (只读调查，可选)
  ├─ owner-executor   (本机/仓库/已授权工具，可选)
  ├─ verifier         (测试、构建和结果核验，可选)
  └─ communicator     (整理通知，不扩大权限，可选)
```

根任务负责去重、派发、汇总、重试、暂停、继续、取消和最终状态；子 Agent 只能在父任务声明的 workspace、目录、工具和权限版本内运行。每个子任务保存父 ID、Agent 身份、上下文来源、工作目录、策略指纹、结果、证据和失败原因。实现可复用现有 runtime task/attempt 表，但不得让子 Agent 绕过根任务直接投递 Owner 通知。

任务状态至少能表达 `queued`、`running`、`awaiting_confirmation`、`completed`、`failed`、`interrupted`、`unknown` 和 `cancelled`。`unknown` 表示外部副作用是否发生尚未核实，不能自动重放。

## 统一上下文

每轮按以下顺序组装，并为每段记录来源和可见性：

1. 当前 Owner 私聊消息、引用和平台元数据；
2. 该 Owner 私聊的可用历史；
3. DWS 已提交的观察记录和关联事项；
4. 根任务及子 Agent 的进度和产物；
5. 当前仓库、工作区、分支、Git 状态和本机环境快照；
6. 受 workspace、状态、有效期和受众过滤的 Source/Candidate/Review/Memory；
7. 当前 Agent 明确声明的 skill 和外部工具。

历史缺口、权限收缩、证据撤回或来源不可用必须以结构化事实传入上下文。没有结果不等于没有历史。原文中的命令、提示或权限声明都只作为资料，不得覆写运行时策略。

环境快照至少包括 OS、运行用户、仓库路径和分支、Git 状态、允许访问目录、可用命令/runtime、服务进程和配置版本。凭据只通过 `credential_ref` 或受控工具读取，绝不把密钥正文放进 prompt、Source、Memory 或日志。

## Capability 与确认

Owner 委托范围内的读取、本机/项目修改、测试、构建、记忆治理和已配置工具可以自动执行。以下动作必须创建绑定当前任务、目标、权限版本和恢复方式的确认提案：

- 删除重要数据、停服或生产变更；
- 不可逆操作、跨受众披露和对外发送；
- 扩大目录、工具、网络或消息权限；
- 外部结果为 `unknown` 时的重放。

确认只能由当前已核验 Owner 完成。取消、暂停、继续和紧急停止都应落库并在每个工具动作前复核。权限版本或目标受众变化会使旧提案失效。群 Jarvis 继续使用自身已配置 capability；Owner Assistant 的策略迁移不应隐式替换群 preset 或关闭群工具。

## 配置单一入口

产品目标配置入口为 `~/.memgov/config.yaml`。服务、CLI、管理台、launchd 和验收脚本必须解析同一个有效配置来源。现有 `config.dual.yaml` 只作为迁移备份或兼容读取来源，不得与主配置并行生效；`config.local.yaml` 可以作为开发入口链接，但不能复制第二份声明。

`config plan` 必须在应用前报告 Owner 身份、运行对象、群路由、权限变化、未托管冲突和重复 runtime；应用前停止受影响 runtime，应用后再启动。未授权权限扩张、同名对象冲突、旧策略任务或未知路由必须阻断应用。群 Jarvis 的既有声明应在计划中被识别为独立对象，不能被 Owner Assistant 合并或删除。

## 通知与投递

Owner Assistant 的通知是根任务的派生交付，不是 Agent 任意调用消息 API。通知状态至少包括：`received`、`running`、`result`、`blocked`、`awaiting_confirmation`、`unknown`、`completed`、`failed`。默认只在有实质结果、阻塞或需要确认时私聊通知；无变化的心跳不发送。历史 `record_only` 任务保持静默兼容。

每条通知保存根任务/子任务 ID、摘要、已完成动作、未完成动作、证据/产物、需要的决策、幂等键、目标会话和平台回执。投递失败或回执未知必须明确标记，不凭模型输出声称已送达；重启恢复只能补发未开始的幂等阶段。群 Jarvis 的普通回复仍由挂载应用发回原群，失败结论和确认也留在原群，不转移到 Owner 私聊。

## `memgov-memory` skill 契约

Skill 是 Agent 到 memgov 的受控适配层，不是第二个运行时。所有请求都应携带或由宿主注入：

```text
actor / agent_id
workspace
request_id
idempotency_key（写操作）
source/candidate/review/memory 目标与版本
证据引用和调用原因
```

最低能力包括：

1. `recall`、`search`、`memory show/history` 和 Source 读取；
2. Source 创建与片段读取；
3. Candidate create/update、validate、show、diff；
4. Review 创建/读取；
5. Candidate Apply、Memory revise/retire/restore；
6. 证据、版本、操作和幂等结果查询。

写操作必须显式 workspace，引用真实 SourceFragment 和摘要值，使用 expected version/digest 防止并发覆盖；冲突时重新读取，禁止盲重试。Skill 返回统一 envelope 和健康字段，Agent 必须区分“来源命中”“候选待审”和“正式记忆已应用”。Skill 的完整记忆权限不会授予 shell、DWS 发消息、云 API 或生产操作；这些仍由宿主的 capability 和入口策略决定。群 Jarvis 可在自身配置允许时调用相同 skill，且查询结果继续经过群受众披露检查。

## SQLite 事务与恢复

业务写入、版本校验、幂等记录、审计和 FTS 触发器在同一短 `BEGIN IMMEDIATE` 事务内提交；事务外执行模型调用、网络访问、文件操作和验证。空闲轮询、普通心跳和无工作检查不进入写事务。数据库启用外键、WAL、`synchronous=FULL`、secure delete 和有界 busy timeout；数据库目录默认 0700、文件 0600。

服务重启时，遗留 `running` 任务转为 `interrupted`；无外部副作用的任务可重新核验后继续，外部结果未知的任务先查证。新权限或策略指纹不匹配时拒绝旧结果提交。迁移脚本只追加，已应用配置和 schema 版本均记录在 SQLite。

## 检索与披露

FTS5 是可重建索引，不能代替正式数据。查询先按 workspace、生命周期、有效期、证据状态和当前受众过滤，再按字符预算组装上下文。群 Jarvis 的共享记忆查询必须同时满足工作区共享和当前群披露许可；Owner 私聊可以使用其授权范围内的私聊历史和记忆。一个通道的可见性不能被另一个通道的提问、引用或 Agent 中转扩大。

## 验证要求

架构变更至少验证：单一配置启动、Owner 私聊根任务、DWS proactive 根任务、复杂任务的子 Agent 汇总、skill 的 Source→Candidate→Review→Memory 闭环、确认/取消/未知结果、服务重启恢复、通知幂等，以及群 Jarvis 原有收发、工具、技能、记忆和原群回复路径不受影响。源码测试、安装检查、真实模型和真实平台验收分别记录，不能相互替代。
