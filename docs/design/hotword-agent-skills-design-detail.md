# 语音热词与 Agent 技能配置：实现详细稿

> **Superseded historical design (2026-09-23).** The database memory pipeline, publication model, hotword ingestion and old AgentHome knowledge loading described here are removed by the [Agent Workspace design](agent-workspace-design.md). Use the [current runtime guide](../guides/runtime-user-guide.md) and [workspace details](agent-workspace-design-detail.md) for current behavior. The remainder records the previous design and its original validation; its commands and delivery claims do not apply to the new version.

范围和用户体验以[主文档](hotword-agent-skills-design.md)为准。

## 热词数据与写入

`Memory` 增加可选 `hotword`：`canonical`、`aliases`、`meaning`。它继续使用现有 `memories.document` JSON、证据、版本、有效期和退役状态，因此不增加 SQLite 迁移。候选应用仍走现有事务与审计。

本人私聊的 Claude 会话得到受控 `memgov-hotword` 包装器。包装器从当前会话的 `.memgov-turn.json` 取得 attempt ID，并调用 `runtime task capture-hotword`。服务端同时验证：

1. task 和 attempt 都在运行且版本一致；
2. 来源是 direct 模式、已验证本人、可用的当前消息；
3. 标准名和每个别名都真实出现在原始消息中；
4. Source、Fragment 和摘要值存在。

通过后自动创建候选、记录接受 Review 并 Apply；同一工作区中相同标准名更新现有记忆并合并别名。仅相似、推测或未出现在本人原文中的映射返回 `denied`。

## 每轮上下文与隔离

`HotwordContext` 只扫描有效的 active Memory，先当前工作区后 global，按更新时间排序，并按 Unicode 字符预算截断。本人或主动 Agent 仍须具有 `memory_read`。`conversation_published` Agent 对每条热词调用现有 `CheckDisclosure`，所以发布绑定到精确通道、会话和记忆版本；另一群、未发布版本或更新后的版本都不可见。

热词上下文作为独立的 `hotword_context` 交给执行器，系统提示明确其用途只限拼写理解。任务正文和保存的消息不被重写。热词上下文摘要值进入本人持续会话的策略指纹，变化会在下一轮新建 Claude 会话。

## Claude 技能解析与加载

`RuntimeSkillPolicy` 包含 `inherit`、配置路径和已解析技能。配置计划为每个技能记录名称、真实路径、内容摘要值和从 `SKILL.md` 提取的简短说明。

- `inherit: executor`：Claude 适配器枚举 `~/.claude/skills`；本人私聊绑定的 Agent 省略时采用此默认值。全局目录中的 `memgov-memory` 不参与继承，仍按 `memory_read` 权限安装运行时管理版本。
- 继承技能属于可选的环境能力。枚举期间单个入口链接失效、`SKILL.md` 不可读或目录摘要校验失败时，只跳过该入口；整个 `~/.claude/skills` 根目录不可读时返回 `unavailable`。这防止技能安装器的原子替换或临时文件状态使所有本人/主动 Agent 任务失败。显式技能不使用该降级路径。
- `inherit: none`：不读取全局目录；其他 Agent 的默认值。
- `paths`：在计划阶段要求目录和 `SKILL.md` 存在，解析顶层符号链接，拒绝技能内部符号链接和非普通文件。
- 显式技能通过工作目录 `.claude/skills/<name>` 链接给 Claude；`--setting-sources` 仅在继承执行器时包含 `user`。
- `--tools` 只启用 Claude 的 `Skill` 入口，`--allowedTools` 只加入 `Skill(<name>)`。原 Agent 的 Read、Write、Bash 和外部动作边界保持不变。

配置计划把技能新增、删除、继承方式变化和内容摘要变化标记为权限/边界变化。执行前重新发现当前可用的继承技能，其变化会改变会话策略指纹并在下一轮重建会话；被跳过的继承技能不会加入允许列表。显式路径再次计算摘要值，路径缺失或内容变化返回 `conflict`，避免持续会话静默使用不同技能。

当前实现未转换不同执行器的技能格式。技能引用的脚本和文件保持原目录关系；依赖由原生执行器加载，执行器报告的缺失依赖作为该轮失败返回。

## 代码位置与验证

- 数据和治理：`internal/core/types.go`、`internal/core/hotword.go`
- 配置与计划：`internal/cli/config.go`、`internal/cli/dual_config.go`、`internal/cli/dual_plan.go`、`internal/cli/agent_skills.go`
- Claude 运行时：`internal/runtime/skills.go`、`internal/runtime/model.go`、`internal/runtime/direct_agent.go`
- 测试：`internal/core/hotword_test.go`、`internal/cli/agent_skills_test.go`、`internal/runtime/skills_test.go`

源码验收运行 `go test ./...`。真实验收需要安装本次构建、应用配置、重启 direct runtime，再从本人钉钉私聊完成一次纠正和一次新会话询问；这一步不能由离线测试代替。
