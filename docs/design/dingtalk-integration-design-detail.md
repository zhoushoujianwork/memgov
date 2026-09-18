# 钉钉消息接入与 AI 值守实现细节

## 后台观察与机器人交互

状态：源码已实现，离线验证已通过；真实平台与模型业务验收仍待部署者完成。范围以[主文档](dingtalk-integration-design.md)为准。旧“输入齐全才启动、受限原会话回答”的方案已被本节替代；`agent_origin_reply` 和 `runtime reply` 不作为现行协议。

### 职责与执行准入

DWS 采集只读，独立观察来源不要求机器人或发送能力。Haiku 按增量批次评估是否值得处理，默认 20 条或 300 秒；认为值得处理即创建独立后台任务。输入缺失是调查目标，不自动变成禁止执行的 clarification。后台 Agent 使用完整的技能、工具和验证能力，在 Owner 预设委托范围内自主调查与解决问题；无法继续时记录具体阻塞，不要求主 Agent 接手。

Owner 私聊和有效群 @ 直接创建交互任务，不调用筛选模型；非 Owner 私聊禁止。Owner 私聊使用本人权限，群始终使用群 Agent，即使发言者是 Owner 也不升权。后台任务不复用 Owner 的私聊会话和历史，不将来源消息伪装成本人直接指令。

### 独立沟通工具

后台任务可按 `owner_delegated` 委托显式调用：

```text
memgov runtime message send <task-id> --input <JSON-file>
memgov runtime message list <task-id>
```

发送输入为 `attempt_id`、`idempotency_key`、`target_type`（`group` 或 `user`）、`target_id`、`content`、`reason`、`evidence_message_ids`。目标使用原生稳定群 ID 或 userId，可联系相关私聊或群，不限原会话；不接受昵称冒充身份。目标通过本地同 profile 路由、完整 DWS 群目录或精确 userId 只读核验。实际披露的消息证据必须列入 evidence_message_ids，只可发给原群、经稳定 userId 核验的消息作者或 Owner；未披露具体来源的普通协作可无证据引用。经验仅指导时机与表达，不扩大授权。当前任务、尝试和权限必须有效；发送使用任务绑定的 DWS profile 与已核验 Owner，以本人身份并强制 AI 标识，不逐条索要确认。

每次发送独立保存决定、目标、正文、理由、证据、实际身份与回执。平台受理只表示 accepted，不等于已读或业务验收。同幂等键同内容返回已有记录；换内容重用键拒绝；failed 或 unknown 不自动重发。发送原生消息 ID 用于回流去重，openTaskId 不能冒充消息 ID。迟到消息 ID 通过只读 query-send-status 回填；保留最少 provider_send_task_id/provider_conversation_id，即使来源撤回或到期也可核验发送结果，公开原始回执仍遵守遮蔽规则。撤回或过期的正文及相关回执按可用性屏障处理。工具权限不构成完整 Bash 的 OS 沙箱，来源正文也不能授予额外披露权限。

Schema 22 固定企业/profile/userId 及证据修订。配置变化期间到达的真实回执仍可登记；任务版本变化或证据失效会隐藏旧正文、理由和原始回执，非正文标识继续用于只读核验与防重。身份不变的配置升级不会阻断迟到回执查询。

回流按稳定会话和平台消息 ID 精确识别；回执未齐时，同一已核验会话内、发送时刻之后（包含同秒）的本人消息先进入 waiting_receipt，管理台显示等待数量。获得原生消息 ID 后，仅 Agent 自发消息作为背景，真人 Owner 消息恢复正常评估。没有可查询的平台任务 ID 时保留 unknown，不猜测成功或失败，也不盲目发送；需要另行核对外部事实。

### 完成与配置兼容

`proactive` 的完成策略固定为 `record_only`，仅保存结果、产物、动作与费用。完成、失败、待确认和阻塞不自动向 Owner、主 Agent 或原群汇报；执行中主动沟通是独立工具动作。交互任务沿用 `reply_to_trigger` 回复当前用户或原群。

`applications.proactive.delivery` 默认 `record_only`；旧 `owner_direct` 归一化并给出诊断。`focus` 默认 `owner_relevant_work`，旧 `owner_related_quick_tasks` 兼容归一化。未指定后台 Agent 时提供完整的默认 Agent；显式 Agent 的权限限制保留，旧配置不会因升级自动获得 Bash 或外部委托。

旧后台通知的 draft/ready 失效，sending 保留未知事实，accepted 保留历史事实；通知创建、实际发送、手工 outbox dispatch/reconcile 后重试均检查当前完成策略。新旧发送记录都不能借完成状态绕过权限。后台不再要求 Owner 投递路由；旧数据库字段仅作兼容内部锚点，公开运行时读取不展示为可用投递目标。

### 机器人配置与权限

`applications.bots` 按应用通道名配置 `default_agent`、`owner_private` 与 `group_mention`。默认人设控制 preset 与模型；Owner 未显式指定 Agent 时继承人设并获得本人默认完整能力。Owner 显式 Agent 可收紧权限。群未覆盖时使用机器人默认 Agent，各群 `bindings` 可覆盖；群 Agent 必须使用 `conversation_published` 和 `owner_confirmation`，不能使用 `owner_request` 或 `owner_delegated`。

`owner_private.runtime` 绑定现有已核验本人运行实例。群 `source` 可省略；显式配置时才按同企业、同群及工作区读 DWS 背景，来源可以停止采集。没有历史通道时可在机器人条目填写 `owner: {id_type: user_id, id_value: ...}`，该 ID 必须已有 DWS 本人核验证据。旧 `applications.owner_private`、`applications.group_mention` 仍兼容，同一机器人或运行实例的重复冲突声明拒绝。

### 应用通道的发送身份

运行时只用可信通道、路由和同企业历史通道元数据生成 `ChannelSystemPrompt`，消息正文不能修改发送身份。应用机器人 Owner 私聊分成两类发送：本轮普通回答直接返回结果，由 Outbox 通过当前应用机器人投递到原私聊，不调用 DWS；Owner 明确要求向另一位用户或群另发消息时，只有绑定了同企业 DWS profile 且 `owner_request` 等当前动作策略允许，才提示 Agent 使用 `dws chat +messages-send --as user`。该路径固定 DWS 本人身份、AI 标识、稳定目标与幂等语义，不用 `--as bot`、Webhook 或父命令探测代替。未绑定 profile 时不得借用环境中的当前 DWS 账号。

群 @ 的通道提示不注入 DWS profile。普通回答只返回结果，由 `reply_to_trigger` 使用挂载的应用机器人发送到触发群；不得调用 DWS 本人身份发送，即使发起人是 Owner。跨群或私聊只有运行时显式提供且绑定精确目标的机器人身份工具才可执行，否则保留为群策略下的待处理操作。提示词负责让 Agent 正确选路，真正的普通答复受应用通道、原群路由和 Outbox 门禁约束；显式开启的完整 Bash 仍不是 OS 沙箱，不能把提示词描述成系统级进程隔离。

验收见[后台观察与机器人交互验收](../architecture/best-practice-scenarios-detail.md#后台观察与机器人交互验收)。

## 本轮离线验证（2026-09-17）

对齐主动值守、群专用 Agent 和事项闭环场景，本轮验证通过：

- `make check`：前端类型与 12 项前端测试、Go 格式、vet 和全仓库测试。
- `make test-runtime-race`，以及最后发送记录与迟到回执变更的定向 race 回归。
- `scripts/runtime-offline-acceptance.sh`：临时二进制、Schema 6/21 → 22、原库副本保护、fake DWS 断连/重启及 Cyber owner 审计回归。
- 后台无自动通知、信息缺失仍可调查、默认完整能力与显式限制、Owner 私聊与群权限、多机器人覆盖和上下文隔离、本人发送身份与跨受众证据、幂等/未知回执、配置变化、修订/撤回/到期、同秒及跨会话防循环。
- 多机器人配置示例展开后通过 `config validate`；本地文档链接和提交差异检查通过。

这些是离线代码、合成数据和假模型测试，不代表真实模型的判断质量或真实钉钉收发通过。新委托仍需通过配置预览/应用启用；真实模型、DWS 本人身份与 AI 标识、群回复及业务结果仍需部署者单独验收。

## 机器人引用消息

范围以[主文档](dingtalk-integration-design.md)为准。本功能对应“事项闭环”的私聊背景补充；源码实现与离线测试不等于已安装或真实钉钉验收。

`dingtalk_app/5` 从原始回调的 `text.isReplyMsg / text.repliedMsg` 提取引用类型、消息 ID 和内容，兼容文字内容为 `{text}`、`{content}`、字符串或 JSON 编码对象。引用 ID 优先使用 `repliedMsg.msgId`，缺失时使用 `originalMsgId`；缺少可读内容仍保留引用事实。聊天记录卡片只接收 `title / summary` 并标记 `summary_only`，不伪装为全文；不下载图片或文件，不解引用其他会话。

本人正文保持原样，引用单独保存于消息修订的 JSON snapshot 与 Source 证据，不增加数据库迁移。引用变化参与去重及修订摘要。私聊当前输入和已投递轮次的恢复历史均携带引用，使用 JSON 编码明确标为不可信背景；引用变化使持久 Agent 上下文重建。原文最多 4000 字、标题最多 256 字，超限截断并提示；未知媒体或无法解析的内容明确提示不可读。引用与所属消息遵循同一可用性及原文保留规则，过期清理同时清除引用和缓存副本。

离线回归覆盖文字形态、卡片摘要、不可读类型、截断、嵌套媒体凭据脱敏、去重与引用变更修订、撤回和过期读屏障、私聊任务输入、历史恢复及持久进程后续轮次。未解决项：真实平台验收、已安装二进制升级、图片/文件内容读取、已收到卡片全文的受控补回；后两项不属于本期范围。

用户范围与使用流程见[简明设计](dingtalk-integration-design.md)。主文档为范围决策依据。源码已实现群内 @ Agent、可信 mention 门禁、DWS 上下文绑定、群记忆召回、原群投递、独立数据源、可恢复历史导入、双模式 YAML 的统一通道应用、离线核验消息领取，以及逐群 Agent 和所有者声明目录的隔离执行。受控目录中的任意本地测试仍待系统隔离能力；源码能力不代表已安装二进制或真实钉钉验收。


## 双模式的数据与权限边界

- 采集任务以 DWS 来源、群和消息版本为断点，先提交 SQLite，再唤醒消费者。暂停 AI 消费不暂停采集；`ignore` 则排除新增正文采集及两种消费。
- 主动消费者拥有独立游标、批次、任务与所有者身份；群 @ 消费者以已核验机器人回调为触发，绑定群、请求者、Agent 挂载及权限版本。机器人自己的消息不再次触发。
- 群 Agent 查询 DWS 入库历史时，经显式来源绑定核对同企业、同群、可见范围和消息可用性；不能仅凭会话 ID 相同跨身份命名空间读取。共享本地消息引用，不复制正文到另一通道。多入口消息经已核验映射关联，不能按文本相同合并。
- 两个消费者各自保存处理断点；Schema 14 的已核验消息对应让同一逻辑事件只进入一个分析批次，另一观察标记 `linked_duplicate`。若群 @ 与 DWS 观察已核验且群 Agent 正在运行，即使主动值守先轮询，也由群 Agent 领取；任务详情保留两条内部消息的对应。真实平台对应没有得到证明前，两条消息保持独立；已投递的旧结果不能靠事后关联撤销。真实业务验证仍待完成。
- Agent 权限必须由执行器强制校验，包括工具及操作类别、文件目录、记忆范围和回复受众。Prompt 或技能文档不能扩大挂载权限。群回复只能包含向该群开放的资料及允许披露的结果；所有者的私有经验不能经群回复泄露。
- 原群回答是挂载时声明的 `reply_to_trigger` 授权，仅对有效 @ 和相同目标群生效；额外外部动作需要明确允许或进入所有者确认。执行、确认与交付绑定任务版本、配置版本和动作内容摘要。
- 原始文字在 `message_revisions.body`，规范消息及可用性在 `messages`，证据通过 `source_id` 关联 Source/Fragment；`coverage_windows` 和水位表示真实历史覆盖。当前无水位的补漏从一小时窗口开始，30 天群活跃筛选不等于 30 天历史归档。独立历史导入使用固定窗口与持久分页断点；新导入消息写 `context_only=1`，编辑后仍不触发旧任务。消息、覆盖和导入游标在同一 SQLite 事务提交。

当前 `runtime configure` 支持 `application_mode=group_mention`。该模式要求处理路由同时是应用机器人群路由，包含 `triggers:[mention]`、`send_policy=reply_to_trigger`；可选 `context_channel` 必须绑定同企业、同群、同工作区的 DWS 通道。普通群消息和机器人自己的消息只入库，不进入该消费者；有效 @ 在所有挂载群均单条触发，以 `mention:<本地消息 ID>` 建立独立任务，不调用 Haiku 判断是否为工作事项。问候、陈述和问题均交给执行 Agent 理解并回答；任务保留原文及证据引用，沿用批次修订、撤回和配置失效检查。任务标题截取原文前 12 个空白分词、最多 120 个字符；标题仅用于任务展示，群记忆由 Agent 按当前群共享策略按需查询，不按标题预装载。路由日志模型为 `local-mention-routing`，不计模型调用用量。配置 DWS 背景时可使用最近 30 条可用同群消息；无 DWS 背景时使用应用会话证据和只对该群发布的记忆。默认能力只有群历史读取和记忆读取，不继承所有者文件或 Shell 能力。额外外部动作的详情与口令随回答投递到任务原群，不额外私聊。只有已核验所有者在任务原群发送完整口令才能确认，其他群、私聊和其他成员无效。

## 双模式 YAML 声明与受限应用

本节示例对应本轮源码；安装版本须另行核对。旧单机器人配置仍兼容；多机器人配置见上文及[配置示例](../../config.local.yaml.example)。

以下结构已由 `config validate` 严格解析。`config plan` 只读 SQLite 与受控 preset Git 状态，不调用 DWS 或 Claude；`config apply-runtime` 要求当前 `plan_digest` 与 `applied_version`，在一个 SQLite 事务中重查并应用。通道可在同一 YAML 的 `channels` 中声明并一并建立；仍要单独核验平台能力，才可启动采集或 AI。群 Agent 的声明目录作为只读资料快照输入隔离任务；所有者 Agent 的声明目录进入私有副本与本地 worktree，`local_test` 暂被预览阻止。

```yaml
data_sources:
  work_chat:
    channel: dws-main
    enabled: true
    groups:
      active_days: 30
      ignore: []
      # member_robot: app-main  # 可选来源范围过滤
    history_import: {enabled: false, days: 30}
agents:
  group-helper:
    preset: claude-default
    memory_scope: conversation_published
    capabilities: [conversation_history_read, memory_read, artifact_create]
    bash: false
    external_actions: owner_confirmation
applications:
  proactive:
    enabled: true
    source: work_chat
    owner: {id_type: user_id, id_value: "OWNER_ID"}
    # 省略 agent：生成完整 Owner 委托策略；显式 Agent 则保留其限制
    focus: owner_relevant_work
    batch: {items: 20, max_wait_seconds: 300}
    analysis_model: haiku
    delivery: record_only
  bots:
    app-main:
      default_agent: group-helper
      owner: {id_type: user_id, id_value: "OWNER_ID"}
      owner_private:
        enabled: true
        runtime: owner-private
        # 省略 agent：继承 bot 默认人设/模型，使用完整 Owner 权限
      group_mention:
        enabled: true
        # source: work_chat  # 可选同群历史；自动群挂载需配置与在群证明
        bindings: []
        # - conversation_id: "GROUP_ID"
        #   agent: specialized-group-helper
```

管理原则：

1. 将数据源、应用、Agent、挂载解析成独立对象，检查引用、企业身份、重复群绑定、目录和工具能力。名称只用于配置引用；平台身份使用核验过的稳定标识。
2. 自动发现范围内的新群可继承 `default_agent`；显式绑定覆盖默认值，`ignore` 最优先。单个群首期只挂载一个 @ Agent，避免路由歧义。
3. 本地声明只表达期望状态；应用时展示新增接管范围、Agent 变化和权限变化，使用版本检查提交 SQLite 并留下操作记录。修改文件不静默改变正在执行的任务。
4. `runtime configure` 重配置会在事务内使旧分析批次、任务、动作和待投递结果失效；`sending` 与已执行中的外部动作进入 unknown。Schema 15 把应用版本绑定到执行尝试；Claude 和确认动作调用前后复核，完成写库前再次检查。外部工具调用进行中无法原子撤销，结果未知时仍须人工检查。两个模式可以独立停用，数据采集保持自己的状态。
5. 日志增加安全的应用模式、挂载内部 ID 和配置版本分类，不保存 YAML 中的凭据或平台标识。状态查询显示待应用差异、采集覆盖和每个消费者的积压。

验收至少覆盖：DWS 入库一次供两个消费者使用；主动模式评估有价值的事项并自主调查、记录结果；有效 @ 才触发且回复原群；群 Agent 无法读取所有者私有记忆或未挂载工具；同一 @ 不重复执行；自动新增群、ignore、机器人退出和权限收缩生效；消费者暂停不丢失采集断点；历史归档明确显示完整或部分覆盖。

## 当前持久化与状态机

Schema 10 保存不可变的 `applied_configs` 版本、规范化声明和受控对象摘要；Schema 11 保存独立 `data_sources`、运行实例到来源的绑定；Schema 12 保存 `history_imports` 固定窗口、游标、状态、覆盖计数及 `messages.context_only`；Schema 13 保存来源 `ignore_rules`、机器人 Code、历史导入开关/天数和来源启用状态；Schema 14 保存已核验的跨通道消息对应及一次领取；Schema 15 把已应用配置版本写入执行与外部动作尝试。SQLite 仍是唯一真相源，运行日志不参与恢复。

`data-source start` 以前台方式运行，依靠原 `channel_leases` 保证一个 DWS 通道只有一个接收者。启动前使用选定的 DWS profile 和显式 `contact +me` 只读调用核对企业与所有者 userId，成功后在 SQLite 为这个精确的 `user_id` 标识写入 `authenticated_dws_profile` 核验依据；不推断 `staff_id`、`open_id` 或机器人 ID 等价。身份不符阻止启动；身份接口暂时不可用时允许独立采集继续运行，但群 Agent 配置仍被阻止，`data-source attest-owner` 可重试。source `pause` 释放接收租约，`resume` 重新发现符合近 30 天活跃的会话；仅配置 member_robot 时再筛选机器人所在群；群名或稳定 ID 命中 `ignore` 时建立/更新 `ignore` 路由，不读取其正文。群发现失败记录受限错误码并等待重试，不把旧群集当作最新已核验结果。来源启用状态为 false 时不能恢复接收。来源独立启动，不能由配置应用事务启动子进程。

首次创建独立来源时，`route_ids` 固定为空，不从通道的既有群路由继承范围。通道中已有路由只表示路由配置存在，不等于该群满足近 30 天活跃及可选的机器人成员过滤条件；首次正向发现提交后才纳入来源范围，`ignore` 优先排除。未完成发现时也不自动创建历史导入任务。重新配置已有来源保留其已提交范围，不重新混入通道旧群；配置版本变化仍会使旧收据无法用于降级接收。

独立来源在启动、恢复、重连时立即发现群范围并补漏；连接健康时也按 `reconcile_seconds` 周期执行，默认 300 秒、最短 10 秒。每轮完成后再计时，慢查询不会造成重叠或积压补跑。群发现、历史导入和有界补漏使用独立工作循环；实时接收独立继续，每轮最多补漏 20 个会话、同时最多 2 个会话、每会话最多 5 页/100 条/30 秒，单轮最多 2 分钟，轮询游标与覆盖保存在 SQLite，重启从已提交断点继续。

活动判定使用固定的近 30 天半开窗口。Schema 17 将发现收据拆分为 `valid` 和 `complete`：`valid=true` 只说明本轮 `groups` 中每个群都有时间窗内消息，且满足已配置的机器人成员过滤（未配置时不依赖机器人）；`complete=true` 才说明已完成全域活动搜索及已配置的机器人成员核验，可以把未出现群移出范围。历史收据迁移后默认 `complete=false`，不能推断旧记录足以负向排除。

DWS 搜索的失败清单仅包含明确的 `stage=search-page-limit` 时，需计数一致且条目严格为 `stage` 与短字符串 `error`，才按有界分页截断处理；不记录原始 `error`，并强制 `complete=false`。认证、未知、混合失败或缺少完整性元数据仍拒绝整轮发现。DWS 搜索返回前 500 条等截断结果时，已出现消息的群仍是正向活跃证据；仅在配置机器人筛选时对这些候选群核验机器人。部分成功时采集范围合并旧群与本轮正向群，扣除显式 `ignore` 和已明确证明机器人不在群的会话。收据只写本轮正向群，不把合并保留的旧群重新写成新授权；群 Agent 只可挂载仍在时效和版本内的正向收据群，无需全域 `complete=true`。没有任何正向证明且发现失败/预算耗尽时使旧收据失效，保留旧范围与观察时间，不能据此新增挂载。完整成功才允许全量替换范围。

每轮总时间预算 2 分钟，1 次完整群枚举、1 次全局活动查询、显式机器人筛选时最多 64 个活动候选群的机器人查询，共最多 66 次 CLI 调用。即使枚举 1519 群，也不会逐群调用 1519 次机器人查询；活动搜索截断、候选群过多或个别核验失败时返回部分证明。未被搜索命中的群不等于沉默或退出，本期不承诺每轮完整证明大账号的全群状态。群集合未变化时保持原接收租约；新正向群、明确机器人退出或 `ignore` 导致范围改变时，等待旧订阅释放租约，再建立新范围订阅并补漏，最多一个 DWS 订阅。历史补漏/导入失败保留断点且不关闭健康接收。启动或周期发现遇到 `unavailable` 时，可在 24 小时内复用最近正向证据与现存 `route_ids` 的交集：来源和通道版本、机器人身份、接收/历史能力必须一致；既有路由沿用已授权且未修改的工作区，来源的 `workspace_id` 只决定新建路由的默认工作区，不要求既有路由与其相同；被忽略、失活或在证据生成后修改过的路由不能恢复。`valid=false` 的超时收据可用于该有限连续采集，但不会恢复 `valid`、刷新 `observed_at` 或授权新挂载；过期、无证据、身份/范围变化均阻止准入。明确拒绝等非临时错误会清空当前可复用成员集，后续超时不能再次放行。降级范围只用于接收与有界补漏，不改写配置路由、不新增群、不启动历史导入任务；历史导入在新发现成功后恢复。日志输出独立 `discovery_degraded`，状态保留 `discovery_unavailable` 等发现错误，补漏成功不会掩盖发现降级。

来源日志按来源内部 ID 写入 `<MEMGOV_HOME>/runtime/logs/`，同时输出 stdout。`data-source logs list/show/follow` 读取安全 JSONL，`data-source status.logging` 显示健康或降级。日志记录群发现、补漏、历史导入的计数、耗时和受限错误类型；不复制外部群 ID、正文和原始 stderr。写日志失败不回滚已提交的采集断点。

历史导入启用后对每个新接管群建立一个固定范围任务，默认 30 天。状态为 `queued → running → queued|completed|failed|cancelled`；每次领取有 2 分钟租约，单步最多 5 页 / 100 条 / 30 秒，记录不透明游标、重复数与缺口。任务取消和来源范围变化使旧领取无法提交；失败保留游标、受限错误码与退避时间。关闭自动导入仍可通过 `data-source history create` 建立明确窗口。覆盖不等于消息去重计数。

`config plan` 展示声明新增/更新/停用、当前对象版本、Git preset commit、授权扩张、漂移、未托管对象和阻断条件，摘要包含已应用版本及读取快照。`config apply-runtime` 在写锁内重新预览；摘要或版本变化就拒绝。通道、来源与应用配置可在同一事务中建立，但新通道能力仍未核验，采集及 AI 启动另有门禁。已有通道身份重定向和未托管同名对象拒绝应用；通道凭据引用或路由边界改变须授权，新增出站路由还需权限扩张授权。应用生成源、主动或群运行配置，保持进程未启动；应用版本与对象映射同时提交。DWS 与应用群路由必须已核验同企业、同群、同工作区，群 Agent 默认配置作用于当前接管群及随后自动发现的群。

双模式 YAML 首次应用可以先创建 `route_ids` 为空的独立来源；此时启用的主动值守声明保留，但值守实例延后创建，也不能启动。来源完成正向群发现后，需要再次运行 `config plan` 和 `config apply-runtime`，才能从来源已提交范围与当前有效的近 30 天活跃收据（以及配置了机器人过滤时的在群证明）的交集创建值守。部分发现会为采集保留旧群，但收据只证明本轮正向群，旧群不会因此进入 AI 分析范围；临时发现失败时，运行中的来源只可沿用 24 小时内仍符合原收据、身份与未修改路由的有限范围。后续发现扩大或缩小范围时，相同 YAML 的下一次应用会刷新已停止值守的群范围；运行中的实例须先停止。计划摘要把来源群范围和发现收据纳入版本栅栏，因此发现发生在计划与应用之间时旧计划会被拒绝。通道中的历史群路由只提供路由资料，不会成为主动值守的准入范围；范围清空后，值守不能重新启动。

Schema 14 保存 `verified_message_associations`。内部 adapter 只有在分别查询两条平台消息，且平台给出相同企业内群与逻辑事件证明时才能建立关联；库内只保存证明摘要。`ClaimRuntimeBatch` 在同一 SQLite 写事务里一次领取已关联事件，群 @ 优先，并从群上下文剔除对应重复观察；任务详情显示已核验的内部对应。当前没有可用于真实 DWS/应用消息对应的生产平台查询实现，`message association <message>` 对未核验消息返回 `unresolved`；离线 fake verifier 测试不能替代真实联调。[钉钉 Stream 协议](https://open-dingtalk.github.io/developerpedia/docs/learn/stream/protocol/)将头部 `messageId` 定义为一次推送标识，机器人回调另带 `msgId`；[DWS 消息契约](https://github.com/DingTalk-Real-AI/dingtalk-workspace-cli/blob/main/skills/multi/dingtalk-chat/references/contracts.md)没有声明它与机器人 `msgId` 等价，因此不能把样本 ID 相同当作生产证明。群任务从已应用版本按 route 解析 Agent preset、profile、模型及能力，读取目录中的受限文字快照，只能在隔离 `artifacts` 目录创建产物；Shell、直接读取原目录和越界产物被阻止。所有者声明目录使用私有副本；代码项目先做本地私有 clone，再使用 worktree 修改和创建本地提交。源仓库的未提交内容、hooks 和配置不会继承；任务执行前后全树检查符号链接及特殊文件，原目录不写入。受控目录的 `local_test` 需要可靠 OS sandbox，当前被阻止。真实钉钉测试仍需验证新进程、已安装命令、机器人回复及数据覆盖；[离线验收说明](../guides/runtime-offline-acceptance.md)仅覆盖 fake DWS 和重启恢复。

## 通道契约

DWS 采集与独立 Owner 沟通分开，后台不使用适配器自动机器人私聊交付，也不依赖发送能力启动。应用机器人自身的交互收发保留。

所有适配器实现 `ProbeCapabilities / ReadWindow / RunReceiver / Send`，只接收冻结的 channel 配置并返回规范事件或投递结果。适配器不读取记忆、不决定受众、不直接修改数据库。

`dws_personal` 使用固定 profile：

- `dws profile list` 验证企业和本人账号；
- `dws event +listen-im` 输出实时 NDJSON，stderr 的 ready marker 作为就绪依据；
- `dws chat +chat-messages` 按半开时间窗口分页补漏，包括实时事件会过滤的本人消息；
- 显式设置机器人范围筛选时，使用机器人目录/成员接口核验；
- 独立沟通工具按任务委托调用 `dws chat +messages-send --as user --ai-tag true --idempotency-key ...`，该调用不属于采集或完成通知。

调用均使用 argv 数组，不经过 shell。错误映射为受限错误码，原始 stderr 不进入运行日志。独立沟通复核稳定身份和披露证据，保留 accepted/failed/unknown；同幂等键不盲目重发。完整 Bash 仍可调用其他程序，该工具不是系统沙箱。

`dingtalk_app` 使用官方 DingTalk Stream Go SDK。凭据只以 `env://`、`file://` 或 `keychain://` 引用保存在配置中，连接时解析。回调内容在最小消息事务提交 SQLite 后才返回成功 ACK；解析失败只有在拒绝记录提交后才 ACK。新提交随后通过进程内按通道广播的合并唤醒通知所有相关 Runtime，直接扫描并领取数据库中的真实工作；通知不承载正文且允许合并或丢失，默认 1 秒扫描负责兜底。会话 webhook 和其他凭据字段在保存前清除。该通道验证 receive，并使用应用 access token 私聊及触发原群交付；本人私聊与有效群 @ 使用原消息“已收到”表情，不另发文字回执；history 保持未验证，运行时从 `bootstrap_at` 开始处理新消息，不因缺少历史能力拒绝启动。

运行实例的增量同步只重查新消息、修订、撤回、等待回执及仍可能受保留策略影响的状态，不在每次 tick 的 SQLite 写事务中重扫已稳定的完整会话。同一服务进程内打开相同 `state.db` 的模块共享可取消的写入门闩，再进入 SQLite 事务；跨进程冲突仍由数据库超时与幂等重试处理。这样旧会话增长或采集、私聊、群聊并发时，机器人接收、接收表情、任务完成和租约续期仍能及时取得写锁。

独立 data-source 在 dws_personal 通道持有唯一采集租约，实时消息、周期补漏和可选历史导入先落 SQLite；后台观察只消费已提交且范围有效的证据。机器人 Stream 独立承接 @ 与 Owner 私聊，可在来源未启用时工作；显式 source 才提供额外同群历史。私聊来源默认停用，另行启用并遵循[保留策略](direct-message-retention-design.md)。旧未绑定来源的运行实例保留兼容采集方式，但不能与独立来源持有相同接收租约。

## 规范消息与证据

Inbox 以平台事件 ID或内容摘要去重。Message 使用 channel ID namespace、conversation ID和 provider message ID形成稳定键；编辑追加 revision，历史观察不覆盖。每个可用文字 revision 同时创建 Source、Fragment 和 `source_origins` 关联。

撤回将 Message 和相关 Source 标为 unavailable。撤回先到时保存 tombstone，后续历史补回正文也保持不可用。运行时读取撤回任务时保留本地消息 ID和 revision，正文及证据字段为空。

外部平台 ID只保存在 SQLite 业务记录，不写入独立运行日志。

## 现有值守配置与迁移

当前 runtime 配置区分 proactive、direct 和 group_mention。完成策略由模式派生，后台不需要投递目标；旧数据库非空外键以处理路由作内部兼容锚点，公开读取返回空投递地址，不产生发送资格。

`runtime_configs` 的主要字段：

| 字段 | 约束 |
| --- | --- |
| `channel_id` | proactive 需要 receive/history；应用交互需要实际 receive/send，应用 Stream 不要求 history |
| `route_ids` | setup 及周期同步发现的非 ignore active route，至少一个 |
| `delivery_route_id` | proactive 可省略且不参与投递；Owner 私聊/群交互分别核验对应路由 |
| `completion_policy` | 派生为 proactive 的 record_only 或交互的 reply_to_trigger |
| `owner_principal_id` | 精确 DWS authenticated_dws_profile 核验的稳定 user_id；显示名及 ID 同值猜测不能授权 |
| `analysis_model` | 默认 `haiku` |
| `execution_model` | 空值表示沿用 Claude 本地配置 |
| `agent_preset` | 默认 `claude-default`，启动前必须 enabled、clean |
| `item_threshold` | 默认 20，范围 1—100 |
| `max_wait_seconds` | 默认 300 |
| `reconcile_seconds` | 默认 300，范围 10—86400 |
| `concurrency` | 首期固定 1 |

配置更新要求运行时未处于 running，并使用 expected version。`pause` 和 degraded 仍继续接收；`stop` 使前台循环退出。

## 采集与补漏

来源使用群事件订阅并按已授权范围过滤，近 30 天活跃与 ignore 是基本条件；仅显式机器人过滤时核验其成员关系。正向发现可扩充范围，完整发现才用于全量缩小，部分/临时失败按收据边界保留采集连续性。机器人群 Agent 的自动挂载仍另需机器人在群证明。启动立即补漏，随后按 reconcile 周期分批执行：

```text
start = covered_until - 5m（无水位时 now - 1h）
end   = now
page_limit = 5
max_items = 200
```

只有平台明确返回 complete 且没有剩余分页时推进覆盖水位；任何上限、解析错误或调用失败都留下 gap。长连接退出后 1 秒重连，重连期间历史窗口继续负责补缺。

`runtime_message_states` 按 runtime 和 message 唯一。初次看到且 `sent_at <= bootstrap_at` 的消息为 context；之后为 pending。每个 route 独立累计，达到条数或最早 `first_seen_at` 超过等待时间时领取最多 100 条。没有 pending 内容不创建 batch，也不调用模型。

后台分析除事项外也识别可复用知识。具名系统的稳定能力、限制、接口规则、职责边界、决定、方法和已验证经验应进入 `memory` 决策；简短技术问答在回答明确且得到确认时即可作为证据，不要求对话里再次出现“记住”字样。猜测、凭据、闲聊、临时故障和普通进度仍只保留为上下文或来源。

## 分析与任务状态

分析批次保存输入 digest、消息 revision、模型、状态和结构化输出。失败时消息从 batched 回到 pending。决策只能引用当前 batch 的本地 message ID。

`memory` 任务的执行 Agent 只做只读召回和候选内容生成，必须在结构化结果的 `candidate` 字段返回完整 `CandidateInput`，不能自行提交或应用候选。后台统一执行 Submit Candidate → 独立 Review → 按已接受 digest Apply，任一步失败都保留明确状态，不能把 Agent 自行提交的游离候选当作任务完成。

去标识化的假模型测试覆盖 Source → Candidate → Review → Memory 的确定性状态链。可选 `MEMGOV_LIVE_MODEL_TEST=1` 仅在操作者自有的隔离环境中执行，真实聊天内容不得提交。

任务以 `(runtime_id, canonical_key)` 唯一。update 修改原任务并增加 version；cancel 增加 version 并取消。消息编辑会让引用旧 revision 的任务 stale，撤回会取消引用任意 revision 的任务。所有运行中的旧尝试同步变为 stale，完成操作用条件更新再次检查版本。

旧版曾将 clarification 由机器人发给 Owner；本轮停用该主动通知。缺输入但值得调查的事项仍进入独立 Agent，实际无可行下一步则保存具体原因。后续实质补充用相同 canonical key 更新任务，不用无变化自动 retry 制造重复执行。

## Claude Agent 和记忆

后台 Haiku 评估与记忆复核保持空工具集合。执行使用独立 Owner Agent，复用完整执行能力和策略校验、保持独立 task/attempt；代码修改按工作目录策略验证，普通调查不要求制造 Git 提交。群 @ 则使用机器人/群 Agent 的能力与受众范围。memory 候选仍须独立 Review 再按 digest Apply。

所有者 direct 会话使用独立的原文传输入口，不进入上述分类、预召回、结构化任务输出或 worktree 提交检查。Agent 的自然语言回复作为交付正文；工具、身份、消息版本和对外动作的确认边界仍有效。具体会话与命令协议见下文“所有者即时私聊”。

## 确认和动作执行状态机

以下确认流程用于交互式 owner_confirmation。后台 owner_delegated 按预设执行，后台完成和待确认状态均不触发自动通知。

```text
pending --owner origin-group / direct token--> confirmed → executing → executed
                                      │          ├→ failed（确认动作开始前）
                                      │          └→ unknown（外部调用开始后结果不明）
task version changed ─────────────────┴────────────→ stale / unknown
```

确认口令包含本地 action ID和 payload digest 前缀。验证以已经入库的 Message 为依据，核对 channel、群任务的精确触发 route（其他模式为绑定 direct route）、owner principal、发送时间、availability、精确正文、task version 和完整 payload digest。CLI 的 `runtime task confirm` 输入 `message_id`，不能传入自报 sender/origin。若消息事件只提供 union ID，配置前需要用 `message identity link` 把它与机器人单聊接受的本人 user/staff ID建立已核验映射。

confirmed action 使用单独 Claude 模式，加载任务上下文但只允许执行已确认的一个目标和 payload。动作通过条件更新单次领取。所有动作 executed 后任务回到 completed 并发送最终结果；failed/unknown 需要人工检查，未知结果禁止自动重试。

### 群任务在原群确认

群 @ 的待确认内容合并进原群 `result` 投递，不另建私聊消息。确认扫描只读取该任务的触发群；不存在唯一所有者私聊路由也不影响群任务。消息主体必须仍是已核验所有者，原文与动作版本仍有效，口令完整匹配；其他成员、其他群、私聊、撤回或过期消息不能批准。平台已核验选中机器人的群消息允许正文带一个开头的 `@名称 `，去掉该前缀后仍必须是完整口令；否定、引用或额外说明不会当作确认。识别已知口令只阻止重复 AI 任务，不授予权限；普通私聊和群 @ 仍直接进入 Agent，不调用 Haiku。执行确认动作及最终结果也回到原群，未知执行不盲目重试。

## 群回复与确认卡片

2026-09-17 源码增量：普通群答复使用机器人 Markdown 消息，保留问题引用、回答和耗时栏，不创建 StandardCard；`@提问人` 放在正文末尾且只出现一次。真正的原生提醒使用触发回调自带的 `sessionWebhook` 和嵌套 `at.atUserIds`；主动群消息接口不支持 `@`，不能把返回成功误判为提醒生效。webhook 只在回调消息提交成功后进入有界进程内缓存，按 channel、conversation、provider message ID 和真实 user ID 精确匹配，不进入 SQLite、日志、模型或导出；服务重启、凭证过期或身份不匹配时，降级为正文显示昵称的一次普通 Markdown。待确认时才使用审批卡片，并增加经过同企业 DWS 认证的所有者；昵称仅作显示，不授权。群目标始终取 task 的精确触发 route，不发送跨群或私聊提醒。本文对应[主文档](dingtalk-integration-design.md)。

应用通道可独立配置 `identity.confirmation_card_template: <应用关联模板ID>.schema`，通过现有配置预览、应用流程生效；不新增数据库 schema。配置后 pending 群任务使用 `format=confirmation_card`，`/v1.0/card/instances/createAndDeliver`、`callbackType=STREAM`、`userIdType=1`、原群 `openSpaceId`，禁止转发。`atUserIds` 提醒需求发起人及所有者；union ID 只有存在唯一的已核验 user ID 链接时用于高级卡片，不猜测同值地址。模板未配置时使用普通 Markdown 在原群展示口令确认，并明确提示按钮尚未配置；旧投递保留审计，不转发。

部署者可关联自有审批模板。模板变量、纯文本转换、截断、同意/拒绝回调及授权边界按本节协议执行；真实模板 ID 不进入公开仓库。

Outbox ID 作为 `outTrackId` / `cardBizId`，冻结任务版本、正文、提醒对象和展示动作的完整 payload digest、kind、target。平台返回成功且精确原群的 deliver result 带 carrier ID 才记 accepted；缺字段、未知结果和网络失败不乐观记成功，不盲目重发。接收表情仍是独立 reaction，不变成文字或卡片回执。

应用 Stream 接收租约每个周期续期。SQLite 短暂写竞争返回 `unavailable` 时，在当前租约真正到期前重试续期并保持同一连接；租约 fence 已被替换时立即停止。这样后台采集的长写入不会因一次 busy timeout 反复拆建机器人连接，同时失效接收者仍不能继续落库。

配置模板后的正式接收会话新增 `/v1.0/card/instances/callback` Stream 订阅，probe 和无模板会话不订阅审批回调；与消息共用有界队列和串行写入。点击回调必须经过当前接收 lease/fence，校验真实 `corpId`、DWS 认证所有者、Outbox、精确原群、已受理状态、任务/消息/路由/模板版本及每个展示动作的完整 digest。钉钉当前文档列出的 Stream 回调字段是 `type`、`corpId`、`userId`、`outTrackId` 和 `content`，不包含 `userIdType` 与群场域；缺失时按该卡片冻结的 `userIdType=1` 及 Outbox 原群恢复，显式冲突值仍拒绝，不能因可选字段缺失丢掉真实点击。同意只把原卡片展示的动作转为 confirmed；拒绝把这些动作转为 rejected、任务转为 cancelled，并且不进入执行器。决定记录 `dingtalk_card:<outboxID>:<frameEventID>` 与 owner principal；重复点击重新核验但不再次决定或执行，已拒绝与已同意之间不能切换。发送状态仍在入库时要求平台重投，不丢失早到的点击。

同意成功后回传 `status=agree`，沿用既有 confirmed action 领取、思考执行及原群最终交付；拒绝成功后回传 `status=reject`，原卡片直接显示已拒绝且不执行。非所有者/失效卡片不更新共享卡片，拒绝记录提交后才 ACK；存储或 lease 失败不 ACK。卡片展示的动作不能由 CLI 或聊天口令绕过按钮。执行前及工具门禁重新核验来源、所有者、原群和批准快照，修改后的 payload / target / kind 不继承旧批准。

离线验证覆盖原生单次 `@` 的回调回放、主动端点无效字段防回归、回调写入失败不保留 webhook、共享 Adapter 装配、双提醒、原群发送、事件标题与纯文本操作摘要、同意与拒绝、官方最小回调字段、拒绝后不可执行、Stream 协议、非所有者/跨企业/跨群/错误 ID 类型、撤回、变更 payload/target/任务/模板、身份或路由撤销、重复点击、未展示的新动作、口令绕过、提交失败和 lease 失效。`internal/channel/dingtalkapp/testdata/bot_conversation_cases.json` 保存去标识化的群 @、普通群消息、Owner 私聊和普通用户私聊回调，解析与运行时测试共同读取它；普通用户私聊必须保持零 Agent、零回执、零回复。原生 `@` 快速回放命令为 `go test ./internal/channel/dingtalkapp -run TestRecordedCallbackSendsNativeMentionThroughSessionWebhook -count=1`，完整定向回归为 `go test ./internal/channel/dingtalkapp ./internal/runtime`。对应“群专用 Agent 的事项发现、受控处理与交付验收”场景。真实平台上的模板关联、按钮往返和资源操作仍由部署者自行验收。

可复现验证包括 `make check`、`make build`、钉钉应用/CLI/运行时竞态检查以及卡片定向测试。真实模板 ID、群标识、用户标识和平台回执保留在部署者环境。

协议依据：[官方 StandardCard 与 @ 示例](https://open-dingtalk.github.io/developerpedia/docs/explore/tutorials/stream/bot/go/send-streaming-card/)、[官方 Stream 按钮回调与模板配置示例](https://open-dingtalk.github.io/developerpedia/docs/explore/tutorials/stream/bot/go/card-callback/)、[官方 Card API SDK](https://github.com/alibabacloud-go/dingtalk/blob/master/card_1_0/client.go)。

## 交互接收回执

2026-09-17：有效请求通过身份、会话、来源版本和运行范围校验并持久化任务后，先在原消息添加“已收到”，任务领取后替换为“处理中”，成功完成后替换为“已完成”，失败或外部操作结果未知时替换为“打叉”。本人私聊与有效群 @ 共用这套阶段状态机。等待所有者审批属于尚未完成，继续保留“处理中”；当前 Agent 调用可以结束，审批通过后由服务重新领取原任务的确认动作。两入口均不调用 Haiku 筛选；`/clear`、`/status` 在本人私聊由系统直接处理，不添加 Agent 阶段表情。普通非 @ 群消息及主动观察不发送状态标记。

四个阶段分别使用独立 Outbox：`runtime_receipt`、`runtime_processing_receipt`、`runtime_completion_receipt`、`runtime_failure_receipt`，均为 `format=reaction`，正文分别保存“已收到”“处理中”“已完成”“打叉”。交互 Runtime 把阶段请求放入单一有序旁路队列，队列依次调用应用机器人 `RemoveReaction` 移除上一个已接受阶段，再用 `AddReaction` 添加当前阶段；排队后 Agent 立即开始，不等待平台表情接口返回。任务真相已先提交，进程在发送前退出时由启动对账补齐适用阶段。所有操作都使用任务原消息的真实 conversation ID 与 provider message ID。旧文字回执不能作为新状态发送，不做文字回退。表情失败、不支持或结果未知只记录本地原因，不阻止 Agent；已开始发送的阶段不盲目重试。回答仍为独立 `result`，阶段标记不会进入本人会话的已交付回答历史，也不会作为未知外部答复阻止任务继续。

群挂载目录发现与历史补漏由独立串行循环处理，不再运行在每秒交互消费循环内。发现最长可等待两分钟，但期间仍使用上次已核验路由接收和处理新 @；刷新完成后写回的新范围由后续唤醒或兜底扫描读取。该拆分不扩大旧路由权限，路由撤销仍以数据库中当前配置为领取门禁。

服务启动会按 SQLite 任务真相对账私聊与群 @ 阶段：`running` / `awaiting_confirmation` 补“处理中”，`completed` 补“已完成”，`failed` / `action_failed` / `action_unknown` 补“打叉”。只处理已经存在接收阶段记录、且目标阶段尚未开始或仍为 `ready` 的任务；`sending`、`unknown` 或已有终态不盲目重发。这同时覆盖任务状态已经提交、对应阶段 Outbox 尚未创建便再次崩溃的窗口。管理台分别显示“接收回执”“处理中标记”“完成标记”“失败回执”，因此服务恢复后仍能核对每个阶段；原消息上只保留当前标记。源码与离线验证已完成，本机安装及真实钉钉阶段替换效果尚未验收。

异常任务的文字收尾与“打叉”阶段分开记录。Owner 私聊的直接 Agent 单轮最长运行 30 分钟；即时执行失败，或服务启动时把遗留 `running` 任务和尝试收口为 `failed/runtime_restarted` 后，运行时为已经成功接收的任务创建 `reason=runtime_failure_notice` 的机器人通知。群 @ 任务进入 `failed`、`action_failed` 或 `action_unknown` 时，如果任务结果、摘要或动作尝试中已有非空结论，也创建同目的的 `bot_group` 回复，保留已有结论并明确说明未完整完成或结果未知；没有结果的群任务仍只显示失败阶段，不编造正文。群回复绑定原任务路由、原消息和发起人提醒，服务启动会补齐尚未创建的通知。准备与发送前重新核验身份、原任务来源和出站路由；相同任务版本与内容共用幂等 Outbox，`sending`、`unknown` 或已有终态不自动重发。`action_unknown` 通知要求先检查目标系统，不能直接重试。

验证覆盖本人私聊、非默认群的有效 @ 与问候、普通非 @ 静默、私聊 30 分钟执行上限、恢复后运行中任务与尝试的失败收口及幂等机器人通知、群任务已有结论时失败或结果未知仍回复原群、私聊与群 @ 阶段按“已收到 → 处理中 → 已完成／打叉”替换、阶段独立持久化、慢表情接口不阻塞 Agent、提交后唤醒绕过长兜底周期、群发现阻塞时 @ 仍可处理、失败/未知/不支持不阻止 Agent、重复消息不重复发送、等待审批保持处理中，以及完成/失败状态已提交但阶段 Outbox 未创建时的启动恢复；还覆盖待确认操作随原群答复展示、所有者同群确认、跨群/私聊/其他成员拒绝确认，以及投递与来源权限门禁。测试入口为 `internal/runtime/*reply_test.go`、`delivery_reaction_test.go` 和 `internal/core/runtime_test.go`。真实模型与真实钉钉往返尚未验收。

## Outbox

本节只描述交互回答及接收回执的 Outbox。proactive 的自动通知入口停用，历史待发通知失效且不能重试；独立沟通存于 runtime_message_actions，按工具幂等键管理，详见[独立沟通工具](#独立沟通工具)。

每个任务版本和结果正文形成唯一 input digest。流程为：

```text
ready → sending → accepted | failed | unknown
```

投递失败不重新执行任务。已有同 digest Outbox 处于 accepted、failed 或 unknown 时，运行时不会创建或发送第二份。启动恢复把遗留 sending 和 delivery attempt 置为 unknown。

显式受限 Owner 私聊的待确认正文包含任务结果、pending action 的类型、目标、完整 payload 和口令。群 @ 合入原群卡片回答，有模板时只允许 Owner 点击，无模板暂用同群口令；不创建额外私聊通知。交互确认动作完成后可以投递实际结果，后台始终仅记录。

## 独立日志

路径为 `<MEMGOV_HOME>/runtime/logs/<runtime-id>/YYYYMMDD-HHMMSS.*-PID-SEQ.jsonl`。目录和文件权限分别为 `0700`、`0600`。文件与 stdout 使用同一 `schema_version=1` 结构：

```json
{
  "schema_version": 1,
  "timestamp": "RFC3339Nano",
  "level": "info|warn|error|debug",
  "component": "analysis|execution|action|delivery|dingtalk|runtime|logs",
  "event": "completed",
  "runtime_id": "内部 ID",
  "batch_id": "内部 ID",
  "task_id": "内部 ID",
  "attempt_id": "内部 ID",
  "trace_id": "内部关联 ID",
  "status": "completed",
  "duration_ms": 120,
  "error_code": "",
  "model": "haiku",
  "input_tokens": 100,
  "output_tokens": 20,
  "cost_usd": 0.001,
  "tool_kinds": ["git"],
  "summary": "识别到 2 个待处理事项"
}
```

logger 不提供自由扩展字段。摘要最多 240 字，清除控制字符，遮蔽 token/key/password 形态和 URL。调用方只写固定分类摘要。文件按 UTC 日期及默认 10 MiB 滚动；清理删除 30 天前文件，并从最老文件开始控制总量 1 GiB，活动文件不删。

`logs list/show` 使用 JSON envelope。`show --task` 返回日志事件及 SQLite 中的任务详情。`logs follow` 和 `runtime start` 输出纯 NDJSON。日志写入、stdout 或清理失败时 runtime 进入 degraded；AI tick 停止，接收 goroutine继续落库。

## 恢复与验证

启动时：分析批次退回 pending；本地 running 尝试标记 failed；外部 executing action 和 sending delivery 标记 unknown。SQLite 是恢复依据，日志是否存在不影响状态。

自动测试覆盖核心状态机、并发领取、dws 故障、Stream ACK、模型参数、preset Git、worktree、日志轮转/过滤/follow/配额和凭据脱敏。真实钉钉验收需要目标企业授权，至少验证：本人承诺能由历史补回、目标群的数量/时间触发、owner bot 私聊、精确口令确认、真实查询或代码任务、本地 commit、投递受理及一次模拟断网恢复。

以上是旧实现的验证范围；主动观察的新策略以[新验收矩阵](../architecture/best-practice-scenarios-detail.md#后台观察与机器人交互验收)为准，旧通知通过项不作为新方案验收。

### 主动值守的跨通道确认入口（旧策略源码记录）

以下保留当时实现事实。新设计停用主动观察的私聊确认通知，不再以此入口推进主动观察；本人私聊的确认入口保留；群 @ 改为任务原群中的所有者确认，不使用本节旧跨通道私聊入口。

主动值守发送机器人私聊后，`ProcessRuntimeConfirmations` 从 SQLite 查找确认，不要求 DWS 单聊接收。跨通道入口必须同时满足：

- 主动实例的 Owner 使用 `user_id`，与 DWS 通道的 `expected_user_id` 完全一致；该 exact user_id 已经由认证 DWS profile 核验，alias 的 basis 为 `authenticated_dws_profile` 且与实例 Owner principal 相同。
- 应用通道与 DWS 同企业；应用的 `identity.history_channel` 显式指向该 DWS 通道（名称或内部 ID 均可）；如果 DWS 配置了 `delivery_robot_code`，还必须与应用 `robot_code` 相同。符合条件的应用必须唯一。
- 应用只有一个 active、非 ignore、`dispatch_only` 的 direct 路由；消息必须出现在这个会话内。另一个群、另一个单聊、另一个企业或未绑定应用都不能确认。
- 当前消息可用，exact sender alias 仍为 verified，并解析到同一 Owner principal。应用回调解析器 `dingtalk_app/2` 将 `senderStaffId` 归一为精确的 `user_id`；缺失时仍以 `senderId` 的 `union_id` 回退。不同 ID 类型即使字符串相同也不视为同一人；历史 `staff_id` 消息必须有独立且明确核验的跨类型 identity link 才能关联。
- 整行口令与待确认 action ID、payload 摘要一致，消息时间不早于动作创建；action 必须 pending、任务仍 awaiting_confirmation、任务版本与 payload 摘要均未变，来源消息 revision 仍有效。

旧同 DWS 通道中已保存的本人确认消息保留兼容校验，但不再以 DWS 单聊监听作为自动接入路径。已知 action 的精确口令在 direct 运行实例中只作控制消息，不创建 AI 任务；这种识别本身不授予任何执行权限。确认结果与 message ID 写入 SQLite，重复扫描不重复确认。日志仅记录确认数量和安全错误码，不复制口令或消息正文。

### 所有者即时私聊

所有者的每条新私聊单独领取，批次只作为可靠传输收据，标记 `direct_agent`，不调用 Haiku，也不检查意图关键词。入站的当前身份必须通过核验，处理路由显式属于 direct 运行实例。回调 CID 和出站本人 userId 属于不同命名空间；两路由必须在同企业、同机器人应用通道，出站地址必须精确匹配已核验所有者。

Schema 18 的 `runtime_direct_sessions` 保存会话和关闭时间，`runtime_direct_turns` 将原消息任务绑定到会话，重复事件和 retry 不重复建立会话。完整且去除首尾空白后恰好为 `/clear` 或 `/status` 才是命令。其他 slash 文本、问候、追问、取消和补充原样发送给 Agent。现有有效确认口令仍由确认状态机处理。

`/clear` 关闭原会话并新建会话，不清除正式记忆或审计记录。确认来源仍有效的后续 `/clear` 是提交和投递的屏障：即使旧 Agent 正在执行，其结果也不能完成或投递。其他发送者、其他路由和撤回的 clear 无效。`/status` 解析当前任务的实际 Agent 策略和 Claude 技能发现结果，直接回复 Agent、preset、模型、技能、能力、Bash、外部操作、记忆范围和热词写入权限；两类命令均不启动模型，也不写入会话恢复文本。

每个会话在 `<MEMGOV_HOME>/runtime/sessions/<session-id>` 工作，使用持续的 Claude `stream-json` 进程。stdin 的 user content 为当前消息正文，Agent 自行决定使用工具和 skill，不接收业务任务 JSON Schema。支持任务继续的新调用会按原生会话 ID 保存可恢复执行上下文；未提供原生会话 ID 的兼容路径仍禁用本地会话持久化。SQLite 保存恢复关系与任务版本，原生文件只保存临时执行上下文，见[任务继续详细稿](task-continuation-detail.md)。重启后只从 SQLite 恢复同一会话中已交付、原消息版本仍有效、发送者身份仍核验的本人/机器人轮次；当前进程的历史摘要不匹配时重建，以剔除撤回或未交付上下文。

记忆 skill 随程序内置并安装到会话的 `.claude/skills`。运行时不替 Agent 选择查询词，也不自动将普通聊天写成记忆。本人私聊未配置独立 Agent 时默认完整 Bash 与 `owner_request`，使用真实 memgov CLI；本人明确请求的外部操作直接执行，不索要额外确认口令。此模式使用运行账户权限，不能用文件工具或记忆包装器宣称完整隔离。

显式关闭 Bash 时，命令包装器绑定当前 home/workspace，按能力开放查询及 Source/Candidate/Review/Apply 流程，不允许跨作用域覆盖、清除或恢复数据库；对外操作通过受控 `memgov-action` 调用 `runtime task propose-action <task-id>`，记录当前 attempt 的具体 kind/target/payload，随后按 `owner_confirmation` 等待本人确认。工具不解释回复语义、不执行外部写入。

`applications.owner_private` 只引用并接管现有核验过的 direct 实例，私聊 Agent 与 proactive 独立解析。指定 Agent 的 `bash` 省略为 false，省略私聊 Agent 则使用内置本人默认值。群默认 false，单群启用须独立 Agent 绑定；`owner_request` 不允许用于群或 proactive。Schema 19、状态输出、能力映射及进程切换规则见[运行时详细稿](agent-runtime-design-detail.md#会话-bash-与能力映射)。

Stream 回调的 inbox、消息修订、传输收据、任务和 Outbox 持久化到 SQLite，支持去重、重启、撤回和交付；不等待 DWS 群历史，不进入群 Haiku 批次。回复引用问题，附接入与执行耗时及实际模型；结构化日志只记录状态、数量、用量和受限错误，不复制正文、原始 stderr 或完整模型流；独立的[任务过程输出](task-terminal-detail.md)保存脱敏公开事件与诊断，并在读取时核对来源和任务版本。失败或未知交付关闭当前 Agent 进程，后续只恢复已受理轮次。

### 应用回调身份升级与历史审计（源码已实现）

DingTalk [官方 Stream 机器人教程](https://open-dingtalk.github.io/developerpedia/docs/explore/tutorials/stream/bot/go/send-streaming-card/) 在单聊发卡时将 `SenderStaffId` 作为接收者的 `userId`。因此新回调按 `(企业, user_id, senderStaffId原值)` 解析，可复用认证 DWS profile 已核验的同一精确标识；显示名称、`senderId` 与 `senderStaffId` 的字面相似均不构成身份链接依据。

解析版本由 `dingtalk_app/1` 升为 `dingtalk_app/2`，再升为 `dingtalk_app/3`：回调的 `chatbotUserId` 是机器人用户标识，独立的 `robotCode` 才与应用通道配置的机器人 Code 精确匹配。[钉钉 Stream 协议示例](https://open-dingtalk.github.io/developerpedia/docs/learn/stream/protocol/)同时展示两种标识；Go SDK v0.9.1 的 `BotCallbackDataModel` 没有声明 `robotCode` 必有。出现 Code 时必须精确匹配，缺失时由已核验应用 Client ID/Secret 建立的 Stream 连接、企业核验和已绑定会话路由限定入库。该回退不能证明同一应用下另一机器人的具体用户标识，真实回调需联调确认。无需数据库身份迁移。已有 inbox 解析版本、message 发送者、principal、alias 和 Source 证据保持原记录；旧消息重投也不改写其发送者。无事件 ID 的旧回调在新解析版本下可能多一条接收观察，但沿用原 message 与证据，不把旧 `staff_id` 升权为 `user_id`。不存在显式核验链接时，历史 `staff_id` 同值确认仍被拒绝；本人应在机器人私聊中发送新的有效确认口令。此变更不自动合并历史记录，也不自动修正旧的 Owner 配置。

接收会话的每次租约取得、续租、消息入库、拒收记录和释放使用独立内部请求 ID；调用方显式给出的会话请求 ID不会在多次事务中重复插入。官方 Go SDK v0.9.1 的自动重连在 `Close` 后仍用后台 context 重开连接，可能留下没有租约的回调订阅；应用 Stream 适配器关闭 SDK 自动重连，并每 5 分钟受控轮转连接。会话返回前会调用 `Close`，外层接收者返回、释放租约后才再次建连；SDK 不暴露读循环退出后的 join 信号，旧连接已关闭但极短时间内旧读 goroutine 可能仍在退出。SDK 也不暴露读循环退出信号，断线后的静默窗口最多可能持续到下一轮转；应用机器人没有历史 API，重投未确认帧及真实机器人消息送达仍需平台联调验收。

协议样例与去标识化夹具覆盖 `msgtype=richText` 及仅含 `text` 元素的 `content.richText`；[钉钉接收消息说明](https://open-dingtalk.github.io/developerpedia/docs/learn/bot/message/)与[Stream 常见问题](https://open-dingtalk.github.io/developerpedia/docs/learn/stream/faq/)也展示富文本中可能混有图片。解析版本 `dingtalk_app/4` 只将 1 至 128 个纯文字元素按顺序合并成文字消息，正文最多 64 KiB；空白、图片、链接、其他媒体、未知结构和超限内容明确拒收，不抽掉附件后猜测完整请求。拒收业务证据中的媒体下载能力字段递归脱敏；运行日志只保留错误分类，不复制富文本原文。

### 机器人混合富文本

`dingtalk_app/6` 在纯文字基础上接收已识别的图片段（`type=picture` 与下载引用）和带 `url` 的文字段，按原顺序在正文插入“内容未读取”或“目标未读取”标记，附件元信息仅记录种类与不可读说明，不记录下载 Code 或链接目标。正文标记随消息、Source 与任务进入 Agent 输入，避免把未读内容当成图片识别结果或链接正文。旧的 `/4` 限制保留为历史行为；`/5` 的机器人引用消息功能继续适用。

不支持的媒体类型、未知字段、空或错误类型引用、空白正文和超出 128 段／64 KiB 的内容仍拒收；原始回调先按已有规则递归脱敏。离线测试覆盖平台样本形态、消息接收确认、附件标记、顺序、凭证脱敏与无效结构拒收。图片下载、链接访问及混合消息的真实平台往返尚未实现或验证；旧拒收记录中的凭证已脱敏，不能自动回放为完整媒体消息。

### DWS 历史导入的时间续步（源码已实现）

已核验当前安装的 DWS v1.0.61、commit `50eb73a0`：[时间范围解析](https://github.com/DingTalk-Real-AI/dingtalk-workspace-cli/blob/50eb73a0/internal/shortcut/smart/message_time_range.go) 将无时区显示时间按固定 CST（UTC+08:00）读取；[分页实现](https://github.com/DingTalk-Real-AI/dingtalk-workspace-cli/blob/50eb73a0/internal/shortcut/smart/chat_messages.go) 将 epoch 毫秒 `nextCursor` 转为 `nextPage.time`，并将升序范围终点记为 `range_end`。宿主机时区不参与这些解释。适配器保留带时区时间/毫秒精度，对无时区消息时间使用这项明确的上游契约。

历史任务的 v2 `cursor` 绑定原始 conversation、start、end 和精确 resume_at。每步使用 `--order asc --page-all --page-limit 5 --max-items 100`；续步仅将 `--start` 调整为经过校验的 `nextPage.time`，固定 `--end` 不变，不使用当前 CLI 没有的 `--page-token`。方向必须为 `newer`，毫秒值与带时区时间必须一致，且断点严格前进并处于固定窗口内。消息 `createTime` 只有秒精度时，允许它落在精确续步边界所在的同一秒；消息本身通过 Intake 去重。同一显示秒内的毫秒游标仍可前进，重复或无效毫秒游标明确保留 partial。

仅在上游明确窗口结束、无 hasMore、无分页失败且无异常原因时报告 complete；`source_complete` 和经过 complete 确认的 `range_end` 都是正常结束。消息、断点、计数和 coverage 在同一事务提交。无法推进时保存已验证消息与未完成 coverage，任务转为 `failed/history_cursor_stalled`，日志保留同一安全错误码；权限拒绝为 `denied`，不会无休止自动重试。

旧 v1 曾把 CST 显示时间当作 UTC，可能在每次分页前进时跳过八小时，并留下错误的 `messages.sent_at`。显式 `data-source history retry IMPORT_ID` 遇到 v1 断点时，会审计升级为 v2，从该固定窗口最早旧 DWS 观察的校正时间之前一秒回读，最早不越过原始 start。不能仅回退最后八小时就宣称早期缺口已经补齐。已有正文去重；已核验空白前缀可避免重读，但若安全回读点触到原始 start，这一次修复必须从原窗口起点重新核验。之后 v2 的中断重试始终保留原断点，不重新选择 30 天窗口。

旧时间只在同一 DWS 历史消息被新适配器再次读取、同会话/消息 ID/发送者/正文均一致、旧入站观察能证明已知八小时偏差时校正，并记录 `message.history_time.correct` 操作的 before、after 与新核验 inbox event。应用消息、离线导入、无旧 DWS 观察或正文变化的记录不自动改时间。原 Source 快照和正文版本是历史证据，不重写；新事实通过审计说明。没有再次读取到的旧消息不能被宣称已经修复。

时间校正位于统一 Intake，适用于导入任务和普通的已授权历史读取，不限当前值守群。某些群的导入完成不会修复未重读会话，也不会把其他旧会话自动加入采集范围。旧版同样接受已带时区的时间，因此仅凭 `dws/1` 来源或未来时间不能批量减八小时；未再次核验的记录保留原证据。后续明确授权读取这些会话时，匹配的旧消息可逐条校正并审计，无须重跑已完成的其他群导入。

测试覆盖 420 条多步导入、同秒毫秒续步、CST 与宿主时区无关、range_end、边界重复、中断后重开 SQLite、旧 v1 显式恢复、逐条时间校正的身份/正文边界、停滞及权限拒绝。

## 群助手首次挂载约束

操作步骤见[运行指南](../guides/runtime-user-guide.md#群助手首次自动挂载)。

仅在本地配置过 DWS 群路由，不代表机器人实际在群内。自动挂载使用完整发现收据，绑定来源和通道版本、机器人配置与发现时间。收据过期或版本变化时，需要刷新数据源发现后重新计划。有效期为两次对账周期，最少 10 分钟、最多 1 小时；未核验的应用机器人不能执行挂载。

当前成员检查依据 DWS 返回的**唯一机器人名称匹配**，并固定到配置中选定的机器人；这不证明 `robotCode` 与 `openBotId` 等价。若同群出现多个同名机器人，发现失败并阻止新自动挂载，需先消除歧义或补充明确身份映射。发现收据仅保存在 SQLite，不包含聊天正文。

所有创建操作及计划版本进入已有操作审计。预览不会写库；预览后发现记录、通道或群权限发生变化，旧摘要不能继续应用。

## 环境验收边界

真实私聊往返、账号标识、安装记录与平台回执属于部署者的私有验收证据，不提交到公开仓库。离线测试不能代替真实模型或平台验收。
