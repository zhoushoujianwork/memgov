# 记忆列表、统计、时间排序与命令简写

> **Superseded historical design (2026-09-23).** The database memory pipeline, publication model, hotword ingestion and old AgentHome knowledge loading described here are removed by the [Agent Workspace design](agent-workspace-design.md). Use the [current runtime guide](../guides/runtime-user-guide.md) and [workspace details](agent-workspace-design-detail.md) for current behavior. The remainder records the previous design and its original validation; its commands and delivery claims do not apply to the new version.

`memory → m` 简写已实现；准确计数、时间字段展示和可选排序仍为设计稿。本人私聊使用场景见[需求主文档](owner-private-chat-requirements.md)。

## 现在就能用

```bash
memgov m list --limit 20
```

当前已按**更新时间倒序**排列，最近修改的排在前面；默认返回 50 条。不过结果没有展示创建、更新时间，也没有排序选项。

## 命令简写

项目采用 Cobra，`memory` 已支持别名 **`m`**，完整命令继续保留。以下简写可用：

```bash
memgov m list       # 等价于 memgov memory list
memgov m show ID    # 等价于 memgov memory show ID
memgov m history ID # 等价于 memgov memory history ID
```

简写适用于整个 memory 命令组，参数和行为完全一致。首期明确支持 `m`，其他命令组的别名按需增加。

## 本次补齐

- 默认继续按**更新时间倒序**，新增和修订后的记忆都会排到前面。
- 可以改为按**创建时间**排序，只看最近沉淀了哪些新记忆。
- 支持倒序（最新在前）和正序（最早在前）。时间相同时顺序保持稳定。
- 列表返回创建、更新时间；文字格式显示简洁表格，完整内容仍可用 `memory show ID` 查看。
- 增加准确的记忆条数统计，使用与列表一致的工作区、分类和状态过滤；条数不受列表 `--limit` 影响。

以下为**待实现用法**：

```bash
# 默认：最近更新的记忆，展示为表格
memgov m list --format text

# 最近创建的 20 条记忆
memgov m list --sort created_at --order desc --limit 20

# 按更新时间，从旧到新
memgov m list --sort updated_at --order asc

# 统计当前可见工作区中的 active 记忆
memgov m count
```

创建时间指首次成为正式记忆的时间；更新时间指最近一次正式修订或状态变更的时间。它们都不是材料里记载的事件发生时间。

保留现有工作区、分类、状态和条数过滤。现行 CLI 默认按当前工作区加全局、`active` 状态读取；其他状态用 `--status` 明确选择，全部生命周期用 `--status all`。首期继续按 `--limit` 取前若干条，翻页后续按需补充。

验收：简写与完整命令结果一致；新增、修订、退役后，列表顺序与显示时间一致；能切换创建时间及正倒序；同时间不乱序；计数、工作区和状态过滤保持准确。

实现约定见[同名详细稿](memory-list-design-detail.md)。
