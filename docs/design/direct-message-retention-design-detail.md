# 私聊增量采集与七天会话保留：实现详细稿

用户行为与配置入口见[主文档](direct-message-retention-design.md)。

## 数据与状态

Schema 20 在 `data_sources` 保存 `direct_enabled`、`direct_enabled_at`、`backfill_after_enable`、`retention_days`、最近私聊收流和清理结果。`direct_conversation_contacts` 以通道和真实会话 ID 为主键，稳定联系人标识用于精确过滤，显示名只作候选搜索。

私聊路由使用 `conversation_type=direct`、`mode=collect`、`audience_policy=local_private` 和 `send_policy=draft_only`。群发现只更新群路由，不能吸收本人交付路由；私聊发现也不能改变群授权。

开启边界按配置真正应用时记录。事件早于边界或早于保留窗口会被拒绝，停用后不会收流。再次从关闭切到开启会覆盖启用时间，因此补漏不会跨过停用区间。

## DWS 采集

适配器并行启动 `dws event +listen-im --kind all-group` 与 `--kind all-direct`，两侧都出现平台 `[event] ready` 后才报告联合就绪。回调串行进入同一个租约围栏和 SQLite 事务。

新私聊通过限定起止时间的 `dws chat +search-msg --conversation-type single` 发现。机器人专用结果用 `--only-robot` 独立获取并从同事会话集合排除。发现结果不以显示名建立身份。

实时个人事件会过滤本人发送的回环消息，所以补漏对每个真实私聊调用 `dws chat +chat-messages --conversation-id`。首次窗口从 `max(direct_enabled_at, now-retention_days)` 开始；后续从覆盖水位重叠读取。分页受限时保存固定窗口游标，下一轮续读；完整读取同一窗口后将旧缺口标记已解决。

私聊历史通过 DWS 的稳定 userId 发送者过滤解析本人 openDingTalkId，并按稳定 ID 标记本人发送的一侧；相同显示名不能改变发言身份。身份解析缺失或不一致时，该补漏窗口失败并保留缺口。补漏与实时写入使用同一联系人更新和启用边界校验。

## 查询与处理范围

`message query` 只返回 `availability=available` 且发送时间仍在保留窗口内的消息。`--type` 连接受控路由类型，联系人筛选连接稳定身份映射。响应公开 `direct_enabled_at`、`available_since`、`retention_days`、逐会话水位和缺口摘要。

所有者主动运行实例使用 `ReadOwnerSourceProcessingRoutes`，在原有正向群发现证据之外加入已启用且有直接会话观测的私聊路由。群 Agent 仍只使用 `ReadSourcePositiveProcessingRoutes`，因此不能读取或披露私聊。

分类输入包含同会话最近事项的稳定键、状态和结论；私聊事项键按真实路由隔离。`complete` 将完成证据关联到原事项并记录结果，不自动私聊通知。完整 Agent 可以按 owner 预设委托自主沟通，具体身份和授权见[接入设计](dingtalk-integration-design.md)。本人发送记录以精确平台消息 ID 识别后续回流，不跳过真人 owner 的新消息。

Schema 22 的 `runtime_message_actions` 同样遵循七天原文保留：引用或所属任务来源到期后清理内容、发送理由、原始回执与详情，保留动作身份、目标、状态、输入摘要、证据版本和平台任务/会话/消息 ID，用于审计、幂等及防循环。读取时先按来源有效性隐藏正文；内部只读回执查询仅使用这些非正文标识，即使来源失效也能核对已发送消息。

## 清理事务

启动后立即清理，之后每小时检查；服务每个事务最多处理 50 条，尚有积压时分批继续，失败可重试。候选消息限定为当前数据源实际拥有的路由。

`config plan` 的 `retention_previews` 显示现有可用原文中将到期的消息数量。每批清理限定消息及其运行时原文副本，避免长事务阻塞消息处理；ready 日志来自实际 DWS 平台标记。

清理会：

- 清空所有消息修订正文和快照、附件定位、事件载荷；
- 将消息和 Source 标记为 `expired`，清空 Source/fragment 内容并移出全文索引；
- 清空可能复制原话的运行批次和原始执行输出、关联任务 instructions/result、待执行操作载荷及操作原始结果；事项处理摘要保留结论并移除已知原文、证据引文及 JSON 转义副本，保留标题、状态和关联；
- 投递草稿和幂等响应缓存同步移除过期原文；缓存仍保留去重键，不重新执行已完成请求；
- 保留 Source、location、digest、消息 ID、去重、覆盖和审计记录。

Workspace knowledge has its own file/history lifecycle and is not automatically deleted when a raw source expires. Notes should retain dated conclusions and source references rather than raw chat copies. Expired evidence requires authorized platform rechecking. SQLite uses `secure_delete`; this rule does not erase workspace files, old archives, user backups or exports.

Message details, internal Source fragments and runtime context exclude expired raw text before physical cleanup. Source identifiers and non-body location metadata remain for traceability; legacy memory/candidate quote scans are removed. Accepted Owner application-bot conversation turns keep their existing session/delivery recovery rules, independently of DWS collection watermarks. Workspace notes are a separate persistence layer and never expand evidence availability or disclosure authority.

## 失败语义与测试

- 私聊发现或机器人分类失败：记录 `direct_*` 错误，不停止已授权群采集。
- 群发现失败：按既有证据策略继续或降级；已经明确启用的私聊消费者独立运行。
- 清理失败：事务回滚，保存错误码，下次小时批次重试。
- 测试覆盖启用边界、真实会话建路由、双方补漏、机器人排除、双消费者、游标续读、联系人筛选、群/私聊隔离、七天到期和来源定位保留。
