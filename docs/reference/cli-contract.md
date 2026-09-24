# CLI 契约 v1

本文是 Personal Jarvis 的 CLI/API 运维契约。默认活动配置只有 `~/.memgov/config.yaml`；仓库内 `config.local.yaml` 只作为开发入口链接，历史 `config.dual.yaml` 只用于迁移备份。CLI、管理台和 `memgov-workspace` skill 使用同一组 Agent Workspace 文件作为知识权威来源；SQLite `state.db` 保存任务、消息、权限、投递及操作元数据，不复制 Workspace 正文。沟通平台适配器和 Agent harness 都是可替换的运行边界。

这里的 v1 指 JSON 输出契约，不是数据库版本或旧产品模型。现行知识模型是按已核验身份分配的 Agent Workspace；内部 Source 和 fragment 继续支持消息证据、撤回和原文保留期；使用总览见 [README](../../README.md)。旧版 Source/Candidate/Review/Memory 公开命令、自动抽取与复核、群记忆发布及热词入库命令已删除。

先用 `memgov version`、`memgov --help` 和子命令 `--help` 确认当前二进制的能力。开发中的设计稿不构成已交付命令清单。

## 输出与作用域

Schema 27 replaces the old memory pipeline with Agent Workspace files. Analysis and execution concurrency/timeouts remain; review timeout, queued-review metrics, memory scope and memory-status fields are removed. This page describes source behavior; confirm the installed executable before using new commands.

默认 stdout 一行 JSON，形如：

```json
{"schema_version":1,"request_id":"UUID","ok":true,"data":{}}
```

失败时 ok=false，error 包含 code/message。`cached:true` 表示已重放幂等结果。每次调用使用新的 request_id；重放不会再次执行写入。成功 envelope、数据库 schema、AI prompt 各自有版本。

退出码：0 成功；1 internal；2 invalid_input；3 conflict；4 not_found；5 unavailable（含期限、锁等待）；6 denied。doctor/status 返回数据本身的健康状态，应检查相应字段。

`--input <path|->` 接受单个 JSON 值，最大 32 MiB。命令 Schema 拒绝未知字段。stdout 只包含结果，帮助、Shell 补全或显式文本格式输出相应文本。无强制交互确认。

Workspace access has no implicit global merge. Owner private chat and proactive work share the verified `OwnerPrincipalID` workspace; group knowledge is isolated by `channel_id + conversation_id`, independently of presets and project worktrees. Runtime tools bind the current task, attempt and audience and revalidate every operation. Only explicit read-only reference directories provide shared materials; knowledge cannot grant execution or disclosure authority.

## YAML 配置

`config show` 显示解析后的系统默认值、`runtime_setup`、`logging`、`channels` 以及 Personal Jarvis 的 `data_sources`、`agents`、`applications` 声明，不解析密钥引用。`config validate` 离线检查字段、系统参数与通道身份；路由策略和工作区在应用事务中校验。两者均不初始化数据库。未知字段、重复键、多文档 YAML 和错误类型返回 `invalid_input`。

`agents.<name>.knowledge_mcp.sources` enables memgov's built-in read-only tools for `dokki` and/or `confluence` on an Owner or group Agent. Bash is not required. Group defaults and exact bindings use the selected Agent's declaration; Agents without it receive no source tools. The YAML stores source names, not credentials. Legacy `command` and `args` remain accepted only as a matched, read-only pair for rollback. `config validate` checks the shape; `config plan` treats a newly admitted source or changed server mode as an authorization boundary change. `knowledge import-relayer --from <absolute SQLite path>` copies legacy credentials once into the private memgov home without printing them; `knowledge configure --input <private JSON path or ->` replaces them directly, and `knowledge status` reports presence only. See [runtime setup](../guides/runtime-user-guide.md#agent-knowledge-sources).

`config migrate-workspaces` converts the selected `--config` file once. It archives the original configuration and legacy AgentHome notes, removes retired memory/home/shared/review fields, validates the replacement, and preserves preset, skills, routes and external-action permissions. It does not apply runtime configuration or migrate the database. Normal parsing rejects unconverted legacy fields. After conversion, `init` performs the database upgrade; saving YAML still does not apply its declarations.

`config apply NAME` 从所选 YAML 的 `channels` 中读取一项，事务内写入 SQLite，不访问平台；该命令不接受 `--input`。已有通道要求 `--expected-version`（`config_version`）和 `--reason`；包含已有路由时另需 `--expected-route-version`。不能改变通道所属企业、个人账号或应用身份。配置内容参与幂等摘要，更新会清空能力记录、使旧草稿失效。完整字段和使用方式见[初始化与配置](../guides/initialization.md)。

## 配置计划与独立数据源

`config plan` 预览统一 Personal Jarvis YAML 的声明、已应用版本、披露边界及自动挂载，`config apply-runtime` 绑定计划摘要与版本执行应用；保存 YAML 不等于配置已生效。计划输出必须能说明 `ready:true`、Owner 身份与权限边界已确认、没有 unmanaged conflict、没有重复 runtime、没有未授权权限扩张。应用前停止依赖中的 runtime，应用后再启动统一服务。具体输入与授权沿用[接入详细稿](../design/dingtalk-integration-design-detail.md#双模式-yaml-声明与受限应用)。

Both `applications.group_mention` and `applications.bots.<channel>.group_mention` accept `execution_concurrency`, an integer from 1 to 32, defaulting to 4. Apply writes this capacity to the group runtime instead of fixing it at one. Different group requesters are independently eligible; one requester in one group remains ordered. This changes scheduling, not group permissions or reply destinations. Existing deployments need a reviewed configuration apply to replace their stored capacity. Both group forms also accept `execution_timeout_seconds` (integer 1–86400, default 900); `3600` sets one hour. Configuration apply persists the selected duration for future task and confirmed-action claims. Zero, negative and non-integer values are rejected; an unlimited deadline is not supported.

`data-source status/start/pause/resume/stop` 管理独立采集；`data-source history create/list/show/cancel/retry` 查询和推进可恢复历史导入。逐实例启动是兼容诊断入口，统一服务运行时不能再并行启动同目录独立实例。查询及七天保留边界见[保留设计](../design/direct-message-retention-design.md)。

## 统一服务与管理台

`service install/uninstall/start/status/stop/restart` 管理同一数据目录内的服务。macOS 的 install 安装并启动当前用户 launchd 托管，保存绝对程序与配置路径、工作目录及固定端口（默认 8787，不能为 0），支持 `--no-ui`。进程退出及稳定的二进制替换自动恢复。安装后 start/restart 在后台启动并等待心跳，使用已保存设置；修改设置需重新 install。stop 禁用并卸载运行中的 job，start 再启用；uninstall 移除 plist 并保留数据、日志。status 保留服务状态字段，并在有托管时增加 manager 对象。未安装托管时 start/restart 仍为前台运行，支持 `--port 0`，restart 可迁移旧独立进程。YAML 配置变更仍需显式应用。详见[统一服务](../design/unified-service-design.md)。

`init` and managed-service upgrades archive and verify the supported old schema before destructive migration. The archive includes a consistent `.db`, a `.knowledge.tar` of effective/default configuration and legacy AgentHome notes (including configured and applied homes), and a digest-bound manifest. A failed archive, verification failure, unsupported version or database lock failure blocks migration. Configuration conversion must happen first. Schema migration and cancellation/resource release for unfinished old-memory work occur in one transaction; unknown external delivery results stay unknown. New workspaces start empty. Other commands do not silently migrate the database. See the [upgrade and recovery guide](../guides/governance.md).

`ui --port PORT --open` 独立提供本机查询及 Agent YAML 声明编辑，不启动采集或迁移数据库。统一服务注入额外的 Web 重启和失败任务继续回调，独立 `ui` 不提供这两个控制；只读实时过程输出与独立诊断日志分开，见[管理台指南](../guides/local-console-user-guide.md)。

管理台 `GET /api/v1/version/check` 查询固定仓库的 GitHub tags，返回 state、checked_at 及可用时的 latest_version/tag_url。包含预发布 tag，按 SemVer 比较；不要求 Release。该查询独立于 meta 和业务读取，成功结果缓存 15 分钟，无 tag 或失败缓存 1 分钟；本机安装文件一致性仍以 meta 的 build/installed_build 为准。

## Agent Workspace files

The maintained skill is `memgov-workspace`. A local operator can use:

```bash
memgov agent workspace list
memgov agent workspace list --workspace-id WORKSPACE_ID
memgov agent workspace read --workspace-id WORKSPACE_ID --path MEMORY.md
memgov agent workspace search --workspace-id WORKSPACE_ID --query TEXT --limit 30
memgov agent workspace history --workspace-id WORKSPACE_ID --path notes/topic.md
memgov agent workspace write --workspace-id WORKSPACE_ID --input -
```

`list` without a selector returns workspace identities; with `--workspace-id`, it lists that workspace's current files. Other operations require a workspace selector. Runtime-bound calls instead use `--task TASK_ID --attempt ATTEMPT_ID` together, without `--workspace-id`. The harness supplies these bindings; the model cannot choose another identity or data home. Expired or cancelled tasks, stale attempts, revoked policies and cleared private sessions cannot keep using their old binding.

Write input is one JSON object:

```json
{"path":"notes/topic.md","content":"# Topic\n\nObserved 2026-09-23. Source: task/example.\n","expected_digest":""}
```

`expected_digest` is required. Empty means create-only; updating requires the digest returned by `read`. A cross-process lock protects the digest check and atomic file replacement. On conflict, read again and merge. The write result contains file metadata, while `read` returns metadata plus `content`. SQLite auditing/idempotency stores operation metadata and digests, never a second copy of the knowledge body.

Paths are relative Markdown paths: `MEMORY.md`, or files under `notes/`, `projects/` and `daily/`. Absolute paths, traversal, hidden metadata and symbolic links are rejected. Keep `MEMORY.md` within 8 KiB and individual files within 256 KiB; split oversized content. File listing supports at most 2,000 files. Search is a literal, case-insensitive path/content match; `--limit` defaults to 30, accepts 1–100, and the search content budget is 16 MiB. It searches current files, excluding history and legacy archives.

`history` returns up to the latest 100 revision metadata entries, with digest, previous digest when present, actor, request ID, time and byte count. Revision bodies are preserved separately under `<home>/agent-workspace-history/`; the command does not expose an arbitrary filesystem path. Revisions do not replace the current file as the knowledge authority.

The console exposes read-only endpoints under `/api/v1`: `GET /agent-workspaces`, `GET /agent-workspaces/{id}/files?query=...`, `GET /agent-workspaces/{id}/file?path=...`, and `GET /agent-workspaces/{id}/history?path=...`. Writes return method-not-allowed. Old memories APIs return not-found. This local operator browser does not broaden a runtime Agent's scope.

## 通道与消息

`channel add --input -` 只写本机配置，不连接平台。凭据用 `credential_ref` 引用 keychain://、env:// 或 file://；内联密文一律拒绝。dws 个人通道与应用机器人通道使用各自的身份与授权命名空间，互不代用。新建绑定默认最严：`audience_policy=local_private`、`send_policy=draft_only`、`approval_display=display_only`。受众键按 `policy:channel:conversation` 逐条会话隔离，默认的 local_private 同样如此；路由权限不会因 Workspace 文件内容而扩大。

`dingtalk_app` 通道由官方 Stream 接入：`channel probe` 只在平台确认订阅后记录 `receive`，并在凭据可解析时启用主动机器人 `send`；`history` 保持未验证。`channel run` 写库成功才向平台确认收到；写入失败即停止接收。统一服务在新消息提交后广播进程内合并唤醒，相关 Runtime 立即从 SQLite 领取，1 秒扫描兜底；唤醒不替代数据库真相。无法解析的帧落 inbox 记为 rejected，跨租户或指向其他机器人的帧只记原因不存正文。会话 webhook 不入库、不进日志或模型，只在统一服务进程内按通道、会话和原消息短期保留；普通群答复优先用它发送原生 `@`，过期、重启或无匹配时通过应用 access token 发送一次普通 Markdown 降级。主动群消息接口不支持原生 `@`，不得添加会被平台静默忽略的寻址字段。私聊、确认卡片和原消息表情仍走各自应用接口。

The hidden `bot-mcp --task-id <id> --attempt-id <id>` stdio server exposes exactly three group-Agent tools: `resolve_bot_user(name)`, `resolve_bot_group(name_or_id)`, and `forward_bot_message(target_type, recipient, content)`. It verifies the claimed attempt, bot channel, same-tenant bound DWS directory and active mounted group routes. User resolution requires one stable `userId`; group resolution accepts only a uniquely matched mounted group. Forwarding writes a durable message action before sending through the application bot's DM or proactive group Markdown API, with the action ID as its idempotency key. It requires no Owner confirmation and never uses DWS personal sending. Repeating the same task, recipient and content returns the recorded state without another send. Platform acceptance is not proof of delivery, and unknown outcomes are not retried automatically. Ordinary same-group replies remain on the existing Outbox route. This server does not expose cards or reactions to the Agent.

机器人富文本支持纯文字、混合图片与带链接的文字段；图片和链接目标只作为未读取附件标记入库并随任务显示，不存下载凭证或 URL，也不执行媒体读取。不认识的富文本元素仍标记 rejected。详见[混合富文本说明](../design/dingtalk-integration-design-detail.md#机器人混合富文本)。

`channel plan`、`channel doctor`、`channel status` 全部离线，返回中明确 `creates_subscription:false`、`sends_message:false`、`online_checked:false`。`channel probe` 是唯一记录“已验证能力”的命令，会以该通道自身身份连接平台；登录身份与配置不一致时返回 denied。未验证的能力按不存在处理，`channel pull`/`run` 因此返回 unavailable。

`channel pull CHANNEL --conversation ID` 按半开区间 `[start,end)` 回填。省略 `--start/--end` 时从水位续读并带 `--overlap` 重叠，因为平台投递与本机提交不是同一个事务。窗口未读到尽头即记为缺口，`covered_until` 不推进；complete 但起点晚于已覆盖范围时，中间未读区间单独记为未闭合缺口。`channel status` 列出水位、覆盖窗口与仍未闭合的缺口。

`channel run CHANNEL` 前台运行接收会话并持有通道租约，每次获取都推进 fence；写入前重新校验租约，失去通道的接收者无法继续写入。事件先提交再计数，写入失败即停止接收而不确认收到。无法解析或不支持的事件写入 inbox 并标记 rejected，不会丢弃。`channel ingest CHANNEL --file -` 离线导入规范化 NDJSON 事件，强制 `origin=import`，不推进覆盖范围，且不能声称未经显式映射的在线身份。

`channel route update ROUTE --expected-version N --reason TEXT` 修改绑定策略，重算 audience_key 并把旧策略下的草稿置为 stale；不允许改指到另一个会话。`message identity link CHANNEL --basis` 只接受 platform_directory、operator_confirmed、same_open_id，显示名不构成依据。被撤回消息在 `message list`/`show` 中不展示正文，也不会被 `message query` 命中。`message query CHANNEL` 可按会话、正文或发送者显示名、RFC3339 半开时间窗口查询已提交观测，并返回命中或指定会话的详细水位及通道覆盖摘要；显示名只能用于检索，即使零命中也保留覆盖摘要，空结果不能越过水位缺口证明平台记录不存在。

## 回复草稿与投递

`reply open CHANNEL --input -` establishes a request context through a trusted connection, binding the actual principal, destination, audience, route version and expiry. `reply show CONTEXT` reads it. `reply draft --input -` accepts the context, body and format; legacy Memory citations are no longer accepted. The target and sender come from the context and channel. An expired context or changed route rejects the draft. The same context and input produce one draft; repeats return the existing draft with `duplicate`.

`outbox preview DRAFT` reads the proposed content, destination, actual sender, current checks, `display_digest`, and `approval: {mode: display_only, status: not_evaluated}`. It does not send, advance state, record an attempt or grant approval. `outbox list/show` are also read-only. Old drafts referencing Memory become stale during upgrade unless delivery has already started; ambiguous results remain unknown and cannot be made retryable by clearing citations.

发送是独立开关：路由默认 `draft_only`，`send` 能力未验证时也不可发送，二者缺一 `outbox dispatch` 返回 denied。`outbox dispatch DRAFT --expected-digest ...` 必须绑定展示时读到的摘要，事务内重新执行全部检查、置为 sending 并先记录发送尝试，返回 `sent:false`；平台调用是独立步骤，再由 `outbox result DRAFT STATE` 记录真实回执。`accepted` 必须带回执；缺回执的结果是 `delivery_unknown`，`retry_safe:false`，不自动重发。已交给平台的草稿不能本地取消；`outbox reconcile DRAFT --outcome ... --evidence ...` 只接受带实际核对依据的结论，不伪造平台送达证明。

## 幂等与并发

`--idempotency-key` 按命令和工作区隔离，绑定参数、文件内容和业务输入；同键不同输入返回 conflict。配置等版本化更新使用 expected-version；投递绑定 display_digest；Workspace 写入使用 expected_digest 绑定旧文件内容。

采集命令是例外：`channel pull`/`run`/`ingest` 一次调用会提交多个独立事务（每条事件、覆盖记录、租约各自提交），幂等键作用在单条事件而不是整条命令上；重跑同一窗口按事件去重，计入 duplicates。租约的获取与释放不使用幂等键，否则新接收者会拿到早已失效的令牌。

`backup create/list/verify/restore` manages consistent **operational SQLite backups only**. `backup create --output FILE` selects a new file; with `--idempotency-key`, the name is derived from the key in the default backups directory and `--output` is rejected. Restore requires the SHA-256 from `backup verify` through `--expected-digest`; a replay cannot overwrite later work. `export` likewise exports operational data, not Workspace knowledge.

A complete recovery set must separately preserve `<home>/agent-workspaces/`, `<home>/agent-workspace-history/`, effective configuration and any independently managed secrets/presets. Stop writers before capturing the filesystem set; copying a live `state.db` is not a consistent backup. To undo Schema 27, restore the verified pre-upgrade archive with its corresponding old binary. Do not run an old binary against the upgraded database or mount legacy archives for Agents.

## AI 值守运行时

`agent preset enable <harness> [--name NAME]` 显式创建受控 Git 规则目录；省略名称时使用 `<harness>-default`，同名目录不能跨 harness 复用。`status/sync/disable` 分别检查、提交规则副本和停用；`sync --from-policy FILE` 适用于任意 harness，`--from-claude-md` 是 Claude 兼容入口。普通 `init` 不创建 preset。`runtime harness [name]` 只读显示已注册 harness 的契约状态，不初始化数据库、不调用模型。

Enabling an existing preset preserves its committed policy; it does not refresh it from a newer template. A newly named preset uses the installed binary's current template. Task preparation separately refreshes the managed Workspace skill and tools without resetting knowledge files. Explicit skill configuration rejects retired memory skills and native/global knowledge writers (`memgov-memory`, `touch-memory`, `error-reflection`), including aliases; inherited executor discovery skips them. `memgov-workspace` is supplied by the runtime rather than configured as an external skill.

`runtime configure --input -` 输入 name、channel、route_ids 和 owner，可选 Agent 策略、claude_profile、模型、preset 与调度参数。当前 proactive 任务使用 `record_only`，完成结果不自动推送 Owner。direct/group_mention 派生为 `reply_to_trigger`，并核验对应私聊/原群出站路由。group_mention 的 context_channel 可省略；提供时仍要求同企业同群。owner 必须是 DWS 已验证稳定身份。修改已有配置需要 expected version，running 状态不能修改。harness 和平台适配器由运行时注册表选择，群 Jarvis 仍保留其独立 Agent、技能、工具、独立 Workspace 和原群回复路径。

`runtime start ID` 是持续前台命令，输出 `schema_version=1` 的 JSONL 日志；它不使用普通 JSON envelope。统一服务提供提交后即时唤醒，独立前台入口仍以默认 1 秒扫描消费。`runtime pause/resume/stop` 修改持久状态。`runtime restart ID` 先写入 stopped 状态，等待当前机器上同名的旧 `memgov runtime start` 或 `memgov runtime restart` 进程退出，再由当前命令以前台流模式启动；等待超过全局 `--timeout` 时失败且不启动并行进程。pause 和日志 degraded 状态继续采集消息，但不创建新 AI 任务。

DWS sources collect messages independently; proactive evaluation can create bounded Owner work tasks. Current proactive completion remains `record_only`. Root/child orchestration and automatic proactive result notifications are future product work, not capabilities of this refactor. Verified Owner bot private messages and valid group mentions enter their respective Agents directly. Interactive bot tasks use their existing receipt/reaction and reply paths; group output stays within the original group route. An Owner speaking in a group does not inherit private-chat permissions, and other users cannot enter the Owner private Agent. See the [runtime guide](../guides/runtime-user-guide.md) for the current execution and notification boundaries.

`runtime task list/show/cancel/retry/resume` 使用普通 envelope。`runtime task confirm ACTION --input -` 接受 `{"message_id":"本地消息 UUID"}`，卡片已展示的动作拒绝此命令绕过按钮；旧口令流程从 SQLite 验证该消息确实来自任务原群的所有者（群助手）或绑定 owner direct route（其他模式）、晚于动作且正文完整匹配确认口令；平台已核验的群 @ 可带一个开头 `@名称 `；不接受自报 sender 或 origin。

Schema 27 removes `memory_status`, `memory_error_code`, `work.queued_reviews` and the dedicated extraction/review queue. Proactive confirmation remains limited to a concrete `destructive_operation` proposal with a nonempty target and payload `operation`/`impact`/`recovery`. Confirmation must be a new original message from the verified Owner in the explicitly bound bot private route; historical, quoted or stale-version input cannot approve it. Other blocked background actions do not acquire permission through Workspace notes.

`runtime task resume <task-id>` 继续失败任务：复用有效原生会话或按原请求与已有成果恢复，增加任务版本并新增尝试；`--expected-version` 可绑定调用方看到的版本。`retry` 则从头准备。原文失效、私聊 clear、权限变化或未知外部结果会阻止不安全的继续；Schema 21 与具体条件见[继续协议](../design/task-continuation-detail.md)。

`runtime task propose-action <task-id> --input <file|->` proposes an action bound to the current attempt and current permissions; it is not a general external-action grant. `runtime task capture-hotword` has been removed. Durable preferences and lessons use the scoped Workspace contract, with observation dates and source references. See [runtime boundaries](../design/agent-runtime-design-detail.md).

`runtime logs list/show` 使用普通 envelope；`show --task ID` 同时返回匹配事件与 SQLite 任务详情。`runtime logs follow` 只输出新增和已有匹配事件的 NDJSON。日志过滤支持 since、until、level、component、batch 和 task；时间使用 RFC3339。

### 后台 Agent 沟通工具

`runtime message send <task-id> --input <JSON-file>` 在当前 running proactive task/attempt、owner_delegated 策略与 DWS Owner 身份下执行独立沟通；`runtime message list <task-id>` 查询审计。输入包含 attempt_id、idempotency_key、target_type（group/user）、稳定原生 target_id、content、reason、evidence_message_ids。目标、实际披露证据及当前配置均复核；固定绑定 profile、本人与 AI 标识。同键同正文返回已有记录，改正文重用键冲突；失败或未知也不盲目重发。结果记录于 runtime_message_actions；这项显式授权沟通能力不表示 proactive 完成会自动通知。群 Jarvis 的交互结果仍走原群路由。完整协议见[独立沟通工具](../design/dingtalk-integration-design-detail.md#独立沟通工具)。源码、安装和真实平台状态以[交付状态](../implementation-status.md)为准。

## Confirmation-card compatibility

应用通道 `identity.confirmation_card_template` 接受已关联应用的 `.schema` 模板 ID。原生卡片及 Stream 回调协议已实现，但当前运行服务暂时停用卡片入口，群 pending 结果使用原群完整确认口令；恢复卡片后只有同企业认证 DWS 所有者可“同意”或“拒绝”，同意后执行，拒绝后取消且不能切换决定。正文、@ 对象和展示动作冻结在 Outbox；模板、来源、动作或路由变化拒绝旧卡片。详见[确认卡片](../design/dingtalk-integration-design-detail.md#群回复与确认卡片)。
