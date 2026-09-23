# 群共享记忆实现详细稿

> **Superseded historical design (2026-09-23).** The database memory pipeline, publication model, hotword ingestion and old AgentHome knowledge loading described here are removed by the [Agent Workspace design](agent-workspace-design.md). Use the [current runtime guide](../guides/runtime-user-guide.md) and [workspace details](agent-workspace-design-detail.md) for current behavior. The remainder records the previous design and its original validation; its commands and delivery claims do not apply to the new version.

范围以[主文档](group-memory-sharing.md)为准。

## 配置与权限

`applications.group_mention.shared_memory_workspaces` 支持 `[global]` 或 `[]`，默认不开启。`excluded_memory_categories` 支持 `[preference]` 或 `[]`，默认不增加过滤。共享工作区或移除过滤属于权限扩张；新增过滤属于权限收缩。配置计划同时标记披露边界变化，应用时沿用既有授权参数与审计原因。

SQLite 的已应用配置是唯一运行时政策来源；仅编辑 YAML 不开放记忆。读请求必须对应配置的 DingTalk 应用通道、企业和有效的 assistant 群路由。所有任务的 Agent 策略摘要包括共享与排除配置，旧策略任务不能继续交付。

## 查询

`audience list <channel> --conversation <id> --limit 1` 按 `updated_at DESC,id DESC` 返回最新可见记忆摘要；条数范围 1..20。

`audience recall <channel> <query> --conversation <id>` 在有效记忆中搜索，先校验可见性，再应用 10 条结果与字符预算。普通查询不返回被拒绝记忆的标题、正文或证据。

`audience show <channel> <memory-id> --conversation <id>` 返回可见记忆的类型、工作区、版本、时间和正文，不返回原始来源、证据引用或私聊记录。摘要、正文有长度上限。

查询在同一只读事务中执行。`CheckDisclosure` 对所有读取及交付复核共同执行：工作区范围、状态、有效期、证据撤回、当前工作区共享或当前版本的单会话发布许可。群类型排除优先于共享和单条发布。

## 执行工具

群不再按任务标题预装载记忆。执行器安装 `.claude/tools/memgov-group-memory`，将 home、通道、会话固定；只支持 `latest [count]`、`recall <query>` 和 `show <memory-id>`。参数按字面值传入 CLI，拒绝写操作、额外参数和目标覆盖。

一般 Bash 关闭时，仅该绝对工具路径获得 Bash 允许规则，不继承所有者的 CLI、MCP 或私聊技能。群 Agent 只在需要时调用工具；目录快照模式仍保留此独立只读查询。

## 验证范围

自动测试覆盖跨群共享、新增记忆、个人偏好列表与正文拒绝、旧单条发布无法覆盖过滤、私聊来源通道和 direct 路由隔离、撤回证据、关闭共享后即时拒绝，以及先过滤再应用搜索结果上限。工具测试覆盖固定目标、字面查询参数、写入和目标覆盖拒绝、无一般 Bash 授权。

真实平台回复与业务验收需另行检查实际收消息、查询调用和交付记录，不能由离线测试替代。
