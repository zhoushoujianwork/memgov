# 群共享记忆

> **Superseded historical design (2026-09-23).** The database memory pipeline, publication model, hotword ingestion and old AgentHome knowledge loading described here are removed by the [Agent Workspace design](agent-workspace-design.md). Use the [current runtime guide](../guides/runtime-user-guide.md) and [workspace details](agent-workspace-design-detail.md) for current behavior. The remainder records the previous design and its original validation; its commands and delivery claims do not apply to the new version.

状态：实现已完成；本机安装与实测状态以交付说明为准。对应[最佳落地场景](../architecture/best-practice-scenarios.md)中的“群内 @：团队复用专用 Agent”。实现细节见[详细稿](group-memory-sharing-detail.md)。

群 Agent 可以按需查询共享记忆，回答“最新的记忆是什么”、搜索相关经验、读取记忆正文。私聊和群聊使用同一个记忆库，读取范围由配置决定。

在实际运行服务使用的配置文件中设置：

```yaml
applications:
  group_mention:
    shared_memory_workspaces: [global]
    excluded_memory_categories: [preference]
```

这让该群应用所有已接入及以后接入的群查询 `global` 中的有效记忆，覆盖已有、新增和更新的条目。`category: preference` 的个人偏好始终不能在这些群中查询，即使曾单独发布给群；私聊仍可使用。此过滤按记忆类型生效，不自动识别误标为其他类型的个人内容。

其他工作区的记忆仍按原来的逐会话、逐版本发布规则开放。原始私聊、证据来源正文和外部操作权限不随共享记忆开放。

将 `shared_memory_workspaces` 改为 `[]` 可关闭工作区共享；单条发布许可仍按原规则生效。修改后先运行 `config plan`，再按计划执行 `config apply-runtime`。统一服务会随应用配置更新；使用旧二进制时需要安装本功能版本并重启。

查询空结果只表示当前群没有可见匹配；“最新”按记忆的更新时间排序，因此群里的最新非偏好记忆可能与私聊中的最新记忆不同。
