# memgov Personal Jarvis 运行手册

Status: current operational guidance with the Agent Workspace replacement. Source, installed and live-platform evidence are separated in [implementation status](../implementation-status.md).

Owner private chat runs a continuous Agent conversation. DWS proactive analysis creates independently tracked work and currently finishes with `record_only`; authorized communication is a separate audited tool action. Group mentions use the original group route and its configured Agent. Root/child task orchestration and automatic proactive result notifications remain product targets in the [Owner design](../design/owner-assistant-design.md).

Owner chat and proactive work share a workspace derived from verified `OwnerPrincipalID`. Each group uses its channel/conversation workspace, including when two groups use the same preset. Every turn gets the latest bounded `MEMORY.md`; detailed notes are searched and read on demand through `memgov-workspace`. Files retain dates, source references and revision history. Source text and knowledge cannot expand permissions. Project worktrees, session scratch, task outputs and durable knowledge remain distinct. Allowed inherited skills remain available through an isolated, filtered skill list. The runtime ignores native automatic memory and ambient `CLAUDE.md` files; configure operating rules through the committed Agent preset and knowledge through `memgov-workspace`. Renaming the retired `memgov-memory` skill does not enable it.

macOS managed service uses the active `~/.memgov/config.yaml`; `config.dual.yaml` is only a migration backup. Stop the old service, run `config migrate-workspaces`, then upgrade with `init` before using the new executable. New workspaces start empty. See [initialization](initialization.md), [Workspace design](../design/agent-workspace-design.md) and [unified service](../design/unified-service-design.md).

直接 Agent 单轮最长运行 30 分钟。到达上限会明确失败；服务恢复时，遗留运行中任务和尝试收口为 `failed/runtime_restarted`，未知外部操作与投递收口为 `unknown` 且不自动重放。执行中的 Owner 私聊任务被 `runtime task cancel` 取消后，运行时会同步停止当前 Agent 及其子进程（process tree，进程树），并拒绝迟到结果；已经完成或等待确认的结果仍会正常交付。若取消前已经收到“处理中”回执，机器人会把原消息更新为失败标记并发送一条安全错误说明，不会只停留在“处理中”。已确认接收的 Owner 私聊和群 @ 都会收到一次机器人身份的幂等失败通知；即使中断前没有产生答案，群里也会显示安全错误代码、原因和下一步，而不是只留下“打叉”。失败通知不回传 Provider stderr、凭据、URL 或原始异常文本。

私聊与群 @ 由机器人 Stream 实时接收。消息先用一个短事务保存去重、任务恢复和投递所需的 SQLite 状态，再立即唤醒对应 Agent；默认 1 秒扫描只在唤醒合并或丢失时兜底，不会等待 DWS 群历史查询。原消息的“已收到”“处理中”等表情按顺序旁路发送，平台接口变慢不会阻止 Agent 开始；群目录刷新和补漏也独立运行。回答与工具验证仍由 Claude 执行，正文当前在完整结果生成后一次性交付，回复会显示接入耗时、执行耗时和实际模型。

After a managed restart, a running supervisor does not by itself prove that the bot Stream is connected. Verify application intake and the resulting bot reply before treating a private-chat probe as successful; DWS send success only confirms platform acceptance. The application receiver has no history backfill, so do not assume a request sent during reconnection will be recovered. The observed limitation is tracked in [implementation status](../implementation-status.md#authorized-local-acceptance).

运行时会按实际通道加入通道专属系统提示。本人在钉钉私聊中提到某位同事、且问题可能依赖双方沟通时，Agent 默认通过 `dws` 先在当前企业中确认联系人，再读取与该人的一对一聊天；不会先遍历长期记忆，也不能因为运行时会话目录为空就声称没有聊天或正式记忆。重名时先请本人消歧，不猜测身份。只有本人明确询问长期知识，或私聊记录不足时，才继续使用 `memgov-workspace`。群内 @ 使用另一套边界：只使用当前群及向该群开放的上下文，不会因为提到某人而读取其私聊。

发送也按通道区分身份。私聊机器人的普通回答不用调用 `dws`，由运行时以应用机器人身份回复当前私聊；本人明确要求“另发给某人或某群”时，绑定了 DWS profile 的完整 Owner Agent 才可用 `dws chat +messages-send --as user` 以本人身份发送，并保留 AI 标识。群 Agent 的回答始终以应用机器人身份回到原群，即使 Owner 在群里要求也不能改用 DWS 本人身份；跨群或私聊没有精确的机器人发送授权时只生成待处理操作。看到 Agent 为普通群回答探测 `dws chat --help`，或用 `--as user` 发送，均属于错误选路。

系统提示的共用自我定位在 [identity.md](../../internal/sysprompt/identity.md)、安全规则在 [security.md](../../internal/sysprompt/security.md) 统一维护，各入口基础提示放在同目录。Agent 被问及身份或能力时，会把自己简要说明为能结合对话、获准工具和受治理长期记忆推进工作的 AI 伙伴，只列当前实际能力，不自称底层模型或 CLI 产品。修改后需要构建、安装新二进制并重启服务，所有入口一起生效；当前不支持热加载，也无需逐个同步 preset。升级后若继续旧任务提示策略已变化，应重新发起请求。规则、验证与完整 Bash 的安全限制见[维护说明](../design/agent-runtime-design-detail.md#统一系统提示与安全验证)。

已采集的增量会话可以直接查询，但它仍是原始观测，不会自动写成长期记忆。`memgov message query dingtalk-watch-dingtalk --query "项目名" --since 2026-09-15T00:00:00Z` 返回匹配消息及会话类型、水位和缺口；加 `--conversation` 可限定一个会话，`--until` 使用不包含结束点的 RFC3339 时间。空结果只有在覆盖完整且目标会话确实属于采集范围时才有意义；独立数据源默认只采集已授权群聊；明确启用 `direct.enabled` 后还可采集授权同事私聊并保留七天原文，见[私聊采集与保留](../design/direct-message-retention-design.md)。未启用、窗口外或有缺口的私聊仍需按授权通过 `dws` 回查。

`/clear` starts a new private conversation without deleting workspace knowledge or audit records. `/status` reports current Agent, preset, model, skills, Bash and action policy. Workspace tools and the short index are intrinsic runtime context, not a `memory_read` capability or `memory_scope` setting. An active conversation receives a changed index on its next turn. Permission, model or preset changes invalidate old execution context as required.

Verified Owner private chat and default proactive Agents retain their configured Bash/file/test/skill access. Explicitly limited Agents keep their limits. No-Bash groups can read and maintain their own workspace through bound tools; they do not gain arbitrary host file access. `local_read`, `local_write` and `local_test` keep their existing project/file meanings. Full Bash uses the service account and is not an OS sandbox. Shared reference material requires explicit read-only `directories` configuration.

需要覆盖本人私聊默认值时，在现有 YAML 中增加：

```yaml
agents:
  owner-chat:
    preset: claude-default
    claude_profile: cc
    execution_model: profile
    capabilities: [local_read, local_write, local_test]
    bash: true
    external_actions: owner_request
applications:
  owner_private:
    enabled: true
    runtime: owner-private # 可省略；内部兼容绑定
    agent: owner-chat
```

保留原有其他 Agent、proactive 和 group_mention 配置。未声明 `owner_private` 的现有本人直聊使用内置默认值；声明它但不写 `agent` 时接管现有本人实例并保留模型配置。指定了 Agent 时严格使用声明：`bash` 只能为 `true`/`false`，省略为 `false`。要关闭本人 Bash，可设 `bash: false` 和 `external_actions: owner_confirmation`。预览会拒绝缺失或身份不匹配的私聊实例，无需重新填写平台 ID。

新配置推荐按机器人声明，下面引用现有群 Agent；`owner_private.agent` 省略时继承 bot 默认人设/模型并使用完整 Owner 权限，显式设置才能收紧：

```yaml
applications:
  bots:
    app-main:
      default_agent: group-helper
      owner: {id_type: user_id, id_value: OWNER_ID}
      owner_private: {enabled: true, runtime: owner-private}
      group_mention:
        enabled: true
        execution_concurrency: 4
        bindings: []
        # source: work_chat  # 可选同群历史
```

不要同时为同一机器人保留冲突的旧 group_mention，或让两个 Owner 声明绑定同一 runtime。其他机器人可用各自 channel 名重复声明。仅使用机器人交互时可以不配置 data_sources；Owner 身份核验仍不可省略。

Group history labels each speaker and identifies the current requester separately. Different members' questions can execute concurrently, while the same member's follow-ups in one group stay ordered. `execution_concurrency` defaults to 4 (range 1–32) across that runtime's groups; 1 requests serial execution. The legacy `applications.group_mention` form accepts the same setting. Apply the configuration to update an existing runtime. Each answer remains bound to its original message. Recent history is limited to 30 messages: quote the earlier question when asking to continue someone else's issue; this does not automatically reopen that task's process or artifacts.

仅为一个群开放 Bash 时，复制出独立 Agent，设置 `bash: true`，再通过 `applications.group_mention.bindings` 绑定目标群；不要修改共享默认 Agent。Full Bash enables normal capability-based file tools, Bash, web search/fetch and selected skills. It cannot be combined with bounded `directories` snapshots; use `directories: []` for an executable Agent. Configure `skills: {inherit: executor, paths: []}` to refresh compatible installed executor skills on each task. This uses the service account, so scoped knowledge APIs do not imply host filesystem isolation. 配置应用及重启方法见下文；`runtime status` 和私聊 `/status` 可查看 Bash 与外部操作策略。权限变化后旧执行与会话失效。

A durable Workspace is a knowledge directory, not a Claude installation. New attempts regenerate managed tools from the running memgov binary and retain existing notes. Upgrading the executable does not rewrite an existing preset: create a current preset with `agent preset enable claude --name <new-name>`, or deliberately sync its policy. Full execution capability does not automatically migrate old reference documents into knowledge.

首次接入默认关注当前账号中已确认最近 30 天有消息的可访问群，并按 `--ignore` 排除不参与值守的群。仅在显式配置机器人筛选时，才要求机器人属于目标群。`runtime setup` 自动完成数据库初始化、Agent preset、项目工作区、当前钉钉身份、本人 `userId`、范围发现、通道、路由、能力探测和运行时配置。

## 章节导航

1. [准备](#1-准备)
2. [一键接入](#2-一键接入)
3. [用统一 YAML 启动 Personal Jarvis 与群 Jarvis](#3-用统一-yaml-启动-personal-jarvis-与群-jarvis)
4. [启动](#4-启动)
5. [切换到正式参数](#5-切换到正式参数)
6. [查看和管理任务](#6-查看和管理任务)
7. [确认外部操作](#7-确认外部操作)
8. [运行日志](#8-运行日志)
9. [暂停、恢复和停止](#9-暂停恢复和停止)
10. [常见问题](#10-常见问题)
11. [不同群使用不同 Agent](#11-不同群使用不同-agent)
12. [限定所有者 Agent 的目录](#12-限定所有者-agent-的目录)
13. [离线验证](#13-离线验证)

## 1. 准备

本机需要安装并登录：

- `dws`：已登录目标钉钉企业；
- `claude`：已完成 Claude CLI 登录，或已在 zsh alias 中配置可用的 Anthropic 接入；
- `git`：用于 Agent preset 和代码任务的本地提交。

在项目目录安装 memgov：

```bash
cd "$HOME/src/memgov"
make install

export PATH="$PWD/.memgov/bin:$PATH"
export MEMGOV_HOME="$HOME/.memgov"
```

For a macOS service that needs a protected folder, configure a stable signing identity before building and installing upgrades; otherwise a rebuilt local binary may trigger the folder permission dialog again. See [build from source](../../INSTALL.md#build-from-source). The proactive Agent receives its configured project path and should use it directly instead of scanning the user home to find a project. Full Bash remains an unrestricted host tool, so the prompt is guidance rather than an OS access boundary.

检查依赖：

```bash
memgov version
dws profile list --format json
claude auth status
```

## 2. 一键接入

重复使用的 profile、机器人 Code、忽略群、Claude 模型及触发参数可放到 YAML 的 `runtime_setup` 中，日志保留参数放到 `logging`。参见[统一配置说明](initialization.md)和[完整配置示例](../../config.local.yaml.example)。使用 `--config` 指定文件，命令行同名参数优先；这些初始化默认值不会覆盖已存在实例。

进入希望 AI 处理代码任务的项目目录，然后运行：

```bash
memgov runtime setup my-watcher \
  --ignore "告警通知群" \
  --ignore "闲聊群" \
  --pilot
```

程序使用当前 dws profile 与 contact +me 核验本人稳定 userId，发现最近 30 天活跃群，并将当前目录注册为任务工作区。机器人筛选可选，不再自动选择唯一机器人。搜索被分页上限截断时只纳入本轮正向核验群；discovered.group_discovery_complete=false 表示未覆盖全部群，watched_group_count 表示实际范围。没有可核验群时配置失败。

`--ignore` 可以重复传入群名或稳定会话 ID；名称必须完整匹配，避免误忽略名称相近的群。完整群目录中匹配的忽略规则会保留，即使该群未出现在本轮截断的活跃结果中，也只会创建 `ignore` 路由。`--pilot` 使用“新增 1 条或等待 30 秒”触发，适合首次验证。

如需把观察范围限定为某个机器人所在群，显式提供 Code 和名称；不提供时按 DWS 可访问的活跃群及 ignore 规则筛选：

```bash
memgov runtime setup my-watcher \
  --ignore "告警通知群" \
  --robot-code "机器人 Code" \
  --robot-name "机器人名称" \
  --pilot
```

后台完成使用 `record_only`，不自动发送完成通知；协作消息通过独立授权的沟通工具处理。机器人参数只用于可选范围筛选；群 Jarvis 私聊/群 @ 继续使用原应用入口和原有回复路由。memgov 不替用户创建企业应用或申请管理员权限。

常用可选项：

| 参数 | 默认值 | 用途 |
| --- | --- | --- |
| `--ignore` | 无 | 标记不采集、不分析的群；可重复传入 |
| `--profile` | 当前 dws profile | 指定另一个已登录企业身份 |
| `--delivery-conversation` | 兼容参数 | Current proactive work keeps `record_only` 任务仍不投递 |
| `--robot-name` | 不筛选机器人 | 与显式 robot-code 配合，限定机器人所在群 |
| `--workspace-path` | 当前目录 | 指定代码任务目录 |
| `--workspace-name` | runtime 名称 | 指定 memgov 工作区名称 |
| `--channel-name` | `<runtime>-dingtalk` | 指定本地通道名称 |
| `--agent-harness` | `claude` | Registered Agent harness; inspect available adapters with `runtime harness` |
| `--agent-preset` | `<agent-harness>-default` | Controlled Agent preset |
| `--claude-profile` | 无 | 读取指定 zsh alias 中的 Claude 环境和默认模型 |
| `--analysis-model` | `haiku` | 指定分析模型；`profile` 表示沿用 alias 默认模型 |
| `--execution-model` | Claude 默认值 | 指定执行模型；`profile` 表示沿用 alias 默认模型 |
| `--pilot` | 关闭 | 使用 1 条/30 秒/10 秒对账的验证参数 |

被忽略的群会以 `mode: ignore` 保存在通道配置中，便于通过 `memgov channel show my-watcher-dingtalk` 查阅。系统不会订阅或补漏这些群，即使收到意外事件也不会保存消息正文。

运行期间每个对账周期重新读取群列表，只有正向活跃证据且符合可选机器人过滤的新群才成为 collect；已有 ignore 路由保持排除。

`setup` 只读核验钉钉身份和范围，不发送测试消息，也不启用自动结果投递。

`runtime setup --agent-harness NAME` selects a registered harness independently of the DWS platform. It rejects an unavailable or incomplete adapter before platform discovery or local state changes, and creates `<NAME>-default` when `--agent-preset` is omitted. A non-Claude harness requires an explicit `--analysis-model` and rejects `--claude-profile`; each adapter owns its credentials and execution behavior. The same selection is available as `runtime_setup.agent_harness` in YAML. A preset alone does not install a harness adapter.

### 2.1 使用本机 Claude alias

如果平时通过 `cc` 之类的 zsh alias 启动 Claude，可直接把它作为运行时配置档：

```bash
memgov runtime setup my-watcher --claude-profile cc --pilot
```

指定 profile 后，任务执行默认沿用 alias 中的 `ANTHROPIC_MODEL`，增量识别仍默认使用 Haiku。也可以显式覆盖某个阶段：

```bash
memgov runtime setup my-watcher \
  --claude-profile cc \
  --analysis-model haiku \
  --execution-model profile
```

memgov 只解析 alias 中简单的 `ANTHROPIC_*` 和 `CLAUDE_CODE_*` 环境变量，不执行 alias，也不继承其中的命令行参数。运行时优先静态读取 `ZDOTDIR/.zshrc`，找不到时才用带超时的 zsh 读取，避免 ccswitch 或 shell 启动插件卡住 Agent。赋值之间使用 `&&` 或 `;` 均可，因此 ccswitch 切换后生成的常见 alias 仍可使用。SQLite 只保存 alias 名和模型选择；认证信息仅传给 Claude 子进程。

## 3. 用统一 YAML 启动 Personal Jarvis 与群 Jarvis

[配置示例](../../config.local.yaml.example)支持 channels、data_sources、agents、applications。复制或合并到 `~/.memgov/config.yaml` 后，Owner 私聊、DWS proactive 和群 Jarvis 共用这一份声明；仓库内 `config.local.yaml` 只作为开发入口链接。后台来源不要求绑定机器人；机器人按 applications.bots.<channel> 配置默认人设及 Owner 私聊/群覆盖，无需启用 DWS 采集或历史导入。Owner 身份仍须通过 DWS 核验；群若显式绑定 source 才读对应同群历史。新建通道仍需核验其实际使用的收发能力。

```bash
memgov --config ~/.memgov/config.yaml config validate
memgov --config ~/.memgov/config.yaml config plan
```

`plan` 返回 `plan_digest` 和 `applied_version`。确认展示的变更后，填入这两个值应用：

```bash
memgov --config ~/.memgov/config.yaml config apply-runtime \
  --plan-digest PLAN_DIGEST \
  --expected-version APPLIED_VERSION
```

扩大可用群或 Agent 权限时，命令会要求明确理由；变更身份或记忆披露边界也需明示。例如：

```bash
memgov --config ~/.memgov/config.yaml config apply-runtime \
  --plan-digest PLAN_DIGEST --expected-version APPLIED_VERSION \
  --authorize-expansion --reason "所有者批准扩大接管范围"
```

预览会核对逐群 Agent 绑定、目录能力和通道权限。群 Agent 支持只读目录快照及隔离产物；所有者 Agent 的声明目录采用独立副本；声明目录中的 Shell 测试与未核验通道能力仍会阻止相关应用。修改 YAML 后重新预览，旧摘要不能继续使用。应用只写入本地 SQLite，不会自动发送钉钉消息。

机器人已具备明确 assistant 群路由和 Owner 身份核验时，可以直接启用 bots 内的群助手，无需先运行来源。希望自动挂载 DWS 发现的群时，先停用群应用、建立并启动来源，再按下述方式应用。新来源从空范围开始；已启用后台观察也会先显示 proactive_deferred，正向发现后才创建运行实例，不继承通道旧群。

数据源启动后核验 DWS Owner 并发现活跃会话；仅设置 member_robot 时才筛选机器人所在群。正向收据写入后，同一 YAML 再 plan/apply 即可创建后台运行实例。旧来源可用 data-source attest-owner 重新核验身份。自动群挂载仍需要同企业、机器人在群等证明，详见[首次自动挂载](#群助手首次自动挂载)；显式群路由不依赖该自动过程。以下为兼容的分进程调试示例，日常优先使用统一服务：

```bash
memgov --config ~/.memgov/config.yaml data-source start work_chat
memgov --config ~/.memgov/config.yaml config plan
memgov --config ~/.memgov/config.yaml config apply-runtime \
  --plan-digest NEW_PLAN_DIGEST --expected-version CURRENT_VERSION
memgov --config ~/.memgov/config.yaml runtime start proactive
```

来源或群证明变化时旧摘要会失效；已运行的主动值守需先 `runtime stop proactive`，再预览并应用新范围。

开启群 Jarvis 并完成它的 `config apply-runtime` 后，再在另一个终端运行 `memgov --config ~/.memgov/config.yaml runtime start group-mention`。

按实际启用模式保留相应进程。数据源独立负责实时接收与周期补漏；在 YAML 中启用 `history_import.enabled` 后，会为新接管群建立固定范围历史导入（默认 30 天）。停止主动值守或群 Agent，采集仍可继续。

数据源的独立运行日志可用 `memgov data-source logs list <source>`、`memgov data-source logs show <source>` 和 `memgov data-source logs follow <source>` 查阅；`memgov data-source status <source>` 会显示日志健康状态。跨通道消息是否已由平台核验，可用 `memgov message association <message-id>` 查看；`unresolved` 表示尚无可用证据，不会按文字相似自动合并。

```bash
memgov data-source status work_chat
memgov data-source history list work_chat
memgov data-source history show IMPORT_ID
memgov data-source history cancel IMPORT_ID
memgov data-source history retry IMPORT_ID
memgov data-source pause work_chat
memgov data-source resume work_chat
memgov data-source stop work_chat
```

导入的旧消息只供上下文查询；`history list/show` 的覆盖与去重计数可以区分导入完成、缺口和重复读取。关闭来源时请先停止依赖它的 AI 消费者，再改配置。

独立采集默认每 5 分钟复核一次最近 30 天活跃群，并补齐遗漏消息；仅在显式设置机器人范围筛选时复核机器人成员关系。YAML 的 `data_sources.<名称>.reconcile_seconds` 可调整间隔，最短 10 秒。连接正常时也会执行检查；群范围没有变化时不会重连。新发现的授权群可以纳入；群不再活跃或被标记 `ignore`，以及启用机器人筛选后明确发现机器人退出时，会按发现完整性规则收缩采集范围。

消息搜索被截断时，只把已出现消息、且通过所有显式范围筛选的群作为本轮已核验范围；这些群可以开始采集和挂载群 Agent。未出现在前 500 条里的旧群会保留，不会被误判退出。显式机器人筛选每轮最多核验 64 个活动候选群，整轮最多 2 分钟；大型账号可能只完成部分范围，状态中的 `discovery.complete=false` 会明确显示这一点。

`discovery.valid=true` 表示收据列出的群已得到本轮正向证明，不表示所有群都检查完成。发现超时时，收据仍标记失效；如果最近一次正向证明在 24 小时内，且来源、通道、机器人和路由授权都未变化，启动或重启仍可采集其中已授权的群并有界补漏。预先配置到其他工作区的群继续存入原工作区，不会移入采集来源的默认工作区。被忽略、修改过或缺少证据的群不会恢复；没有可用证据时等待发现成功。降级不会新增群、更新证据时间或启动历史导入任务，状态显示 `discovery_unavailable`，日志显示 `discovery_degraded`。历史读取失败也不会中断实时接收。暂停会释放接收租约，恢复重新检查范围并补漏；进程重启保留已提交的覆盖和轮询游标。通过 `data-source status <名称>` 查看断点和发现收据，通过 `data-source logs follow <名称>` 查看周期检查及耗时。

## 4. 启动

以前台方式启动值守：

```bash
memgov runtime start my-watcher
```

保持该终端运行。它会把结构化日志输出到 stdout，并同时写入独立日志目录。

另开一个终端查看状态：

```bash
memgov runtime status my-watcher
memgov runtime logs follow my-watcher
```

在监听群发送一条启动后的新消息，例如：

```text
请你检查这个项目当前测试是否通过，并把结果告诉我。
```

后台验证应核对本地任务、Agent 产物和操作审计，而不是等待机器人通知。新矩阵见[验收要求](../architecture/best-practice-scenarios-detail.md#personal-jarvis-固定验收案例)：

1. 新消息进入增量批次；
2. Haiku 评估处理价值与可行调查方向；
3. 独立 Agent 按预设权限查证、处理并验证；
4. 实际代码修改按工作目录策略留下产物和本地 commit，普通调查无需制造提交；
5. 本地保存结果，不自动发送完成通知；主动工具沟通另查 runtime message list <任务ID>。

机器人交互另外使用 Markdown 私聊或群消息并在末尾 @ 发起人，附问题引用和执行模型/耗时；需要所有者同意或拒绝时显示文字确认口令。这类展示不用于后台自动通知，例如：

```text
> 你问：请检查这个项目当前测试是否通过

测试已经通过。

⏱ 执行 46.0s · 🤖 claude-sonnet-4
```

源码已提供 `memgov runtime task resume <任务ID>` 和 Web 任务详情的「继续任务」入口：中断后恢复原会话，或结合旧任务的原请求与已有文件继续。该入口需使用当前程序并显式升级数据库；使用与边界见[中断后继续任务](../design/task-continuation.md)。

首次启动前的历史只作上下文，不执行陈年任务。没有新消息时不会调用模型。

## 5. 切换到正式参数

`--pilot` 适合短期验证。正式实例默认按新增 20 条或等待 5 分钟触发，并每 5 分钟补漏。建议验证通过后创建不带 `--pilot` 的正式实例：

```bash
memgov runtime setup work-watcher \
  --ignore "不参与值守的群" \
  --robot-code "机器人 Code"

memgov runtime start work-watcher
```

需要精细修改已有实例时，使用 `runtime status` 查看当前配置，再通过 `runtime configure --input runtime.json --expected-version VERSION` 更新。

## 6. 查看和管理任务

```bash
memgov runtime task list my-watcher
memgov runtime task list my-watcher --status failed
memgov runtime task show TASK_ID
memgov runtime task cancel TASK_ID
memgov runtime task retry TASK_ID
```

同一事项的后续消息会更新原任务。执行中发生编辑、取消或撤回时，旧版本结果不会交付。

## 7. 确认外部操作

群助手关联 `identity.confirmation_card_template` 后，原生审批卡片能力仍保留在代码中；当前运行服务暂时停用卡片入口，群内待确认操作统一展示完整确认口令。这样可以先完成操作闭环，恢复卡片时无需改动任务或动作数据。卡片标题、详情、按钮与执行链路的实现和验收要求见[模板要求](../design/dingtalk-integration-design-detail.md#群回复与确认卡片)。

以下口令兼容协议适用于未配置模板的群助手和显式收紧权限的 Owner 私聊（owner_confirmation）。后台默认 owner_delegated 自主处理；受限后台的待确认只留本地记录，不发送确认通知。Owner 私聊默认 owner_request，按本人的明确要求处理。

使用 owner_confirmation 的交互 Agent 先准备具体操作，再展示一次性口令：群助手在任务原群，受限 Owner 私聊在该私聊。后台 Agent 的已授权操作不走这个交互确认流程；需要独立沟通时调用 runtime message send，并保存目标、理由、证据和回执。

```text
确认操作 ACTION_ID DIGEST_PREFIX
```

确认无误后，群任务由所有者在任务原群通过钉钉 @ 功能选中机器人并发送整行口令；其他模式在绑定的机器人单聊回复。只有已核验所有者、动作生成后的新消息和完整匹配的口令有效。群任务不接受其他群或私聊确认。运行结果未知的外部动作不会被盲目重试。

旧主动值守通知配置归一为 `record_only`；当前 proactive 完成、阻塞和待确认状态都只记录，不会因声明 Owner 私聊路由而启用自动通知。具体破坏性操作仍可通过明确绑定且已核验的 Owner 私聊确认，详见 [CLI 契约](../reference/cli-contract.md#ai-值守运行时)。机器人 `identity.history_channel` 可用于 DWS Owner 身份核验，不要求启动采集进程。

值守 Owner 应使用经过 DWS 认证核验的 `user_id`。旧配置若写 `staff_id`，不能仅因值相同就视为已核验：先通过认证 DWS profile 核验本人，再将值守 Owner 改为该 `user_id` 并重新 plan/apply。机器人消息若使用另一种 ID 类型，还需已核验的 identity link。配置缺失或存在多个可用机器人/私聊入口时，系统不自动接受跨通道确认。错误群聊、其他人的回复、过期或撤回消息均无效。确认口令不会作为新的私聊任务调用模型。

## 8. 运行日志

```bash
memgov runtime logs list my-watcher
memgov runtime logs show my-watcher --level error
memgov runtime logs show my-watcher --component execution
memgov runtime logs show my-watcher --task TASK_ID
memgov runtime logs follow my-watcher --task TASK_ID
```

日志文件位于：

```text
<MEMGOV_HOME>/runtime/logs/<runtime-id>/
```

结构化诊断日志不保存聊天原文、完整提示词、模型完整输出、凭据或原始子进程 stderr。任务正文、来源和执行结果仍通过 `runtime task show` 从 SQLite 查阅。

## 9. 暂停、恢复和停止

```bash
memgov runtime status my-watcher --human
memgov runtime pause my-watcher
memgov runtime resume my-watcher
memgov runtime stop my-watcher
memgov runtime restart my-watcher
```

- `pause` 继续采集消息，暂停新分析和执行；
- `resume` 恢复分析和执行；
- `stop` 保存停止状态并让前台进程退出；
- `restart` 先请求该实例旧的 `runtime start` 或 `runtime restart` 进程退出，确认原进程已结束后，由当前终端以前台方式重新启动。重启命令会持续运行并输出 JSONL 日志，终端需要保持开启；超过全局 `--timeout` 仍未停止时不会启动第二个进程。

## 10. 常见问题

### 10.1 找不到或存在多个机器人

查看本人创建的机器人，然后把选定的 `robotCode` 交给 `--robot-code`：

```bash
dws chat +bot-search --page 1 --size 100 --format json
```

后台观察不依赖机器人列表；不设置 robot-code/robot-name 即可按活跃群与 ignore 筛选。只有显式要求机器人范围过滤时才需要核验所选机器人的 Code/名称。机器人交互入口仍需独立应用配置和能力核验。setup 失败不会提交半套配置。

### 10.2 ignore 群名没有找到

先查找完整群名，再重新执行 setup；也可以把 `openConversationId` 直接传给 `--ignore`：

```bash
dws chat +chat-search --query "群名" --page-all --page-limit 20 --format json
```

### 10.3 启动返回 `unavailable`

查看自动生成的通道名称及能力：

```bash
memgov channel show my-watcher-dingtalk
memgov channel probe my-watcher-dingtalk
```

DWS 主动观察要求已核验的 `history` 和 `receive`，不依赖 `send`。机器人交互入口要求已核验的 `receive` 和 `send`，实际回答还需符合原会话路由策略。按当前入口检查对应平台身份、能力与 Agent harness 登录；观察任务无需配置机器人发送能力。

### 10.4 群里有消息但没有模型调用

检查消息是否在首次启动之后到达、runtime 是否为 `running`，以及是否达到触发条数或等待时间：

```bash
memgov runtime status my-watcher
memgov runtime logs show my-watcher --level warn
```

### 10.5 Agent preset 变脏

```bash
memgov agent preset status claude-default
git -C "$MEMGOV_HOME/agents/claude-default" status
```

审阅并提交需要的规则修改，或恢复误改后再启动。运行时不会加载未提交的规则。

### 10.6 日志进入 degraded

检查 `<MEMGOV_HOME>/runtime/logs/` 权限和磁盘空间。日志故障时系统继续保存采集断点，但暂停创建新的 AI 任务。

## 11. 不同群使用不同 Agent

双模式配置的群助手可以设置默认 Agent，再为个别群指定专属 Agent：

```yaml
agents:
  group-helper:
    preset: claude-default
    capabilities: [conversation_history_read]
  product-helper:
    preset: product-rules
    claude_profile: cc
    execution_model: profile
    capabilities: [conversation_history_read, local_read, artifact_create]
    directories: [/Users/you/group-documents]
applications:
  group_mention:
    enabled: true
    source: work_chat
    channel: app-main
    default_agent: group-helper
    bindings:
      - conversation_id: YOUR_GROUP_ID
        agent: product-helper
```

上例需合并到已有数据源、通道配置中，并先启用相应 preset。运行 `memgov --config <配置文件> config plan` 查看影响，再使用输出中的版本与摘要执行 `config apply-runtime`。

`directories` 是明确提供给该群 Agent 的只读文字资料。系统把资料复制到该任务中，输出留在独立产物目录；群 Agent 不执行 Shell，也不直接修改原目录。隐藏文件、二进制文件和依赖缓存不读取；符号链接或资料超出限制时任务报错。详细限制见 [Agent 运行时详细稿](../design/agent-runtime-design-detail.md#按群解析-agent-与目录快照)。

模型、preset 和 commit 可从 `runtime task show` 的执行尝试中查看。配置变更后旧任务会失效，避免把原权限下生成的结果继续发到群里。此能力需要安装包含该功能的新二进制，已有运行进程不会因源码更新而自动升级。

## 12. 限定所有者 Agent 的目录

为所有者 Agent 添加 `directories` 后，系统只读取这些明确授权的原目录，在任务副本中修改文件。示例：

```yaml
agents:
  owner-assistant:
    preset: claude-default
    capabilities: [local_read, local_write, artifact_create]
    directories: [/Users/you/work/project]
```

若 workspace 是代码仓库，授权目录必须包含整个仓库根目录；系统建立私有 clone/worktree，以仓库 HEAD 为基础工作，原目录未提交修改保持不变。完成后会检查 Git 差异并记录本地提交。普通文件修改保存在任务的 `work/` 副本，生成产物保存在 `artifacts/`。

受控目录模式当前不执行任意 Shell 或项目测试，不能配置 `local_test`；这项能力需要经过验证的操作系统沙箱。未填写 `directories` 的现有代码任务继续使用原有 worktree、测试和本地提交流程。目录必须填写真实绝对路径，不接受符号链接；资料量限制与群目录快照相同。

## 13. 离线验证

不连接真实钉钉、不调用真实模型的测试：

```bash
cd "$HOME/src/memgov"
make test-runtime
make test-runtime-race
```

产品范围参见[钉钉接入设计](../design/dingtalk-integration-design.md)和[Agent 运行时设计](../design/agent-runtime-design.md)。

## 群助手首次自动挂载

开启 `applications.group_mention` 后，`config plan` 可以根据独立采集已核验的群范围，预览并自动补建机器人群路由。无需先手工为每个群添加 `assistant` 路由。

首次使用顺序：

1. 应用通道和数据源配置，核验 DWS 与应用机器人的能力。
2. 启动独立数据源，等待一轮完整的群列表、最近 30 天活跃筛选与机器人成员检查。
3. 开启群助手配置，运行 `config plan`；`group_mounts` 列出即将接管的群。
4. 使用当前计划摘要和版本执行 `config apply-runtime`，同一事务创建群路由并配置群助手。随后通过 `memgov service start` 统一运行；已有服务按已应用配置重新加载模块。

机器人回复仍只由可信群 @ 触发，并返回原群。`ignore` 的群名或会话 ID 优先；已经手工标成 ignore 的机器人路由也不会被覆盖。已有其他类型的机器人路由保留原策略。

发现收据、有效期及身份限制见[接入详细稿](../design/dingtalk-integration-design-detail.md#群助手首次挂载约束)。
