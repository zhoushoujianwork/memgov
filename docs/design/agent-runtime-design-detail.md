# Agent preset 与 AI 值守运行时实现细节

面向用户的范围与使用方式见[主设计文档](agent-runtime-design.md)。

## Cyber 并发与可靠性（2026-09-18）

第一期源码：`runtime_scheduling.go` 在写事务内领取工作，Schema 23 增量保存租约、执行归属、进程组身份、绝对截止、阶段、重试时间及目录写锁。分析池与执行池独立计数，服务内多个主动 Runtime 共享总额度（冲突配置取运行/暂停/降级实例声明的最小值）。后台工作只派发已持久化的记录，无无限内存队列；交互模式不占后台额度。单会话最多一个未回收分析租约，按首条本地观察时间选择会话；执行期间仍可分析新消息。同任务未回收租约禁止再领取，即使旧任务已被取消。

执行截止从领取开始，包含准备、Agent、验证和记忆处理；审查子截止不得延长父截止。独立控制循环每 5 秒检查截止/任务版本，每 10 秒续 30 秒租约。Unix 调用独立进程组，取消先 TERM，5 秒后 KILL，并限制输出管道等待；启动恢复先核验旧 Owner 进程身份、回收旧组，健康执行者不能被另一 Runtime 接管。完成须通过租约、执行 ID、任务版本、来源证据和权限 epoch 校验。仅调度参数变化可在线应用，权限变化仍使旧执行失效。

分析暂时不可用或超时最多重试两次，退避 5/30 秒加小于 1 秒抖动，不占槽位；拒绝和格式校验失败保留失败证据。耗尽后消息标记 `analysis_failed`，不阻止同会话后续消息。任务超时保留失败状态和已有会话/流式输出，不自动重放。代码沿用独立 worktree，写根规范化并在 SQLite 内做包含关系互斥，读操作无需写锁。

配置增加 `analysis_concurrency`、`execution_concurrency`、`analysis_timeout_seconds`、`execution_timeout_seconds`、`review_timeout_seconds`；旧 `concurrency` 与新执行字段冲突时拒绝。并发 1–32，截止为 1–86400 秒整数；YAML 明确的 0、负数与非整数不能关闭超时。旧配置省略执行字段时保留 1，示例显式启用 8/4。调低额度仅限制新领取，已领取绝对截止不变。

`runtime status` 与管理台显示共享分析/执行/审查槽位、排队数、最老等待、最后成功分析、重试消息、失败缺口和超时累计；这些指标不把控制心跳当作模型进展。采集连续性继续使用独立来源覆盖与缺口；任务失败、阻塞、澄清和待确认明确标记需处理。

管理台 Runtime 列表增加一条全局健康快照 SQL，总查询次数固定为 3，不随 Runtime 数量增加；原有 1 秒成功缓存与刷新合并保留。第一期 `make check` 已通过，新增热降并发与跨 Runtime 配额测试也通过定向竞态检查。

验证：`TestSchedulingAtomicSharedPools` 验证 8 分析、4 执行、执行满载仍可分析、取消后回收前不释放槽位；`TestSchedulingRetryExhaustionAndDeadlineFence` 验证退避、耗尽缺口、迟到结果拒绝与后续消息推进；进程组测试使用忽略 TERM 且子进程持有管道的永久等待。定向 `-race` 已通过。120/900 秒真实时长超时、接收至分析 P95≤35 秒和连续 24 小时业务验收仍待部署者完成，不能以离线结果替代。

### 安装与运行观察

生产迁移、运行基线与长时间观察由部署者保留在仓库外。公开仓库只记录可复现的离线测试、协议边界和未解决项。

## 后台观察执行与记录边界

### 第三期调查、确认与记忆闭环

2026-09-18 源码增量（Schema 25）：`runtime_reviews` 单独保存候选、父执行截止、审查执行 ID、状态、错误类别及模型用量，任务新增 `memory_status/memory_error_code`。审查使用共享执行池、最多 2 个，普通待执行任务优先；截止为 `min(原任务截止, 领取时间+review_timeout_seconds)`，排队不能延长 15 分钟总预算。业务结果先原子完成并入审查队列；审查提交、独立 Review 和 digest Apply 均校验租约、任务版本、证据与权限，失败不覆盖业务结果。正在审查的工作中断后保留失败原因，不自动重放；未领取队列可恢复，过期父预算直接记录失败。

错误区分 `memory_permission_denied`、`memory_invalid_candidate`、`memory_evidence_unavailable`、`memory_version_conflict`、`memory_review_timeout`、`memory_review_unavailable` 与父预算耗尽。新路径由定向注错与有效/拒绝候选测试验证。

后台破坏性动作使用 `kind=destructive_operation`，`target` 为具体目标，JSON payload 包含非空 `operation/impact/recovery`。返回提案后任务进入 `awaiting_confirmation` 并释放执行槽位；其他历史受限动作仍为 `blocked`，不会因升级被重新授权。管理台在证据及版本有效时显示完整提案与口令。确认只接受已绑定应用机器人私聊中的新实时消息、当前已核验 Owner、完整口令及 payload digest；历史导入、上下文引用、撤销的身份、改版任务均不能批准。确认后独立派发、占执行池并受超时/租约/目录互斥控制；调用开始后失败或取消标记 unknown，先核验再继续，不自动重试。

系统提示统一要求破坏性操作具体确认，包括 owner_delegated 模式；普通调查、项目定位、聊天/附件查证、测试与本地提交按已有完整 Bash 和执行器技能能力执行，缺少工具必须点明具体缺项。完整 Bash 仍使用本机账户权限，并不构成 OS 沙箱。自动业务效果、附件读取能力及真实模型遵循提示的效果须通过实际案例验证，不能以工具开启代替验收。

会话调度使用首条接收时间，并在每次派发后推进该会话的虚拟就绪时间，让其他就绪会话先获得机会。调度参数更新不改变执行权限摘要，已领取任务继续使用原绝对截止；实际权限变化由 watchdog 和提交门禁拒绝。进程组 KILL 后等待退出确认；失败则保留租约和目录锁，需检查而非冒险重用。模型输出时间由实际流式输出更新，与 10 秒控制心跳分栏展示；非流式阶段没有输出时不伪造活动时间。

离线验证覆盖：有效候选完整闭环、审查拒绝及四类失败保留业务结果、缩短时限的超时回收、父预算不可延长、旧消息/引用/失效 Owner 不能确认、具体提案释放槽位及确认后领取。真实 120/900 秒永久挂起、并发负载 P95、真实附件/业务操作和安装后的 24 小时观察独立记录，未有证据前不标为验收通过。

### 第二期采集与时间核验

2026-09-18 源码增量（Schema 24）：DWS 适配器所有短请求共享 2 个槽位，排队计入单请求最多 30 秒截止，长连接不占查询槽位。群发现、实时收流、补漏与历史导入分别推进，单轮维护上限 2 分钟；补漏最多 2 个会话并发，每会话 30 秒。持久化轮转断点及失败会话的 5–300 秒指数退避，慢会话不阻塞其他会话。查询失败即使父截止已到也以短清理事务保留覆盖缺口；partial 仍沿用既有范围/证据有效性交集，不能扩大授权或宣称完整发现。

历史导入每步最多 100 条，拒绝越界适配器响应；消息同步每事务最多 100 条，清理继续使用现有 50 条批次。网络与文件工作在事务外，既有写入门禁与 250ms 慢写分段统计保留。管理台采集、模型与业务状态各自解释失败，不把补漏成功覆盖掉发现错误。正常停止造成的取消不新增会话失败退避，避免响应已返回但尚未提交时重启被误判为提供方故障；既有真实失败退避保持有效。

调度按 `messages.created_at`（本地接收）和接收顺序推进，不受来信未来时间影响；来源时间原值另存 `inbox_events.source_time_raw`，与既有 `parse_version`、原始 payload 一起核验。已证实的旧 DWS 历史无时区时间按 CST+8 解析及逐消息纠正沿用已有严格校验：同标识、正文、发送者、旧解析记录和精确 8 小时差缺一不可；不增加消息版本、不重触发历史任务。没有对来源时间做无条件减 8 小时或批量回放。新测试覆盖两适配器共享并发、慢会话隔离、超时缺口、本地接收排序；真实来源异常分布仍需安装后核验。

本轮源码实现、离线验证与新二进制安装已完成；未主动发送生产验收消息，真实业务效果仍待记录。统一协议见[钉钉接入详细稿](dingtalk-integration-design-detail.md#后台观察与机器人交互)。旧“输入齐全才执行 / 仅原会话回答 / 禁止完整 Bash”方案已被本节替代，`agent_origin_reply` 与 `runtime reply` 不作为现行协议。

执行顺序为：DWS 采集 → Haiku 判断处理价值 → 独立 Owner Agent 调查与执行 → SQLite 保存结果。独立任务复用 Owner Agent 的模型、技能、工具与策略验证，拥有自己的 task/attempt 和工作目录；不把结果交给主 Agent，也不复用 Owner 私聊会话。信息不足但存在可行调查方向可以执行；普通讨论、无新增事实的重复事项或没有可行下一步才留本地原因。

`completion_policy=record_only` 由 proactive 模式派生。成功、失败、clarification、awaiting_confirmation 及恢复均不得创建自动通知；旧待发 proactive 通知失效，发送和重试入口复核当前模式。历史记录保留，不转换成群回复。

新建默认策略为完整能力、Bash、执行器技能与 `external_actions=owner_delegated`，只适用于 proactive。显式 Agent 的限制仍生效；旧配置不自动获得更多能力。Agent 的独立沟通行为通过 `runtime message send` 保存目标、理由、证据和幂等回执，完成结果与沟通记录分别管理，详见[独立沟通工具](dingtalk-integration-design-detail.md#独立沟通工具)。

后台与 Owner 私聊共享执行能力，不共享授权入口。Owner 私聊的 `owner_request` 只接受已核验本人的请求；群 @ 不论发起人是谁都解析群策略。所有路径按当前配置、task/attempt、来源可用性复核。验收见[矩阵](../architecture/best-practice-scenarios-detail.md#后台观察与机器人交互验收)。

## 目录与 preset

`memgov agent preset enable <harness>` 在 `<MEMGOV_HOME>/agents/<name>` 创建独立 Git 仓库，文件权限为 `0600`，目录权限为 `0700`。受控规则入口由 harness 决定（Claude 为 `CLAUDE.md`，其他 harness 使用通用 `AGENT.md`），并统一包含 `agent.yaml`、`policy/memgov.md`、`README.md` 和 `.gitignore`；`runtime/` 被排除。

`agent.yaml` 的格式版本为 1，记录名称、provider、规则入口、运行目录和 enabled/disabled 状态。`status` 检查清单、受控文件是否被 Git 跟踪、`HEAD` commit 和工作树状态。`sync` 只接受不超过 1 MiB 的普通文件，拒绝明显包含 API key 或 token 的内容，并提交受控副本。

## 模型阶段

`runtime_configs.claude_profile` 可保存一个 zsh alias 名。每次调用模型前，运行时优先静态读取 `ZDOTDIR/.zshrc` 中的 alias 定义；未找到时才使用带超时的受限 zsh 读取，避免 launchd 下的交互式 shell 启动插件卡住 Agent。运行时只接受简单的 `ANTHROPIC_*` 和 `CLAUDE_CODE_*` 赋值，并把它们合并到 Claude 子进程环境。赋值之间使用 `&&` 或 `;` 均可，兼容 ccswitch 生成的两种常见 alias 形式。alias 本身不会执行，其他命令、参数和环境变量会被忽略；凭据不进入 SQLite 和日志。分析或执行模型为 `profile` 时不传 `--model`，由 alias 中的 Claude 默认模型决定。

### 群来源增量分析

分析器接收一个会话的新消息、最近上下文、本地消息 ID、发送者主体、本人消息标记及 Source/Fragment 证据。默认模型为 `haiku`，输出固定 JSON Schema：

- `task`：新任务；
- `update`：更新同一 `canonical_key` 的任务；
- `cancel`：取消已有任务；
- `memory`：提出可复用长期记忆；
- `context`：普通讨论。

分析器判断事项与 Owner 的相关性、处理价值及可行下一步，不要求启动前输入齐全。信息缺失可成为调查计划的一部分；无价值、重复或无可行下一步时记录原因。分析阶段禁用工具、MCP、Chrome、slash command 和会话持久化；真正的调查交给独立 Agent。

DWS 来源负责独立采集，机器人私聊和群 @ 由应用 Stream 接收。后台观察沿用数量/时间阈值。本人私聊与有效群 @ 只做身份、路由准入后直接调用 Agent，并共用“已收到”“处理中”“已完成”或“打叉”状态链；等待审批保持“处理中”。普通观察不展示任何读取或状态表情。每阶段使用独立 Outbox，启动恢复按任务状态补齐未开始的阶段。私聊协议见[所有者即时私聊](dingtalk-integration-design-detail.md#所有者即时私聊)，回执见[交互接收回执](dingtalk-integration-design-detail.md#交互接收回执)。

### 通道专属系统提示

执行前，运行时从任务的实际 channel、route、application mode 和已绑定 DWS profile 生成 `channel_system_prompt`。该值只来自 SQLite 中已核验的通道元数据，不拼接消息正文或可配置的聊天内容。本人钉钉 direct 提示规定：问题提到某人且答案可能依赖沟通时，先通过已安装的 `dws` 技能或获准 CLI 在当前企业和绑定 profile 内解析唯一联系人，再读取双方单聊；不先做广泛记忆召回，重名时先澄清，也不因读取而获得发送权限。明确询问长期记忆时先调用 `memgov-memory`。运行时会话目录只是临时工作区，不能据其内容判断正式记忆或钉钉历史是否为空。普通私聊回答由应用机器人回当前会话；Owner 明确要求另发且动作策略允许时，才提示使用绑定 profile 的 `dws chat +messages-send --as user --format json`，未绑定 profile 时禁止借用环境账号。群 mention 提示规定只使用本群上下文，普通回答由应用机器人回原群，禁止因人名转读成员私聊、使用所有者私有记忆或以 DWS 本人身份发送。其他模式只使用当前 route 已授权的上下文。

`channel_system_prompt` 进入 Claude 执行系统提示；本人持续会话的策略摘要也包含其 digest。通道提示变化会在下一轮关闭旧进程并建立新会话，避免旧通道行为继续生效。它只决定上下文检索优先级，不扩大 Agent 的工具、能力、受众或外部操作权限。

`message query CHANNEL` 查询 SQLite 中已经提交且尚未撤回的消息，可按精确会话、正文或发送者显示名、RFC3339 时间窗口和数量筛选。结果返回命中或指定会话的 `conversation_type`、`observed_at`、`covered_until` 与 `gap_unresolved`，并用紧凑摘要报告通道的有效会话数、类型、已观测数、未解决缺口数和最新观测时间；即使搜索零命中也保留摘要。显示名只参与检索，不作为身份或授权依据；水位有缺口、目标 direct 会话未进入观测范围或结果不足时，Agent 必须回到绑定通道补查，不能把空结果解释为平台上没有记录。查询结果仍是 Source/Observation 证据，只有经过候选、复核和应用后才成为正式 Memory。

### 群来源任务执行

执行器加载 preset 规则与任务消息。主动值守按任务 workspace 准备记忆；群 @ 不按标题预装载记忆，改由[群共享查询工具](group-memory-sharing-detail.md)按需读取当前受众可见内容。非 Git workspace 使用 `<MEMGOV_HOME>/runtime/tasks/<task>/<attempt>`；Git workspace 使用 `<MEMGOV_HOME>/runtime/worktrees/<task>/<attempt>` 和 `codex/runtime-*` 分支。

后台独立 Agent 使用 Owner 执行能力，依据实际 Agent 的 Bash、技能、目录和 external_actions 策略开放工具；显式受限 Agent 仍受其限制。代码任务按工作目录模式验证产物和 Git 状态，不能要求普通调查或问答必须制造提交。任务、尝试、实际模型、preset commit、结果、产物、工具类别和用量全部写入 SQLite。

交互回答在写入 Outbox 时加入受限长度的问题引用与耗时、模型页脚；后台 record_only 结果不进入此路径。独立沟通工具的正文和平台回执单独保存在 runtime_message_actions。引用、页脚和接收表情属于通道展示，不写入长期记忆。

### 长期记忆

`memory` 任务的执行结果必须包含 `CandidateInput`，其中 Evidence 引用消息快照对应的 Source 和 Fragment。运行时调用现有候选校验，再以独立 Haiku 调用生成 Review。接受后按 candidate digest 应用；拒绝记录在 Review 和 Candidate 中。撤回消息会让 Source 不可用，并取消相关未完成任务。

## 任务与版本状态

下图描述现行状态；新主动观察仅本地记录缺口和结束原因，不因进入这些状态而发送通知，也不在无实质变化时自动反复执行。

```text
pending → running → completed
                  → awaiting_confirmation → completed
                  → failed
clarification → pending（补充或显式 retry）

任何关联消息编辑：当前任务 version + 1，旧尝试 stale
任何关联消息撤回：当前任务 version + 1，任务 cancelled
外部动作结果不明：action_unknown
```

任务以 `(runtime_id, canonical_key)` 去重。跨批次补充修改原任务并增加版本。领取、完成和失败都检查版本及当前状态；执行期间消息变化会拒绝旧结果。更新或取消任务时，未执行的确认动作变为 `stale`；正在执行的外部动作变为 `unknown`。

启动恢复执行以下操作：

- `analyzing` 批次标记失败，关联消息退回 `pending`，允许重新分析；
- 本地 `running` 任务和尝试标记 `failed/runtime_restarted`，群任务随后发布失败回执，由任务 retry 或 resume 恢复；
- 已确认接收的 Owner 私聊任务由机器人发送一次幂等失败通知；直接 Agent 单轮最长运行 30 分钟，超时进入同一失败收尾链路；
- 失败通知包含白名单化的错误代码和安全原因分类（例如 Agent/模型/本地进程不可用、超时或权限拒绝），不把 Provider stderr、凭据、URL 或原始异常文本发送到聊天；
- `executing` 外部动作和 `sending` 投递标记 `unknown`，禁止盲目重发。

Schema 21 源码增加人工 `task resume`：保留原尝试与工作目录，新任务按原生会话 ID 恢复并发送“继续”；旧任务用原请求和已有进度继续。权限、来源和上下文仍需复核。用户说明见[中断后继续任务](task-continuation.md)，协议与验证见[详细稿](task-continuation-detail.md)。

## 外部动作确认

本节确认协议适用于群 @ 及显式收紧权限的 Owner 私聊。后台普通操作按 owner_delegated 委托自主执行；具体破坏性操作沿用上文专用提案门禁，无论是否委托均须 Owner 确认。其他显式受限后台动作或缺口仅本地记录，不生成私聊确认通知。

`external_actions=owner_confirmation` 的任务不能直接进行外部副作用，只能创建 `runtime_pending_actions`。动作保存 `kind / target / payload / payload_digest / task_version`。交付消息逐项显示动作和确认口令：

```text
确认操作 <action-id> <payload-digest 前 12 位>
```

确认验证读取 SQLite 中已经采集的消息，不接受调用方自报身份。消息必须：

1. 来自运行时配置的同一 channel；
2. 群任务必须位于该任务的触发原群，其他模式位于绑定的 owner direct route；
3. 发送者主体等于运行时 owner；
4. 消息仍可用，且时间不早于动作创建；
5. 正文与当前动作口令完全相同；已核验选中机器人的群消息可带一个开头 `@名称 `，不接受否定、引用或额外说明；
6. 任务版本和 payload digest 仍匹配。

确认后创建 `runtime_action_attempts`，由单独的 confirmed-action 执行模式只执行该具体操作。开始调用外部工具后若进程报错，保守记录 `unknown`。同一动作只有 `confirmed` 状态可被领取一次。

## SQLite 表

运行时业务状态使用：

- `runtime_direct_sessions`、`runtime_direct_turns`：私聊会话、命令和已交付轮次恢复关系；
- `runtime_configs`：会话、所有者、模型、preset、阈值和状态；
- `runtime_message_states`：每条消息在当前运行时中的 context/pending/batched/processed/recalled；
- `runtime_batches`、`runtime_batch_messages`：分析输入、摘要、模型和结果；
- `runtime_tasks`、`runtime_task_messages`：去重任务、版本和证据消息；
- `runtime_attempts`：本地任务执行；
- `runtime_pending_actions`、`runtime_action_attempts`：确认门禁和外部操作结果；
- `runtime_message_actions`：后台 Agent 主动沟通的目标、证据、理由和平台结果；
- 现有 `outbox`、`delivery_attempts`：交互回答、接收/处理/完成/失败阶段标记及未知投递状态；不承担 proactive 完成通知。

这些表参与备份和 JSON exchange。日志文件不用于恢复。

## 独立运行日志

日志位于 `<MEMGOV_HOME>/runtime/logs/<runtime-id>/`，与 preset Git 仓库分开。每条 JSONL 与 stdout 内容一致，`schema_version=1`，包含时间、级别、组件、事件、内部 runtime/batch/task/attempt/trace ID、状态、耗时、错误码、模型、token/费用、工具类别和安全摘要。

日志 API 不接受任意字段。摘要截断到 240 字，移除控制字符，遮蔽凭据形态和 URL。模型完整输入输出、聊天正文、子进程 stderr、凭据和平台稳定 ID不进入日志。无法分类的 Claude/dws 错误只记录受限错误码和固定摘要。

日志按 UTC 日期和 10 MiB 大小滚动，默认保留 30 天且总量不超过 1 GiB。清理不删除当前活动文件；活动文件本身导致配额无法满足时返回错误，运行时进入 `degraded`，停止新分析和执行，但继续接收消息并保留断点。目录为 `0700`，文件为 `0600`。

## 并发与故障边界

首期每个运行时只允许一个任务执行并发。SQLite 条件更新保证批次、任务、动作和 Outbox 只能被一个 worker 领取。通道租约阻止同一 channel 的两个接收器并行提交；运行时传给接收器的会话列表只包含 `collect` route 和 owner direct route，`mode=ignore` 的黑名单 route 不进入接收器。

### 空闲调度与写事务边界

运行时保留一秒调度检查以维持现有响应上界，但确认、消息同步、批次领取、确认动作领取和任务领取先通过 SQLite 只读查询判断是否存在实际工作。完全空闲、低于数量且未达到等待时间的批次、仍等待原生回执的本人消息、没有精确 Owner 口令的待确认动作，以及已有 attempt 占用并发槽时，不调用 `Store.Mutate`，不执行 `BEGIN IMMEDIATE`，也不向 `requests` 增加内部空轮询记录。

只读判断不是授权或领取凭据。判断为真后，原有写事务仍重新读取 runtime、route、版本、来源可用性、身份和当前状态，再通过条件更新领取；判断与事务之间出现的变化只能使事务无操作或冲突，不能重复领取、越过确认或扩大受众。进程重启继续从 SQLite 状态恢复，不依赖内存通知。

这项边界用于消除后台空转对平台消息持久化、接收表情、任务完成和投递的写锁排队；实际业务变化、幂等结果和外部操作仍按 `synchronous=FULL` 提交。后续若引入事件唤醒，它只能减少只读检查，不能替代持久状态或事务内复核。回归测试要求连续空闲 tick 不增加 `requests`，并覆盖新消息待同步、低于阈值后按时间唤醒和执行中任务不重复领取。

dws 长连接中断后按 1 秒退避重连。接收会话使用 `all-group` 订阅；每次轮换前以 30 天时间窗、最多 10 页消息刷新活跃群集合，把新活跃群注册为 `collect`，将沉默群移出 runtime 处理集合，并保留已有 `ignore`。启动时立即对账，随后每个周期最多对 20 个活跃群执行 5 页、200 条的一小时有界历史窗口，并保留 5 分钟重叠。只有完整窗口推进覆盖水位。

## 验证矩阵

自动测试覆盖数量/时间触发、首次历史上下文、空批次、编辑/撤回、任务版本冲突、精确私聊与所有者同群确认、跨群/私聊/其他成员拒绝确认、动作单次领取、恢复、Git preset、worktree 提交检查、dws bot 能力和未知发送、日志 Schema/权限/过滤/follow/轮转/保留/配额，以及 Stream ACK 和故障隔离。

真实企业验收还需用目标企业的 profile、robot code、监听会话和 owner 身份执行收发联调，并核对机器人权限及费用数据。


## 按群解析 Agent 与目录快照

`applied_configs` 中的最新声明是托管 Agent 的规则来源。先按任务 channel 选择 applications.bots 中的机器人，再按 Owner 私聊或群入口选择 Agent；群入口按精确会话匹配 bindings，无覆盖才使用 group_mention.agent/default_agent，再回落 bot.default_agent。旧 applications.owner_private/group_mention 仍兼容；同一通道或 runtime 的冲突声明拒绝应用。非托管实例继续使用自身 runtime 配置。解析包含 preset、profile、model、memory、capabilities、directories、Bash、skills 和 external_actions，不读取旧配置当作当前授权。

执行前重新检查所选 preset 已启用且 Git 工作树干净，并把实际 preset 名称、commit、模型及策略摘要写入 `runtime_attempts`。任务开始和结果提交前再次核对策略。任意已引用 Agent 的声明改变都会使该群运行时旧任务、待确认操作和待投递结果失效。确认后的外部操作同样解析所选 Agent，避免回落到默认 Agent 的接入配置。

群目录语义为输入快照：需要显式 `local_read`，只读取声明绝对路径中的 UTF-8 普通文件；原路径不加入 Claude 的工作目录。快照最多 200 个文件、合计 2 MiB、单文件 128 KiB，拒绝符号链接和特殊文件，跳过隐藏文件/隐藏目录、`node_modules` 及非 UTF-8 文件。快照保存在任务的 `inputs/<序号>/` 下，文件权限 `0400`，并作为任务数据提供给模型。使用 Go rooted filesystem 读取，目录内路径不能逃逸出声明根目录。目录路径本身必须是解析符号链接后的真实绝对路径。

默认 `bash=false` 的群模型仅可根据 `artifact_create` 或 `local_write` 使用 Write/Edit，权限统一以 `Edit(./artifacts/**)` 限定到任务产物目录；不开放自由 Bash，关闭项目/用户 settings 加载及 MCP。`local_test` 不能间接开启 Bash。Write 的路径权限应使用 Edit 规则，具体语义参见 [Claude Code 文件权限说明](https://code.claude.com/docs/en/permissions#read-and-edit)。显式 `bash=true` 的独立群 Agent 开放完整 Shell，因此不再具有这些文件工具规则所暗示的完整隔离；禁止同时配置目录快照。

交付前逐个校验产物为 `artifacts/` 内的普通文件，拒绝 URL、越界路径、输入快照和符号链接。未声明 `memory_read` 或 `conversation_history_read` 时，不提供对应额外上下文。群记忆始终为 `conversation_published`；所有者非空 directories 采用下述独立复制边界。

该能力已在当前源码实现并通过离线测试；目标企业实际验收仍是独立步骤。


## 所有者声明目录的执行门禁

所有者 Agent 的非空 `directories` 开启独立复制模式。声明目录是只读来源，必须有 `local_read` 能力；`local_write` 只授权本次任务的工作副本，`artifact_create` 只授权任务的产物目录。模型不获原目录的 `--add-dir` 授权。所有目录、workspace 和 Git 顶层都要通过真实绝对路径与父子边界检查，拒绝符号链接、同名前缀兄弟目录，以及只授权子目录却读取整个仓库的情况。

非 Git 任务将快照放在 `inputs/`，工作副本放在 `work/<来源序号>/`，产物放在 `artifacts/`。只读输入文件为 `0400`，工作副本和产物为任务内普通文件。沿用每任务 200 个文本文件、合计 2 MiB、单文件 128 KiB 的限制；隐藏文件、依赖缓存和非 UTF-8 数据不提供给模型。

代码任务从已授权仓库建立本地私有克隆，再从该克隆创建 `codex/declared-*` worktree。克隆保留 HEAD 祖先，但不复制源仓库的 hooks 或本地 Git 配置、不使用 hardlink、不修改原仓库或原工作树。源仓库元数据拒绝符号链接、外部 object alternates、外部 common-dir，最多 256 MiB、10 万条元数据项。模型看到的代码以克隆 HEAD 为基准，原目录未提交内容不会自动应用到 worktree。所有者原本的未提交修改保持原状。

该模式模型只能 Read 私有工作区，以及按能力 Edit `work/`、`artifacts/` 或代码 worktree；`.git` 与 `.claude` 控制路径另行拒绝读写。关闭用户/项目 settings、MCP 和其他工具，不开放 Bash。项目脚本的文件访问无法由命令前缀可靠约束，在接入并验证 OS sandbox 前，`local_test` 或 `bash=true` 在非空声明目录中会明确阻止配置和运行。没有声明目录的任务也必须显式开启 Bash 才能执行自由 Shell。

## 统一系统提示与安全验证

状态：2026-09-16 源码已实现；范围以[主文档](agent-runtime-design.md#统一系统提示与安全规则)为准。对应最佳场景中的主动值守受控处理、群内 @ 受众隔离和长期经验可信复用。

`internal/sysprompt` 通过 `go:embed` 内置规则，由同一个 `Compose` 函数组合 preset、入口提示、公共自我定位和安全规则。任务目录、聊天、记忆、工具输出不能指定加载路径。preset 是补充规则，可以提供机器人或群的专属人设，但不能覆盖运行时权限、公共自我定位或安全规则；工具开关及身份、受众、确认门禁继续由运行时代码控制。

| 文件 | 使用入口 |
| --- | --- |
| [identity.md](../../internal/sysprompt/identity.md) | 全部入口的简短基础定位；按用户语言说明 AI 工作伙伴角色，只列当前可用能力，不冒充底层模型或工具产品 |
| [security.md](../../internal/sysprompt/security.md) | 全部入口的统一攻击、破坏、外传与记忆污染规则 |
| [analysis.md](../../internal/sysprompt/analysis.md) | 主动观察筛选 |
| [direct.md](../../internal/sysprompt/direct.md) | 已核验本人私聊 |
| [proactive.md](../../internal/sysprompt/proactive.md) | 后台独立 Agent |
| [group.md](../../internal/sysprompt/group.md) | 群内有效 @ |
| [execute.md](../../internal/sysprompt/execute.md) | 普通任务执行 |
| [confirmed-action.md](../../internal/sysprompt/confirmed-action.md) | 一次已确认外部操作 |
| [review.md](../../internal/sysprompt/review.md) | 独立记忆复核 |

公共规则区分当前已核验请求与资料中的指令，不按危险关键词直接封禁。要求在操作前核查目标、影响、受众、权限及可恢复性；对未授权破坏和外传拒绝执行，对范围不清的合法维护澄清必要信息。区分 `owner_request`、`owner_delegated` 与 `owner_confirmation`，不把资料内容当作新的授权。筛选阶段将嵌入攻击作为 context；复核阶段拒绝伪造授权和试图控制未来行为的候选。上述语义由模型遵循，不是新增的确定性攻击分类器。

私聊恢复不再把已接受对话或热词拼入 `--append-system-prompt`。新进程首次 stdin 的 user message 使用独立 text block 携带 JSON 编码的恢复数据，末尾 text block 保留当前请求；无背景数据时仍用原始字符串。同一存活进程后续轮次、原生 resume 均不重复注入背景。引用仍按原有规则 JSON 编码并标为不可信。恢复数据上限为 128 KiB，超过后在模型调用前失败；系统提示另有 128 KiB 上限。stream user message 结构参考 [Claude Agent SDK](https://code.claude.com/docs/en/agent-sdk/typescript#sdkusermessage)，本次未调用真实模型验证该部署版本。

整个 sysprompt 内容摘要进入私聊与任务继续的策略指纹。升级规则后，旧任务继续会因策略不一致而拒绝；重新发起请求使用新规则，不带入旧原生任务权限。既有会话的已交付历史仍可作为不可信恢复数据。SQLite 继续作为恢复关系和审计的唯一真相源，prompt 文件不存放聊天或任务状态。

维护流程：修改公共或入口 Markdown → 运行下列检查并审阅 diff → 提交 → 在获准更新的环境构建、安装并重启服务。只编辑仓库文件、重启旧二进制或同步 preset 均不会启用新的内置规则。

```bash
go test ./...
go vet ./...
go test -race ./internal/runtime
git diff --check
```

验证覆盖：六个模型入口均带同一份公共安全规则；攻击文本不能进入 system 参数；私聊当前原文与 JSON 背景独立；存活进程不重复历史、引用更正触发重建、超限上下文不调用模型。样例包括中英文角色伪造、外传与删除请求、JSON/分隔符伪装、编码指令、记忆污染和正常安全分析。fake runner 仅验证传输与工具配置，不把模拟响应当作模型安全判断。既有确认、受众、目录及任务恢复门禁由全量测试回归。

剩余缺口：真实模型对攻击的识别率、正常维护误拒率和多轮攻击仍需在无生产凭据的隔离环境实测，并记录模型及版本；完整 Bash 仍使用运行账户权限，prompt 无法保证阻止所有越权访问。不得把离线通过写成真实钉钉、安装二进制或运行中服务的安全验收。

## 会话 Bash 与能力映射

Schema 19 的历史迁移曾将群与 proactive 设为 false/owner_confirmation，该已有显式限制继续保留。本轮新建 proactive 和 Owner direct 默认完整 Bash，分别默认 owner_delegated 与 owner_request；群默认 false/owner_confirmation。RuntimeConfigInput.agent_bash 使用可缺省布尔，显式 false 保留。YAML 中显式声明 Agent 的 bash 省略仍为 false；未指定 proactive Agent 或 bot Owner 私聊 Agent 才生成完整默认策略。

`applications.bots.<channel>` 以机器人 channel 为键，default_agent 提供基础人设。owner_private 引用已核验 direct runtime；省略 agent 时继承 bot 默认 preset/profile/model，并生成完整 Owner 权限，不继承群的 conversation_published 限制。group_mention.agent 可覆盖 bot 默认，bindings 再按群覆盖。显式 Owner Agent 可收紧。旧 applications.owner_private 仍可引用现有实例，省略 Agent 时保留实例人设。owner_request 仅允许 direct；owner_delegated 仅允许 proactive；群只接受 owner_confirmation。DWS history_channel 可只用于身份核验，不要求启用 data_source；没有群历史来源时只用机器人当前上下文。

| 字段 | 封装与生效方式 |
| --- | --- |
| `bash` | Claude Bash 工具的自由 Shell 开关。true 使用完整 Bash，不用命令白名单或全局 bypassPermissions。false 不能通过 local_test 等间接开启；本人私聊仅保留固定路径的受控记忆/提案包装器。 |
| `local_read` | 本人普通模式配置 Claude Read/Glob/Grep；群与 directories 模式只提供声明范围的输入。 |
| `local_write` | 配置 Edit/Write；目录模式和群产物仍使用相应文件工具范围。 |
| `local_test` | 表达测试能力，但不授权自由 Shell；测试命令需要 bash=true。 |
| `artifact_create` | 群任务可在 artifacts 内写入交付产物。 |
| `memory_read` | memgov 自有记忆能力。本人私聊将内置 SKILL.md 与命令指南复制到会话的 .claude/skills/memgov-memory，Agent 决定是否调用；其他任务按该能力提供受控记忆上下文。 |
| `memory_scope` | 决定受控记忆接口的范围；群只能 conversation_published，本人可用 owner_authorized。 |
| `external_actions` | owner_confirmation 使用交互提案/确认；owner_request 仅用于已核验 Owner 私聊的明确请求；owner_delegated 仅用于后台观察的 Owner 预设授权。来源文字不能改变任何策略。 |

完整 Bash 将真实 memgov CLI 加入 PATH，不使用受限包装器代替。它可以调用其他命令、读写运行账户能访问的文件；memory_read、文件工具规则和命令包装器不能成为此模式的完整隔离边界。能力开关不等同于 OS sandbox。

有效策略包含 Bash 和 external_actions，并进入任务策略摘要及原生 Claude 会话指纹。重配置使旧任务/结果失效；再次发送消息时关闭旧权限进程，建立新进程。`runtime status` 显示持久化策略，私聊 `/status` 显示本人策略。日志仅记录工具类别、耗时和结果分类，不记录命令、输出或正文。普通私聊继续原文传递，不强制预召回、提案或本地 commit。

模型完成后，后端验证产物是可写根内的普通文件，拒绝输入来源路径、符号链接和越界路径。私有代码任务执行 `git diff --check`，对实际更改创建本地 commit；这仅代表 Git 差异检查通过，不声称运行了项目测试。记录的 commit 位于本次私有 clone/worktree，可由所有者审阅后决定后续处理。

Claude/确认动作执行前后都复核任务与 attempt 绑定的 applied configuration version。配置变化、任务撤回或取消后，旧结果不得继续提交或投递；在变更前已经开始的外部操作保持结果不明的保守状态，不自动重发。

2026-09-17 群卡片增量：群回答 @ 发起人，待确认卡片 @ DWS 所有者。已发布关联应用的审批模板，仅所有者可同意或拒绝；拒绝后任务取消且动作不可执行，操作变更不继承旧批准，口令不能绕过卡片。源码与离线验证已具备，真实群按钮往返待验收，详见[确认卡片](dingtalk-integration-design-detail.md#群回复与确认卡片)。


## Per-Agent home and CLAUDE.md

`agents.<name>.home` is normalized to an absolute path during config loading. When omitted, runtime uses `<MEMGOV_HOME>/agent-homes/<name>`. The runtime creates the directory with mode `0700` and a regular `CLAUDE.md` with mode `0600`, then passes the directory to Claude with `--add-dir`; the preset repository remains Git-controlled and clean.

The execution prompt treats `CLAUDE.md` as durable Agent-local notes, not authorization. Only Agents with `local_write` receive exact `Edit(<home>/CLAUDE.md)` and `Write(<home>/CLAUDE.md)` allowlist entries; group Agents do not receive arbitrary home-file access. Notes must be non-secret and bounded by the existing conversation and disclosure policy. Memgov memory tools remain explicitly on demand rather than being queried on every turn; an empty result is not evidence that the library is empty.
