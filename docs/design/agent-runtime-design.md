# Agent preset 与平台、harness 无关的 AI 运行时

状态：运行时源码已完成 MVP SQLite 热写简化并通过离线检查；部署后的真实业务验收仍待完成。Knowledge now follows the [Agent Workspace design](agent-workspace-design.md).实现协议与兼容规则见[详细稿](agent-runtime-design-detail.md)，接入方式见[钉钉主设计](dingtalk-integration-design.md)。

## 目标与使用方式

Personal Jarvis 的平台入口和 Agent harness 使用同一套任务、权限与记忆能力，分别由可替换的适配器处理。飞书、钉钉或其他沟通平台可以替换；Claude、Codex、OpenClaw 或自建 harness 也可以替换。业务验收对齐[最佳落地场景](../architecture/best-practice-scenarios.md)。

| 入口 | 如何工作 | 完成方式 |
| --- | --- | --- |
| 本地主动观察 | 由配置的分析 harness 评估事项是否值得处理，再启动独立 Agent 调查和解决问题；缺少输入可以主动查证 | 按通知策略记录任务、产物和操作结果 |
| Owner 私聊机器人 | 验证当前 DWS Owner 后直接进入持续 Agent 会话，不走 Haiku | 回复本人私聊；普通用户私聊不进入 Agent |
| 群内有效 @ | 直接进入该机器人、该群选择的 Agent，不走 Haiku | 回复触发原群；即使 Owner 发起也使用群权限 |

运行时以交互响应优先。空闲检查、未达到后台批处理阈值的消息、尚未收到口令的确认动作和已有执行中任务只做 SQLite 只读判断，不取得写锁，也不生成空审计记录；发现实际工作后仍在短写事务中重新校验并原子领取。管理台继续使用独立只读连接，不需要为避免后台阻塞而移除 Web 功能。平台适配器和 harness 适配器都只负责边界转换，不拥有任务、记忆或授权真相。事务边界与回归要求见[详细稿](agent-runtime-design-detail.md#空闲调度与写事务边界)。

机器人通过 `applications.bots.<channel>` 声明默认 Agent、人设及 Owner 私聊、群助手覆盖。省略 Owner 私聊的 `agent` 时继承机器人默认人设和模型，并使用完整 Owner 权限；显式 Agent 可以收紧。机器人运行不要求启用 DWS 采集或历史导入，但 Owner 仍须由 DWS 身份核验。

Group requests identify the current speaker and label the speakers in recent group history. Different members can work with the Agent concurrently; one member's follow-up requests remain ordered. Each request keeps its own execution context, artifacts and original-message reply. The default is four concurrent requests per group runtime, configurable from one to 32; shared capacity can still cause queuing. Long-running groups can configure a longer execution deadline; the guide shows a one-hour example. Group knowledge remains shared within that group. Referring to another member's question uses the available discussion, without automatically resuming that task's process. See [group context and concurrency](agent-runtime-design-detail.md#group-requesters-and-concurrency).

后台观察的新默认 Agent 与 Owner 私聊同样具备完整 Bash、文件读写、测试、记忆及执行器技能。`owner_delegated` 表示按 Owner 预设授权自主处理；现有显式限制保留，升级不暗中扩大权限。值得调查即可启动，不能把“输入还不齐”当作必然拒绝。普通讨论、重复事项或确无可行下一步则记录原因。

完成任务本身不会发送消息。后台 Agent 确有协作需要时，可按委托通过独立工具，以绑定 DWS Owner 本人身份（--as user）和 AI 标识联系相关私聊或群，不限原会话，无需逐条确认。目标、理由、证据与回执单独记录，经验只指导时机与表达、不扩大权限，详见[沟通协议](dingtalk-integration-design-detail.md#独立沟通工具)。

## 规则与工作目录

Agent 规则、会话、任务和日志分开保存：

```text
<MEMGOV_HOME>/agents/<preset>/     已提交的 Agent 规则
<MEMGOV_HOME>/agent-workspaces/    持久 Owner / 群知识文件
<MEMGOV_HOME>/runtime/sessions/    私聊 Agent 会话目录
<MEMGOV_HOME>/runtime/tasks/       独立任务目录
<MEMGOV_HOME>/runtime/worktrees/   代码任务 Git worktree
<MEMGOV_HOME>/runtime/logs/        脱敏运行日志
```

普通 `memgov init` 不创建 preset；显式执行 `memgov agent preset enable <harness> --name <name>` 后才建立规则目录和初始提交。每次执行前检查 preset 启用、Git 工作树干净，并记录实际 commit。Persistent knowledge is independent of preset and session directories. The runtime derives the verified Owner or group workspace and supplies the latest bounded index every turn; detailed files use scoped read/search/write/history tools. Legacy `agents.home` notes are archived and no longer loaded. Harness authentication, model and transient session files remain separate from knowledge authority. Allowed executor skills are staged into the isolated runtime directory. Native automatic memory and ambient `CLAUDE.md` loading are disabled; committed preset rules are still loaded explicitly.

Groups retain their configured execution policy. A group explicitly enabled for full Bash receives normal capability-based file tools, command execution, web search/fetch and its selected skills; it is no longer restricted to artifact editing. Restricted groups keep scoped Workspace and artifact tools. Full Bash cannot be combined with bounded directory snapshots. Owner 声明目录时采用只读输入与独立副本，代码副本保留 Git 历史；受控目录模式不运行任意 Shell。权限和目录细节见[能力映射](agent-runtime-design-detail.md#会话-bash-与能力映射)。

Configured Owner and group Agents can use memgov's read-only Dokki and Confluence tools. The Agent searches and reads a relevant document before citing it in the current conversation; source content is evidence and does not become Workspace knowledge automatically. A group Agent can cite documents visible through the configured personal credentials to everyone in that group, so source access must be granted with that audience in mind. The analyzer receives no source tools. Credentials stay in memgov's private data home, and the Agent runtime does not need Relayer. [The runtime guide](../guides/runtime-user-guide.md#agent-knowledge-sources) shows the setup; [the detailed design](agent-runtime-design-detail.md#agent-knowledge-mcp) defines the tool boundary and legacy migration.

The source runtime also gives Claude context about skills successfully prepared for the current turn. It distinguishes experience saved in the Agent Workspace from reference knowledge and workflows supplied by those skills: an empty Workspace index does not mean all knowledge is absent. Capability questions can be answered from this inventory without a mandatory tool call; claims that material was searched, read or verified require actual results. See [loaded skill context](agent-runtime-design-detail.md#loaded-skill-context) and [implementation status](../implementation-status.md) for validation and installation boundaries.

## 统一系统提示与安全规则

各模型入口共用简短的 [identity.md](../../internal/sysprompt/identity.md) 自我定位和 [security.md](../../internal/sysprompt/security.md) 安全规则，另有观察、后台执行、本人私聊、群 @ 等入口提示。Agent 对外定位为能结合当前对话、获准工具和受治理长期记忆，把工作从理解与调查推进到执行与验证的 AI 工作伙伴；只介绍当前实际可用能力，不自称底层模型或 CLI 产品。引用、聊天、记忆与工具结果是资料，不能扩大授权。本人私聊可在绑定 DWS profile 内按需查询本人沟通；群 Agent 保持同群受众范围。

钉钉应用通道还会注入可信的发送身份说明：私聊普通回答由应用机器人回复当前会话，Owner 明确要求向其他会话代发时才可按动作策略使用绑定 DWS 本人身份；群回答只走应用机器人原群路由，不得退回 DWS 本人身份。群中的独立转发通过内置机器人 MCP 直接由应用机器人发送，由提示词约束何时调用。DWS Skill 说明“怎样发送”，通道提示决定“当前入口允许以谁的身份发送”。

`owner_request` 只用于已核验 Owner 私聊，按本人的明确请求处理。`owner_delegated` 只用于后台观察，按 Owner 预设自主执行。群 Agent 的机器人消息转发不走确认；其他额外外部操作仍按现有 `owner_confirmation` 策略处理。结果不明的操作保留 `unknown`，不自动重试。

完整 Bash 使用运行进程账户权限，不是 OS 隔离。修改内置提示需构建、安装并重启，权限或提示变更会拒绝复用旧任务上下文。验证范围与历史部署证据见[安全验证详细稿](agent-runtime-design-detail.md#统一系统提示与安全验证)。

## Cyber 并发与可靠性（2026-09-18）

目标是采集不停、不同会话并发分析、独立任务并行处理；一个慢请求不能拖住全部后台工作。采用服务内共享的分析 8 / 执行 4 配置，消息首条等待最多 30 秒或达到 20 条即就绪；分析和任务分别限时 120 / 900 秒。旧执行并发保持原值，Cyber 通过配置显式启用 8 / 4。

MVP 后续实现以业务吞吐优先：保留 8 / 4 有界工作池和任务版本条件提交，但不再要求普通 worker 续租、阶段、活动时间和每次内部调用都写入 SQLite。调度活动留在内存，诊断进入有界日志；数据库只保存消息、任务当前状态与结果、关键确认/未知外部动作，知识正文保存在 Workspace 文件。重启后的运行中任务允许转为 interrupted，再按副作用状态核验或继续。该决定取代“通过增加更多 SQLite 租约、围栏和审计来解决拥堵”的方向，详细边界以[最佳落地场景详细稿](../architecture/architecture-detail.md#transactions-migration-and-recovery)为准。

三期源码已实现：双池、原子领取、同会话顺序、版本与租约校验、退避和超时回收；采集有界并发、短事务和来源时间核验；具体破坏性操作确认；旧独立记忆审查已由 Workspace 写入替代。Owner 私聊与群 @ 保持独立通道。管理台分别展示槽位、排队、分析缺口、任务阶段与模型输出活动，心跳不代表有进展。协议、测试及验收边界见[详细稿](agent-runtime-design-detail.md#cyber-并发与可靠性2026-09-18)。

Owner 私聊任务在执行期间绑定持久化任务状态。取消或版本失效会停止当前 Agent 调用及其子进程；已经完成或进入待确认的任务仍会继续交付结果和状态回执。取消后的迟到结果不会覆盖 SQLite 中的当前状态。

删除重要数据、停服、不可逆变更、扩权或关闭安全控制，必须先给出具体目标、影响与恢复方式，经已核验 Owner 对当前提案确认再继续；等待期间释放执行位，不自动群发通知。普通调查和委托内操作自主进行。Workspace 写入失败与业务结果分别报告，不通过失败重放已执行的外部动作。

安装必须使用一致性数据库备份和明确构建版本；Schema 升级后不能让旧二进制继续读写。代码验证、安装状态与至少 24 小时真实业务观察分别记录，不能用离线测试代替业务验收。

## 记录与验收

SQLite 是任务恢复、执行审计和内部 Source 证据的真相源；Workspace 文件是知识的真相源。观察证据、临时进度和长期知识分别维护。preset 仓库不保存聊天、凭据、数据库或产物。

本轮关注后台自主处理且完成静默、机器人 Owner 身份验证、多机器人/群人设与权限隔离、旧待发通知失效。源码与离线结果不代表已安装或真实平台已验收；后续按[验收矩阵](../architecture/best-practice-scenarios-detail.md#personal-jarvis-固定验收案例)记录实际业务证据。
