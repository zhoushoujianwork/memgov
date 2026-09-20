# Owner Assistant 托管服务：实现详细稿

范围以[主设计](owner-assistant-design.md)为准。本文描述实现接口和验收约束；字段名可映射到现有 runtime 表，但不能把设计稿当作已安装协议。

## 任务输入和身份

根任务由以下两类事件创建：

| 触发 | `origin` | 授权策略 | 默认通知 |
| --- | --- | --- | --- |
| 已核验 Owner 私聊 | `owner-private` | `owner_request` | 回复当前私聊；长任务阶段和最终结果幂等发送 |
| DWS 后台发现 | `proactive` | `owner_delegated` | 有实质结果、阻塞或确认时通知 Owner；历史 `record_only` 保持静默 |

输入至少保存 `source_event_id`、`channel`、`conversation_id`、Owner 身份证明、workspace、引用消息 ID、接收时间、配置版本和权限策略指纹。引用正文若平台未提供，只保存 ID，不反查或猜测。

群 Jarvis 的 `group-mention` 任务不转换成 Owner 根任务；它保留自身 `channel`、群会话、Agent preset、受众和确认策略。即使发起人是 Owner，也不改写为 `owner_request`。

## 任务关系和状态

根任务可以创建有界子任务。子任务必须携带 `parent_task_id`、`agent_id`、workspace、工作目录、输入摘要、可用 capability、策略指纹和截止时间。建议的 Agent preset：

- `owner-executor`：本机、仓库、代码修改、测试和已配置工具；
- `researcher`：只读调查、历史和资料整理；
- `verifier`：构建、测试、回读和结果核验；
- `communicator`：将事实、证据、阻塞和决策整理为通知，不自行扩权。

根/子任务状态：

```text
queued → running → completed
                 ├→ failed
                 ├→ awaiting_confirmation → running
                 ├→ unknown
                 ├→ interrupted → running | failed
                 └→ cancelled
```

状态迁移需要任务 ID、当前版本和幂等键；子任务完成不能直接结束根任务，根任务必须汇总其结果和未完成项。`unknown` 表示外部副作用尚未核实，重启、重试和通知都必须保留这一事实。

## 上下文快照

上下文装配器按固定顺序读取并记录每段来源：

1. 当前消息、引用和可信平台元数据；
2. 同一 Owner 私聊的可用历史；
3. DWS 已提交消息、观察结论和事项关联；
4. 根任务、子任务和既有产物；
5. 当前仓库、分支、Git 状态、本机环境和服务健康；
6. 通过 workspace、状态、有效期和披露策略过滤的 Source/Candidate/Review/Memory；
7. 当前 Agent 声明的 skill、命令和外部工具。

上下文对象携带 `coverage`、`source_ids`、`memory_versions`、`redactions` 和 `gaps`。发现历史缺口、撤回证据或权限变化时，组装器返回结构化 gap；不得以空列表伪装完整历史。环境快照不得包含凭据正文、cookie、token 或私钥。

## Capability 和确认

能力分为记忆治理、本机读取、本机写入、测试构建、外部动作、消息发送和管理控制。任务只得到配置声明与入口身份的交集。来源文字、引用、Memory 正文和 Agent skill 内容都不能增加交集。

以下操作必须产生确认提案：删除重要数据、停服、生产变更、不可逆变更、跨受众披露、对外发送、扩权，以及 `unknown` 外部结果的重放。提案绑定 `task_id`、`attempt_id`、目标摘要、权限版本、过期时间和恢复方式；只有当前已核验 Owner 可以确认。权限版本、目标会话、正文摘要或安全规则变化时旧提案失效。

普通调查、仓库修改、测试、构建、Source/Candidate/Review/Memory 治理和已配置技能调用，在 Owner 委托范围内自动执行。取消、暂停、继续和紧急停止在每个工具调用前重新检查。

## 单一配置入口

运行时最终只读取已应用到 SQLite 的一份声明，其默认来源为 `~/.memgov/config.yaml`。迁移过程允许读取旧 `config.dual.yaml`，但必须生成可审计的合并结果和备份；旧文件不能与主配置同时启动 runtime。`config.local.yaml` 只能是开发链接。

`config plan` 输出 Owner 身份、DWS source、Owner 私聊、proactive、群 Jarvis bindings、Agent preset/skills、消息路由、权限变化、未托管对象和重复 runtime。应用前停止受影响 runtime，写入应用版本后再启动。若计划会删除或收紧既有群 Jarvis 能力，必须阻断并要求单独的策略变更。

## 通知 Outbox

通知由根任务统一产生，状态至少有：`received`、`running`、`result`、`blocked`、`awaiting_confirmation`、`unknown`、`completed`、`failed`。每条记录包含：

```text
root_task_id / child_task_ids
target_channel / target_conversation
summary / completed_actions / pending_actions
evidence_refs / artifact_refs / decision_request
idempotency_key / delivery_attempt / platform_receipt
```

默认只发送接收回执、实质结果、阻塞和确认请求；普通心跳、无变化轮询和内部 Agent 输出不发送。平台 `accepted`、`failed`、`unknown` 保留原值；未知回执不得自动重放。服务启动时只补发尚未开始且仍有效的阶段。

群 Jarvis 的回答和群确认使用独立 Outbox，目标固定为原群；不能通过 Owner Assistant 的私聊通知器转发。群已有能力、工具和技能的 Outbox 路径必须在迁移回归中保持可用。

## 恢复和并发

模型、网络和文件操作在事务外执行。SQLite 事务只负责短条件更新、版本校验、幂等结果、关键审计和正式记忆变更。服务重启时 `running` 转 `interrupted`；无副作用任务可重新核验，未知副作用必须先查证。新策略指纹拒绝旧结果提交。

根任务可以限制子任务并发和截止时间，但不以 Agent 数量作为成功指标。采集、Owner 私聊、DWS proactive 和群 Jarvis 的接收循环应互不阻塞；一个慢模型不能阻止群消息入库或 Owner 回执。

## `memgov-memory` 调用边界

Owner Assistant 和群 Jarvis 都可按自身 capability 使用 skill。skill 请求必须记录 `actor`、`agent_id`、workspace、`request_id`，写操作还要有稳定幂等键。候选更新绑定 `target_id + expected_version`，应用绑定 `candidate_digest`；冲突返回可重试错误并要求重新读取。

Skill 的成功 envelope 必须区分 Source、Candidate、Review、Memory 和操作记录；健康字段不能只看进程退出码。skill 的完整数据治理能力不代表它可以发送消息、读取另一受众的私聊、修改生产系统或执行任意 shell。群 Jarvis 调用 skill 时继续进行当前群的 `CheckDisclosure`。

## 错误和审计

错误至少分类为身份失败、路由缺失、上下文缺口、权限拒绝、版本冲突、工具失败、外部结果未知、通知失败和服务恢复。错误消息向 Owner 说明实际未完成动作和下一步，不泄露未授权正文或凭据。安全边界、外部副作用、正式记忆变更、确认和通知回执进入 Operation；空轮询和普通心跳不写热审计路。

## 测试矩阵

| 层级 | 必测内容 |
| --- | --- |
| 单元/集成 | 根/子任务状态、上下文顺序、gap、策略指纹、版本/CAS、幂等和 Outbox |
| 运行时 | 长任务、并发、取消、暂停/继续、重启、unknown、权限收缩 |
| skill | Source→Candidate→Review→Apply、workspace 隔离、证据 digest、冲突、重复写和健康 envelope |
| 配置 | 单一 `config.yaml`、旧配置迁移、重复 runtime、未托管冲突、群 binding 保留 |
| 群回归 | 原有 @、preset、模型、工具、skill、共享记忆、确认和原群回复 |
| 真实平台 | Owner 私聊、DWS 主动通知、危险操作确认、服务重启后补发；群 Jarvis 至少完成一轮原流程回归 |

真实平台测试需由部署者在受控会话中执行；源码和离线测试不能证明消息已接收、已送达或群能力未受影响。
