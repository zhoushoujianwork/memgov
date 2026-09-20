# 最佳落地场景与对齐标准

状态：Owner Assistant 产品主线与后续业务验收基准。目标行为不等同于已安装或已在真实平台验收的能力；逐项证据见[实现状态](../implementation-status.md)和[详细稿](best-practice-scenarios-detail.md)。

## 核心目标

让 DWS Owner 可以把日常工作交给一个持续在线的托管助手：消息进入后，它能找到背景、判断是否需要拆分 Agent、在委托范围内执行和验证，并把结果、阻塞或需要确认的事项交回 Owner。经过复核的经验沉淀为 memgov Memory，供下一次工作和其他获准 Agent 使用。

```text
Owner 私聊 / DWS 发现
    → 事项识别与背景关联
    → Owner Assistant 直接处理或派发 Agent
    → 工具执行与验证
    → Owner 结果/阻塞/确认
    → Source → Candidate → Review → Memory
```

价值以减少遗漏、重复补背景和重复处理衡量，不以接入工具或启动 Agent 数量衡量。

## 三个优先场景

| 场景 | 用户期待的体验 | 完成标准 |
| --- | --- | --- |
| Owner Assistant：替 Owner 接住工作 | Owner 私聊一句话即可提问、交办、跟进；后台发现的事项也能继续调查，复杂事项由 Agent 分工完成 | 及时回执；上下文和权限可解释；结果、阻塞、确认和证据可追溯；不重复执行未知外部动作 |
| DWS 主动值守：主动推进属于 Owner 的事项 | 不必把所有背景先整理齐，助手可在授权范围内调查和执行；有实质结果时私聊通知 Owner | 观察与执行任务隔离；历史缺口显式说明；无变化不打扰；`record_only` 历史任务保持兼容 |
| 群挂载 Jarvis：团队继续使用群助手 | 群成员在原群 @ 已挂载的 Jarvis，继续使用该群原有的人设、资料、工具、技能和记忆 | Owner Assistant 重构不改变群 Jarvis 的接入通道、能力或回复路由；群消息不自动取得 Owner 私聊历史和本人身份；未来限制另行决定 |

事项闭环是三类场景的共同结果：同一事项的请求、补充、决定、执行和交付能被追溯，验收后的经验可以在合适条件下复用。

## 六项统一标准

1. **事项连续。** 能解释请求、补充、决定、执行和结果的关系；无法确认时保留待核实原因，不凭文字相似强行合并。
2. **记忆可信。** 原始消息用于回查，临时进展留在任务；可复用结论经过 Source → Candidate → Review → Apply，保留版本、证据和适用条件。
3. **权限随入口。** Owner Assistant、DWS 值守和群 Jarvis 各自保存身份、上下文、受众和发送策略。读取资料不等于可以转述或执行；提问、引用和来源正文不能扩大权限。
4. **主动但有边界。** Owner 委托范围内可自动调查、执行和派发；删除、停服、生产变更、跨受众披露、对外发送、扩权和未知结果重放需要 Owner 确认。结果、阻塞和确认主动通知，无变化保持安静。
5. **交付可核对。** 区分方案生成、实际执行、外部发送和 Owner 验收；每一步说明证据、验证状态和未完成事项。
6. **经验改善下一次工作。** 只沉淀有复用价值且有依据的经验；条件变化和反例通过新版本修订，不覆盖历史事实。

## Skill 接入标准

其他 Agent 可通过 `memgov-memory` skill 查询和治理同一套 Source、Candidate、Review、Memory。Skill 记录 Agent、workspace、请求、证据、版本和幂等结果，不能直接改 SQLite，也不自动授予 shell、消息、云 API 或生产权限。群 Jarvis 可按自身配置接入 skill，仍遵守当前群的披露规则。

## 实施顺序

先统一配置和服务入口，建立 Owner 根任务与子 Agent 关系，再接通统一上下文、环境快照、结果通知和 skill 契约。随后修复 DWS 覆盖缺口、任务恢复和管理台健康，最后以真实 Owner 私聊、后台值守、Agent 委派、记忆写入及群 Jarvis 回归完成验收。

更多来源、跨群协作和新的外部连接器按实际需要另行设计。任何新能力都必须说明作用于哪个入口、增加什么权限、如何撤销以及如何证明群 Jarvis 的既有通道仍可用。

参考： [产品定位](positioning.md) · [总体架构](architecture.md) · [Owner Assistant 设计](../design/owner-assistant-design.md) · [memgov-memory skill 设计](../design/memgov-memory-skill-design.md)。
