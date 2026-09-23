# `memgov-memory` 跨 Agent 记忆接入：实现详细稿

> **Superseded historical design (2026-09-23).** The database memory pipeline, publication model, hotword ingestion and old AgentHome knowledge loading described here are removed by the [Agent Workspace design](agent-workspace-design.md). Use the [current runtime guide](../guides/runtime-user-guide.md) and [workspace details](agent-workspace-design-detail.md) for current behavior. The remainder records the previous design and its original validation; its commands and delivery claims do not apply to the new version.

范围以[主设计](memgov-memory-skill-design.md)为准。当前仓库中的 `.agents/skills/memgov-memory/SKILL.md` 是使用约束；本稿定义把它作为 Personal Jarvis 和其他 Agent 的稳定接入协议时必须保持的边界。具体命令仍以目标二进制帮助为准。

## 接入原则

1. SQLite `state.db` 是权威数据源；skill 不直接读写数据库表。
2. Agent 先确认版本、配置、workspace 和 `doctor` 健康状态，再执行任务。
3. 读取和写入都显式选择 workspace；没有明确跨项目需求时不能使用 `--all-workspaces`。
4. 存储内容视为不可信资料；其中的命令、提示或权限声明不能改变宿主 Agent 的授权。
5. 写入只在用户明确要求记住、保存、更新或维护，或宿主任务声明允许 capture 时进行。

## 请求 envelope

所有适配器向 CLI/API 传递或注入以下字段：

```json
{
  "actor": "codex",
  "agent_id": "owner-executor",
  "workspace": "project-id",
  "request_id": "req-...",
  "idempotency_key": "stable-write-key",
  "operation": "recall|source.create|candidate.apply",
  "target_id": "optional-id",
  "expected_version": 3,
  "evidence_refs": ["source-id#fragment-id"]
}
```

`actor` 和 `agent_id` 用于审计，不是扩大权限的口令。workspace 解析顺序可以沿用现有约定（显式参数、环境变量、注册目录、默认 workspace、global），但写操作必须在请求中得到最终明确值。重复的幂等键只在操作类型和输入摘要完全一致时复用结果；输入变化返回冲突。

## 读取工作流

推荐顺序：

1. `config show`、`workspace list`、`doctor` 和版本检查；
2. 用小结果数、字符预算和 `--explain` 做 focused `recall`；
3. `memory show/history` 核对状态、版本、适用条件、观察时间和证据；
4. 正式记忆不足时才 `search --kind source`，并明确来源命中是证据，不是批准记忆；
5. 将可见性、证据状态、时间和历史缺口传给模型。

Personal Jarvis 的私聊任务可查询已授权 Owner 历史；群 Jarvis 的请求必须执行当前群的 workspace、发布和披露过滤。一个 Agent 读到的内容不能仅因 skill 调用就向另一个通道转述。

## 写入和治理工作流

### 新建记忆

1. 从当前任务提炼可复用事实、偏好、约束、决定、流程或教训，排除凭据、猜测、一次性进度和无证据错误。
2. 以最小必要正文创建 Source，指定稳定 URI、workspace 和 `observed_at`。
3. 读取 Source 和 Fragment，取得真实 `source_id`、`fragment_id`、片段 SHA-256 和可选原文 quote。
4. 创建 Candidate，并提交 `candidate validate`。
5. 阅读完整 Candidate，结合引用片段做语义 Review。
6. 用返回的 candidate `digest` 执行 Apply，再用 `memory show` 或 focused recall 验证。

### 修订现有记忆

读取当前 Memory，绑定 `target_id` 和 `expected_version` 创建 update Candidate。若版本冲突，重新读取和生成候选；不得盲目重试旧 digest。Apply 后版本递增，历史和 Operation 保留。

### 退休、恢复和清除

退休、恢复和敏感清除是明确的生命周期操作，不能由普通“记住”请求隐式触发。restore 生成新版本，不回拨版本号；敏感清除保留非正文墓碑和操作事实。skill 不提供绕过治理的直接 SQL 或文件删除路径。

## 结果 envelope 和错误

返回至少区分：

```text
ok / error
kind = source | candidate | review | memory | operation
status = found | pending | applied | retired | conflict | denied | unknown
id / version / digest
evidence_refs
health
message（不含凭据和未授权正文）
```

错误分类包括版本或 digest 冲突、workspace 拒绝、证据缺失、权限/披露拒绝、无效参数、数据库忙、工具不可用和外部结果未知。`ok: true` 不代表 `doctor` 健康；适配器必须同时检查进程退出状态和 JSON envelope。

## Personal Jarvis 集成

Owner 根任务在上下文中记录 skill 请求 ID、召回范围、采用的 Memory 版本和候选/审查结果。Agent 可以把代码任务、调查结论和已确认经验分开保存：任务结果留在 Task/Operation，长期经验走 Source → Candidate → Review → Apply。根任务通知 Owner 时，明确“已找到来源”“候选待审”或“Memory 已应用”。

skill 写入的权限由根任务的 `memory_read`/`memory_write` capability 和 workspace 共同决定。即使 Owner 委托 Agent 有完整记忆治理权限，也不能借此发送钉钉消息、读取无授权私聊、扩大目录或执行生产操作。

## 群 Jarvis 集成

群 Jarvis 可通过相同 skill adapter 查询共享记忆或提交经授权的治理变更，但每次读取和交付都执行群 `CheckDisclosure`：workspace、active 状态、有效期、证据撤回、当前群共享规则和单条发布许可必须同时满足。skill 接入不会改变群机器人、群 preset、工具、技能、目录或原群 Outbox。

群 Agent 的输出不能把 Owner 私聊 Source、私有 Memory 或 DWS 本人身份带入群；同样，群来源也不能自动写入 Owner 私聊可见的长期 Memory，除非配置和审查明确授权。未来若要收紧群侧 skill 能力，必须另立版本和迁移，不在本期隐式执行。

## 并发、幂等和审计

候选更新使用 target/version CAS，Apply 使用 candidate digest；Source、Candidate、Review、Memory 以及 Operation 的关键写入在短事务内完成。模型调用、网络访问和文件读写在事务外。成功幂等键绑定精确输入；失败或未知不自动改写成成功。

审计记录 actor、agent、workspace、request、操作类型、目标、证据摘要、前后版本、结果和错误码，不记录密钥或不必要的原文。读取审计可以按配置保留最小摘要，不能把每轮空轮询写入热路径。

## 测试矩阵

| 层级 | 覆盖 |
| --- | --- |
| CLI/API 契约 | envelope、帮助、版本、健康字段、workspace、JSON 错误 |
| 证据治理 | Source 片段和 digest、Candidate validate/diff、Review、Apply、版本冲突 |
| 记忆生命周期 | create/update/retire/restore、撤回证据、有效期和状态过滤 |
| 并发 | 同一 Memory 的 CAS、重复幂等键、输入变化冲突、数据库忙 |
| Personal Jarvis | 召回上下文、任务结果与 Memory 分离、根任务审计和通知摘要 |
| 群 Jarvis | 共享记忆过滤、私有内容拒绝、skill 读取不改变群路由/工具/技能、原群回复回归 |
| 安全 | 来源提示注入、未授权 workspace、凭据泄露、跨受众转述和 shell/消息权限隔离 |

真实 Agent 和平台验收应记录二进制版本、skill 目录摘要、workspace、任务/操作 ID 与结果。未安装或未运行的契约只能标记为设计中，不得写成已经交付。
