# Personal Jarvis 托管服务设计

状态：产品主线设计，作为后续实现和验收的范围依据；尚不代表所有行为已在安装版本或真实平台交付。实现细节见[详细稿](owner-assistant-design-detail.md)。

Current implementation boundary: root/child orchestration and automatic proactive result notifications below are product targets. Current proactive completion remains `record_only`. Knowledge follows the [Agent Workspace design](agent-workspace-design.md), replacing all database memory governance stages.
## 目标体验

memgov Personal Jarvis 是用户自己的日常工作助手，运行在用户控制的本地环境中。用户可以从任意已接入的沟通平台提问、交办、跟进和确认；本地任务也可以主动开始调查。简单问题直接回答，复杂事项拆给 Agent，并把过程、证据、产物和最终结论收回来。

助手需要同时理解：当前消息、私聊历史、DWS 观察、本机和项目环境、任务进度、受治理记忆以及已配置技能。它可以在 Owner 委托范围内读写项目、运行测试、维护记忆和调用工具；危险、不可逆、对外或跨受众动作先向 Owner 请求确认。

## 使用方式

| 入口 | 作用 |
| --- | --- |
| 任意平台适配器 | 日常交办、追问、查看进度、确认或取消动作 |
| 本地主动值守 | 发现属于用户的工作，按授权自主调查并在有结果时回原入口通知 |
| CLI/管理台 | 查看任务、配置、运行状态、证据和审计 |
| `memgov-workspace` skill | 让其他 Agent 复用同一套记忆和证据治理 |

群里挂载的另一个 Jarvis 是并行接入通道。本设计不改变它的机器人、群路由、preset、工具、技能、记忆范围或原群回复方式；它继续按自身配置工作。它不会自动获得 Owner 私聊历史或本人身份，任何未来限制都另行设计。

## 本期范围

- 统一平台消息和本地主动任务的根任务模型，支持子 Agent、结果汇总、继续、取消和通知；
- 固定上下文装配顺序，提供本机/仓库/服务环境快照，并显式标记历史缺口；
- 以 `~/.memgov/config.yaml` 作为唯一活动配置，迁移旧双模式声明；
- 让结果、阻塞和确认请求通过原平台适配器幂等交付；
- 通过 `memgov-workspace` skill 让其他 Agent 查询、读取和维护 Workspace files；
- 提供至少一个平台适配器，并验证群 Jarvis 的现有能力和接入路径不受影响。

## 实施顺序

先稳定 Workspace 和平台适配器，再建立根任务/子 Agent 和上下文快照；随后接通通知、确认、恢复和 skill 契约，最后进行跨平台交互、本地主动值守、记忆写入和群 Jarvis 回归验收。

范围、状态和验收标准以本文为准，字段、状态机、错误和测试矩阵见[详细稿](owner-assistant-design-detail.md)。
