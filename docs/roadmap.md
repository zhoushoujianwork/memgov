# 实施路线

状态：以[Owner Assistant 主设计](design/owner-assistant-design.md)和[最佳落地场景](architecture/best-practice-scenarios.md)为实施顺序；这是工作安排，不是交付承诺。已有能力与可复现验证见[实现状态](implementation-status.md)。群挂载 Jarvis 是并行接入通道，本路线不以削减其现有能力换取 Owner Assistant 能力。

## 主线：完成一条可核验的工作闭环

当前优先把 Owner 私聊、DWS proactive、任务委派、结果通知和记忆治理串成一条可核验闭环，同时保持群 Jarvis 的原有接入、能力和回复通道。既有精细机制可以保留兼容读取，但不再作为高频调度热路径。范围见[MVP 核心取舍](architecture/best-practice-scenarios.md#mvp-核心取舍业务先顺畅运行)。

实施 SQLite 热写简化后，应在部署者自有的隔离环境中验证延迟、真实业务和经验复用。公开仓库只保留可复现测试与脱敏结论。

| 顺序 | 下一步与依赖 | 完成条件 |
| --- | --- | --- |
| 1 | **已实施**：简化 SQLite 热写路径：调度槽位和普通活动状态留在内存，移除高频续租、phase、空轮询及逐次内部审计写入；任务只用短条件更新保存关键业务状态 | 满足[MVP SQLite 验收](architecture/best-practice-scenarios-detail.md#mvp-验收)：空闲零周期写，8/4 后台负载不阻塞交互，数据库等待不因一次续租失败取消 Agent，重启不重复外部副作用 |
| 2 | **进行中**：统一 `~/.memgov/config.yaml`，清理重复 runtime，建立 Owner 根任务/子 Agent 关系和环境上下文快照 | `config plan` 明确 `ready:true`、Owner 边界、无冲突、无重复 runtime、无未授权扩权；Owner 私聊和 DWS 发现能关联到同一任务图；旧 `config.dual.yaml` 只作为迁移备份 |
| 3 | **进行中**：[Owner Assistant 通知与恢复](design/owner-assistant-design.md)：实质结果、阻塞和确认请求回到 Owner，支持暂停、继续、取消、紧急停止和幂等投递；历史 `record_only` 保持兼容 | 离线验收覆盖直接回答、复杂委派、多 Agent 汇总、重启继续、通知失败恢复和危险操作确认；真实模型与平台回执仍需部署者验收 |
| 4 | **并行保持**：[群挂载 Jarvis](design/dingtalk-integration-design.md)继续使用原有机器人、群路由、Agent、技能、工具、记忆范围和原群回复，不继承 Owner 私聊权限 | 群内 `@` 回答、确认、共享记忆和失败恢复回到原群；Owner 在群里不会升权；任何后续限制另行设计、迁移和验收 |
| 5 | **下一步**：让其他 Agent 通过 `memgov-memory` skill 完成 Source/Candidate/Review/Memory 全治理，并贯通一条真实 Owner 事项从发现到经验复用 | 记录 Agent/workspace/actor/request/idempotency/evidence/version；完成 Owner 验收和受众隔离证据，不能用工具数量替代业务结果 |

第 5 项依赖目标企业权限、明确部署版本和真实业务样本；不在本次文档整理中预设未经配置的外部连接器或新增执行授权。

## 独立待办与后续范围

- **系统提示真实模型安全验收：** 集中维护和输入分层已在源码实现；继续按[验证边界](design/agent-runtime-design-detail.md#统一系统提示与安全验证)，在隔离环境验证模型对注入、破坏、外传、记忆污染的识别及合法请求误拒情况，补齐多轮样例与部署版本证据。完整 Bash 的 OS 隔离仍是独立缺口，不能以 prompt 测试代替。

- **本人私聊记忆盘点与诊断：** 按[私聊需求](design/owner-private-chat-requirements.md)和[列表设计](design/memory-list-design.md)补齐同范围计数、时间展示、排序及受控工具错误说明；已有 `m` 简写不重复开发。完成条件是私聊能正确解释范围、最新创建与最新更新，失败能说明实际原因。
- **已有功能的安装与现场验收：** 群共享记忆、热词与技能、私聊双方补漏、七天保留、统一服务、管理台、任务继续和实时终端分别跟踪缺口。源码已实现不代表当前本机进程已更新；Schema 26 升级与任务继续按[专项说明](design/task-continuation.md)执行。
- **管理台后续控制与 Desktop：** 已有服务重启、失败任务继续、macOS 当前用户 launchd 托管及受支持旧 Schema 的备份后自动迁移不再列作未来目标。Linux 系统托管、其他控制和桌面打包仍另行安排，不能阻塞主线业务闭环。托管能力与验证边界见[统一服务](design/unified-service-design.md)。
- **更多来源和多方协作：** 文档、会议及多 Agent 协作按真实需要单独设计；已授权后台沟通按本轮工具边界验收；不作为第一条闭环的前置条件。

## 推进时同步什么

每项工作都更新对应主设计、必要的详细稿和[实现状态](implementation-status.md)，记录场景、验证结果与剩余缺口。实际验证后再关闭待办；优先级或范围改变时写明原因并同步本页，维护方式见[README](README.md#维护标准)。

群原生 @ 答复与仅 DWS 所有者可操作的审批卡片已有源码及离线验证；真实模板、客户端提醒和同意/拒绝回路由部署者在自有环境验收。审批留在原群，不转私聊。参见[接入设计](design/dingtalk-integration-design.md)。
