# CLI 契约 v1

本文是 Personal Jarvis 的 CLI/API 运维契约。默认活动配置只有 `~/.memgov/config.yaml`；仓库内 `config.local.yaml` 只作为开发入口链接，历史 `config.dual.yaml` 只用于迁移备份。CLI、管理台和 `memgov-memory` skill 共享同一 SQLite `state.db`，不建立第二套任务或记忆存储。沟通平台适配器和 Agent harness 都是可替换的运行边界。

这里的 v1 指 JSON 输出契约，不是数据库版本或旧产品模型。现行对象为 Source、Candidate、Review、Memory 及其证据、版本和操作记录；使用总览见 [README](../../README.md)。旧版的原子、卡片、战斗及索引导入命令不属于本契约。

先用 `memgov version`、`memgov --help` 和子命令 `--help` 确认当前二进制的能力。开发中的设计稿不构成已交付命令清单。

## 输出与作用域

Schema 23 源码增量：`runtime setup` 与声明式 `applications.proactive` 支持分析/执行并发和分析/执行/审查超时字段；默认及兼容规则见[运行时协议](../design/agent-runtime-design-detail.md#cyber-并发与可靠性2026-09-18)。`runtime status` 新增 `work` 调度指标；原有 runtime/task 字段保留。二进制未升级时不能仅凭本页传入新参数。

默认 stdout 一行 JSON，形如：

```json
{"schema_version":1,"request_id":"UUID","ok":true,"data":{}}
```

失败时 ok=false，error 包含 code/message。`cached:true` 表示已重放幂等结果。每次调用使用新的 request_id；重放不会再次执行写入。成功 envelope、数据库 schema、AI prompt 各自有版本。

退出码：0 成功；1 internal；2 invalid_input；3 conflict；4 not_found；5 unavailable（含期限、锁等待）；6 denied。doctor/status 返回数据本身的健康状态，应检查相应字段。

`--input <path|->` 接受单个 JSON 值，最大 32 MiB。命令 Schema 拒绝未知字段。stdout 只包含结果，帮助、Shell 补全或显式文本格式输出相应文本。无强制交互确认。

读记忆默认当前工作区加 global。写入必须选择对象所属工作区；不能把项目来源提升成全局证据。search/list 的 `--all-workspaces` 是显式跨工作区查询。recall 默认只返回 active、当前作用域及已知有效期内的记忆。

## YAML 配置

`config show` 显示解析后的系统默认值、`runtime_setup`、`logging`、`channels` 以及 Personal Jarvis 的 `data_sources`、`agents`、`applications` 声明，不解析密钥引用。`config validate` 离线检查字段、系统参数与通道身份；路由策略和工作区在应用事务中校验。两者均不初始化数据库。未知字段、重复键、多文档 YAML 和错误类型返回 `invalid_input`。

`config apply NAME` 从所选 YAML 的 `channels` 中读取一项，事务内写入 SQLite，不访问平台；该命令不接受 `--input`。已有通道要求 `--expected-version`（`config_version`）和 `--reason`；包含已有路由时另需 `--expected-route-version`。不能改变通道所属企业、个人账号或应用身份。配置内容参与幂等摘要，更新会清空能力记录、使旧草稿失效。完整字段和使用方式见[初始化与配置](../guides/initialization.md)。

## 配置计划与独立数据源

`config plan` 预览统一 Personal Jarvis YAML 的声明、已应用版本、披露边界及自动挂载，`config apply-runtime` 绑定计划摘要与版本执行应用；保存 YAML 不等于配置已生效。计划输出必须能说明 `ready:true`、Owner 身份与权限边界已确认、没有 unmanaged conflict、没有重复 runtime、没有未授权权限扩张。应用前停止依赖中的 runtime，应用后再启动统一服务。具体输入与授权沿用[接入详细稿](../design/dingtalk-integration-design-detail.md#双模式-yaml-声明与受限应用)。

`data-source status/start/pause/resume/stop` 管理独立采集；`data-source history create/list/show/cancel/retry` 查询和推进可恢复历史导入。逐实例启动是兼容诊断入口，统一服务运行时不能再并行启动同目录独立实例。查询及七天保留边界见[保留设计](../design/direct-message-retention-design.md)。

## 统一服务与管理台

`service install/uninstall/start/status/stop/restart` 管理同一数据目录内的服务。macOS 的 install 安装并启动当前用户 launchd 托管，保存绝对程序与配置路径、工作目录及固定端口（默认 8787，不能为 0），支持 `--no-ui`。进程退出及稳定的二进制替换自动恢复。安装后 start/restart 在后台启动并等待心跳，使用已保存设置；修改设置需重新 install。stop 禁用并卸载运行中的 job，start 再启用；uninstall 移除 plist 并保留数据、日志。status 保留服务状态字段，并在有托管时增加 manager 对象。未安装托管时 start/restart 仍为前台运行，支持 `--port 0`，restart 可迁移旧独立进程。YAML 配置变更仍需显式应用。详见[统一服务](../design/unified-service-design.md)。

托管入口启动前自动为受支持的旧 Schema 创建并校验一致性备份，成功后事务迁移；备份位于 `<home>/backups/service-upgrades/`，同版本重启不重复备份。备份失败、数据库独占锁超时或不受支持的版本阻止启动，日志保留具体原因。普通前台 start/restart 与其他命令仍需显式 `init` 升级，YAML 仍需显式应用。

`ui --port PORT --open` 独立提供本机查询及 Agent YAML 声明编辑，不启动采集或迁移数据库。统一服务注入额外的 Web 重启和失败任务继续回调，独立 `ui` 不提供这两个控制；只读实时过程输出与独立诊断日志分开，见[管理台指南](../guides/local-console-user-guide.md)。

管理台 `GET /api/v1/version/check` 查询固定仓库的 GitHub tags，返回 state、checked_at 及可用时的 latest_version/tag_url。包含预发布 tag，按 SemVer 比较；不要求 Release。该查询独立于 meta 和业务读取，成功结果缓存 15 分钟，无 tag 或失败缓存 1 分钟；本机安装文件一致性仍以 meta 的 build/installed_build 为准。

## 记忆列表

`m` 是 `memory` 命令组的别名：`memgov m list`、`memgov m show ID`、`memgov m history ID` 与完整命令等价；参数、版本检查、审计和幂等语义相同。

现有 `memory list` 按更新时间倒序排列，时间相同按 ID 升序；默认 50 条。支持工作区、分类和状态过滤，不指定 status 时返回全部生命周期状态，不能把管理列表当作只含有效 active 记忆的 recall。

当前返回对象尚未展示系统创建、更新时间，也不支持显式排序参数。时间展示和可选排序见[记忆列表设计](../design/memory-list-design.md)，这些扩展尚未实现。

## 候选

`candidate submit --input -` 的输入：

```json
{
  "action":"update",
  "target_id":"原记忆 UUID",
  "expected_version":3,
  "reason":"新来源纠正了适用范围",
  "memory":{
    "category":"procedure",
    "title":"测试环境发布检查",
    "summary":"仅在测试环境采用该检查方法。",
    "content":"## 前提\n测试环境，具备发布权限。\n## 步骤\n先检查权限，再查看错误日志。\n## 验证\n生产环境验证情况未知。",
    "applicability":["仅适用于来源记载的测试环境"],
    "entities":["发布系统"],
    "tags":["部署"],
    "evidence":[{"source_id":"来源 UUID","fragment_id":"片段 ID","sha256":"该片段返回的 SHA-256"}]
  }
}
```

create 省略 target_id/expected_version。Memory 内不得指定 id/version。applicability、entities、tags 为字符串数组。时间用 RFC3339，不确定时省略。证据 ID 和指纹必须来自 source show 或领取的任务，不得自行猜测；sha256 使用所引用片段的指纹，不是整份来源的指纹。quote 可省略，提供时必须逐字匹配来源片段。

`candidate validate` 只做结构、作用域、证据和版本校验；不代表语义复核。`candidate apply ID --expected-digest DIGEST` 必须绑定已读过的完整候选。旧候选引用的目标版本已改变时返回冲突。

`memory history` 的每项保留当时版本正文，并附 operation（理由、执行者、请求 ID、操作时间和前后版本）。`candidate reviews` 返回独立复核意见及其绑定指纹。

## 通道、消息与受众

`channel add --input -` 只写本机配置，不连接平台。凭据用 `credential_ref` 引用 keychain://、env:// 或 file://；内联密文一律拒绝。dws 个人通道与应用机器人通道使用各自的身份与授权命名空间，互不代用。新建绑定默认最严：`audience_policy=local_private`、`memory_policy=explicit_only`、`send_policy=draft_only`、`approval_display=display_only`。受众键按 `policy:channel:conversation` 逐条会话隔离，默认的 local_private 同样如此；向一个会话发布不会披露给同通道的其他会话。

`dingtalk_app` 通道由官方 Stream 接入：`channel probe` 只在平台确认订阅后记录 `receive`，并在凭据可解析时启用主动机器人 `send`；`history` 保持未验证。`channel run` 写库成功才向平台确认收到；写入失败即停止接收。统一服务在新消息提交后广播进程内合并唤醒，相关 Runtime 立即从 SQLite 领取，1 秒扫描兜底；唤醒不替代数据库真相。无法解析的帧落 inbox 记为 rejected，跨租户或指向其他机器人的帧只记原因不存正文。会话 webhook 不入库、不进日志或模型，只在统一服务进程内按通道、会话和原消息短期保留；普通群答复优先用它发送原生 `@`，过期、重启或无匹配时通过应用 access token 发送一次普通 Markdown 降级。主动群消息接口不支持原生 `@`，不得添加会被平台静默忽略的寻址字段。私聊、确认卡片和原消息表情仍走各自应用接口。

机器人富文本支持纯文字、混合图片与带链接的文字段；图片和链接目标只作为未读取附件标记入库并随任务显示，不存下载凭证或 URL，也不执行媒体读取。不认识的富文本元素仍标记 rejected。详见[混合富文本说明](../design/dingtalk-integration-design-detail.md#机器人混合富文本)。

`channel plan`、`channel doctor`、`channel status` 全部离线，返回中明确 `creates_subscription:false`、`sends_message:false`、`online_checked:false`。`channel probe` 是唯一记录“已验证能力”的命令，会以该通道自身身份连接平台；登录身份与配置不一致时返回 denied。未验证的能力按不存在处理，`channel pull`/`run` 因此返回 unavailable。

`channel pull CHANNEL --conversation ID` 按半开区间 `[start,end)` 回填。省略 `--start/--end` 时从水位续读并带 `--overlap` 重叠，因为平台投递与本机提交不是同一个事务。窗口未读到尽头即记为缺口，`covered_until` 不推进；complete 但起点晚于已覆盖范围时，中间未读区间单独记为未闭合缺口。`channel status` 列出水位、覆盖窗口与仍未闭合的缺口。

`channel run CHANNEL` 前台运行接收会话并持有通道租约，每次获取都推进 fence；写入前重新校验租约，失去通道的接收者无法继续写入。事件先提交再计数，写入失败即停止接收而不确认收到。无法解析或不支持的事件写入 inbox 并标记 rejected，不会丢弃。`channel ingest CHANNEL --file -` 离线导入规范化 NDJSON 事件，强制 `origin=import`，不推进覆盖范围，且不能声称未经显式映射的在线身份。

`channel route update ROUTE --expected-version N --reason TEXT` 修改绑定策略，重算 audience_key 并把旧策略下的草稿置为 stale；不允许改指到另一个会话。`message identity link CHANNEL --basis` 只接受 platform_directory、operator_confirmed、same_open_id，显示名不构成依据。被撤回消息在 `message list`/`show` 中不展示正文，也不会被 `message query` 命中。`message query CHANNEL` 可按会话、正文或发送者显示名、RFC3339 半开时间窗口查询已提交观测，并返回命中或指定会话的详细水位及通道覆盖摘要；显示名只能用于检索，即使零命中也保留覆盖摘要，空结果不能越过水位缺口证明平台记录不存在。

`audience check CHANNEL MEMORY-ID --conversation ID` 解释某个记忆能否向该会话披露，拒绝时逐条给出原因。`audience publish` 只对该受众和该版本放行披露，返回 `sent:false`——它不发送任何内容；`audience unpublish` 收回许可并作废相关草稿。记忆版本变化后需重新 publish。`audience list/recall/show` 在该会话受众范围内查询；群查询不返回被拒绝记忆的标题、正文或证据。共享工作区与排除分类也参与披露校验，详见[群共享查询](../design/group-memory-sharing-detail.md#查询)。

## 回复草稿与投递

`reply open CHANNEL --input -` 由可信连接建立 RequestContext，绑定真实主体、目标会话、受众和路由版本，并带有效期。外部 Agent 只能在该上下文内工作：`reply recall CONTEXT QUESTION` 的受众来自上下文本身，没有可以扩大范围的参数；`reply draft --input -` 只接受正文、格式和所引用的记忆 ID，目标与发送身份由上下文和通道决定。上下文过期或路由版本变化时，草稿被拒绝而不是沿用旧权限。

草稿的每条引用都在写入时按当前受众重新检查，不可披露即整份拒绝。同一上下文与同一输入只产生一份草稿，重复提交返回既有草稿并标记 `duplicate`。

`outbox preview DRAFT` 是本期的展示契约：待发内容、目标会话、实际发送身份、逐条引用及其当前可披露性、检查项、`display_digest`，以及固定语义的 `approval: {mode: display_only, status: not_evaluated}`。展示不发送、不推进状态、不写投递尝试，也不产生任何“已审批”记录；检查全过只代表技术条件成立。`outbox list/show` 同样只读。

发送是独立开关：路由默认 `draft_only`，`send` 能力未验证时也不可发送，二者缺一 `outbox dispatch` 返回 denied。`outbox dispatch DRAFT --expected-digest ...` 必须绑定展示时读到的摘要，事务内重新执行全部检查、置为 sending 并先记录发送尝试，再由 `outbox result DRAFT STATE` 记录平台回执。`accepted` 必须带回执；缺回执的结果是 `delivery_unknown`，`retry_safe:false`，不自动重发。已交给平台的草稿不能本地取消；`outbox reconcile DRAFT --outcome ... --evidence ...` 只接受带实际核对依据的结论，不伪造平台送达证明。

## 幂等与并发

`--idempotency-key` 按命令和工作区隔离，绑定参数、文件内容和业务输入；同键不同输入返回 conflict。更新使用 expected-version；合并和 purge 使用整份预览的 expected-digest。

采集命令是例外：`channel pull`/`run`/`ingest` 一次调用会提交多个独立事务（每条事件、覆盖记录、租约各自提交），幂等键作用在单条事件而不是整条命令上；重跑同一窗口按事件去重，计入 duplicates。租约的获取与释放不使用幂等键，否则新接收者会拿到早已失效的令牌。

备份是文件操作：使用幂等键时文件名由键派生，位于默认 backups 目录，此时不接受自定义 --output。恢复前用 backup verify 取 SHA-256，再传 expected-digest。恢复请求也支持幂等键，重放不会覆盖后来工作。

`memory retire ID --expected-version N --reason TEXT` 追加 retired 版本。`memory restore ID --expected-version N --reason TEXT` 追加 active 版本；加 `--revision V` 可恢复历史 V 的内容为新版本。

## AI 值守运行时

`agent preset enable <harness> [--name NAME]` 显式创建受控 Git 规则目录；省略名称时使用 `<harness>-default`，同名目录不能跨 harness 复用。`status/sync/disable` 分别检查、提交规则副本和停用；`sync --from-policy FILE` 适用于任意 harness，`--from-claude-md` 是 Claude 兼容入口。普通 `init` 不创建 preset。`runtime harness [name]` 只读显示已注册 harness 的契约状态，不初始化数据库、不调用模型。

`runtime configure --input -` 输入 name、channel、route_ids 和 owner，可选 Agent 策略、claude_profile、模型、preset 与调度参数。Personal Jarvis proactive 任务使用配置的结果通知策略；历史 `record_only` 任务继续只记录。direct/group_mention 派生为 `reply_to_trigger`，并核验对应私聊/原群出站路由。group_mention 的 context_channel 可省略；提供时仍要求同企业同群。owner 必须是 DWS 已验证稳定身份。修改已有配置需要 expected version，running 状态不能修改。harness 和平台适配器由运行时注册表选择，群 Jarvis 仍保留其独立 Agent、技能、工具、记忆范围和原群回复路径。

`runtime start ID` 是持续前台命令，输出 `schema_version=1` 的 JSONL 日志；它不使用普通 JSON envelope。统一服务提供提交后即时唤醒，独立前台入口仍以默认 1 秒扫描消费。`runtime pause/resume/stop` 修改持久状态。`runtime restart ID` 先写入 stopped 状态，等待当前机器上同名的旧 `memgov runtime start` 或 `memgov runtime restart` 进程退出，再由当前命令以前台流模式启动；等待超过全局 `--timeout` 时失败且不启动并行进程。pause 和日志 degraded 状态继续采集消息，但不创建新 AI 任务。

DWS 来源独立采集并评估后台事项，值得处理即可创建 Owner 根任务并启动有界 Agent；新 Personal Jarvis 任务在有实质结果、阻塞或需要确认时向已核验 Owner 私聊通知，`record_only` 历史任务仍只记录。机器人仅已核验 Owner 私聊及有效群 @ 直接进入相应 Agent，不走 Haiku。任务以独立 Outbox 记录“已接收”“处理中”“有结果/被阻塞/等待确认/已完成/已失败”等状态；通知正文包含任务摘要、已完成动作、证据和产物、未完成动作、决策项、外部回执、根/子任务关系、幂等键和投递结果。阶段平台调用在有序旁路队列执行，不作为 Agent 启动前置条件。`awaiting_confirmation` 保持处理中，服务启动按任务真相补齐未开始的阶段。群任务继续以原群 Jarvis 的身份、受众、能力和路由回复；Owner 在群里也不继承私聊授权。普通用户私聊拒绝进入 Owner Agent。

`runtime task list/show/cancel/retry/resume` 使用普通 envelope。`runtime task confirm ACTION --input -` 接受 `{"message_id":"本地消息 UUID"}`，卡片已展示的动作拒绝此命令绕过按钮；旧口令流程从 SQLite 验证该消息确实来自任务原群的所有者（群助手）或绑定 owner direct route（其他模式）、晚于动作且正文完整匹配确认口令；平台已核验的群 @ 可带一个开头 `@名称 `；不接受自报 sender 或 origin。

Schema 25 增加独立记忆结果 `memory_status/memory_error_code`，状态列表增加 `work.queued_reviews`。proactive 仅具体 `destructive_operation` 提案可进入确认；需非空具体目标及 payload 的 operation/impact/recovery。确认须为已核验 Owner 在显式绑定机器人私聊中的新实时原始消息，不能使用历史、引用或失效版本；其他既有后台 blocked 动作不会恢复确认或自动执行。审查占共享执行额度，最多 2 个，父任务绝对截止仍生效；其失败不改变业务完成结果。

`runtime task resume <task-id>` 继续失败任务：复用有效原生会话或按原请求与已有成果恢复，增加任务版本并新增尝试；`--expected-version` 可绑定调用方看到的版本。`retry` 则从头准备。原文失效、私聊 clear、权限变化或未知外部结果会阻止不安全的继续；Schema 21 与具体条件见[继续协议](../design/task-continuation-detail.md)。

`runtime task propose-action <task-id> --input <file|->` 绑定当前尝试提出待确认动作；`runtime task capture-hotword <task-id> --input <file|->` 仅保存本人明确纠正的热词。两者校验任务与权限，不能视为通用外部动作或任意记忆写入入口，分别见[运行时边界](../design/agent-runtime-design-detail.md)及[热词协议](../design/hotword-agent-skills-design-detail.md)。

`runtime logs list/show` 使用普通 envelope；`show --task ID` 同时返回匹配事件与 SQLite 任务详情。`runtime logs follow` 只输出新增和已有匹配事件的 NDJSON。日志过滤支持 since、until、level、component、batch 和 task；时间使用 RFC3339。

### 后台 Agent 沟通工具

`runtime message send <task-id> --input <JSON-file>` 在当前 running proactive task/attempt、owner_delegated 策略与 DWS Owner 身份下执行独立沟通；`runtime message list <task-id>` 查询审计。输入包含 attempt_id、idempotency_key、target_type（group/user）、稳定原生 target_id、content、reason、evidence_message_ids。目标、实际披露证据及当前配置均复核；固定绑定 profile、本人与 AI 标识。同键同正文返回已有记录，改正文重用键冲突；失败或未知也不盲目重发。结果记录于 runtime_message_actions，和任务完成通知分离。默认 Personal Jarvis 通知走已核验 Owner 私聊；群 Jarvis 的结果仍走原群路由。完整协议见[独立沟通工具](../design/dingtalk-integration-design-detail.md#独立沟通工具)。源码、安装和真实平台状态以[交付状态](../implementation-status.md)为准。

## 检索与输出预算

search 对记忆和证据返回不同 kind。FTS 查询按字面短语匹配；中文短词补充子串检索。recall 的 `--limit` 与 `--budget-chars` 控制条数和 context 内 Unicode 字符数，JSON 元数据本身不计入上下文预算。items 包含版本、摘要、适用条件和完整证据引用；完整正文用 memory show 获取。

`--format markdown` 支持 recall 与 export，`memory graph ID --format mermaid` 输出关系图。其他命令的 text 输出为可读 JSON。Graph 的 --depth 上限 5，--limit 上限 1000。

应用通道 `identity.confirmation_card_template` 接受已关联应用的 `.schema` 模板 ID。原生卡片及 Stream 回调协议已实现，但当前运行服务暂时停用卡片入口，群 pending 结果使用原群完整确认口令；恢复卡片后只有同企业认证 DWS 所有者可“同意”或“拒绝”，同意后执行，拒绝后取消且不能切换决定。正文、@ 对象和展示动作冻结在 Outbox；模板、来源、动作或路由变化拒绝旧卡片。详见[确认卡片](../design/dingtalk-integration-design-detail.md#群回复与确认卡片)。
