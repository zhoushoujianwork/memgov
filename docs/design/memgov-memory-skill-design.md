# `memgov-memory` 跨 Agent 记忆接入设计

状态：将现有 skill 说明提升为正式的跨 Agent 接入协议；主线范围已确定，稳定 API/CLI 契约和安装版本仍需按实现状态验收。实现细节见[详细稿](memgov-memory-skill-design-detail.md)。

## 定位

`memgov-memory` 是 memgov 记忆和证据底座的标准 Agent 接入口。Owner Assistant、群挂载 Jarvis 以及其他获准的 Agent，都可以通过它查询和维护同一套 Source、Candidate、Review、Memory，而不直接操作 SQLite。

memgov 负责保存证据、控制工作区和受众、管理版本与复核、记录操作历史；宿主 Agent 仍负责自己的模型、工具和外部动作。skill 的“完整记忆能力”不等于本机 shell、DWS 发消息、云平台或生产权限。

## Agent 可以做什么

- 召回和查看正式 Memory、Source、Candidate、Review 及历史；
- 写入有稳定来源的 Source；
- 创建或修订 Candidate，执行结构验证和差异检查；
- 提交 Review，并按核对后的 digest 应用 Memory；
- 修订、退休、恢复 Memory；
- 查询证据、版本、适用条件、工作区和操作记录。

所有写入都必须指定 workspace、actor、请求 ID 和幂等键，并引用真实证据。skill 返回“来源”“候选”“已应用记忆”和“操作失败”等明确状态，Agent 不能把候选或搜索命中说成正式记忆。

## 与 Owner Assistant 和群 Jarvis 的关系

Owner Assistant 通过 skill 把任务结果和长期经验连接起来；临时任务进度仍留在任务记录中，只有经过 Source → Candidate → Review → Apply 才进入 Memory。群挂载 Jarvis 可以按自身配置使用同一 skill，现有群 preset、工具、技能、记忆范围和原群回复通道保持不变。Owner Assistant 不会借 skill 把 Owner 私聊历史或本人授权转入群；未来群侧限制另行决定。

## 本期范围与顺序

先稳定 CLI/API envelope、身份/工作区/证据/版本/幂等字段，再让 Owner Assistant 和其他 Agent 通过同一适配器调用；随后补齐冲突、恢复、审计和群 Jarvis 回归验收。旧 skill 命令继续兼容，设计文档不把未安装的接口写成现行能力。

范围和用户体验以本文为准，调用契约、错误和测试矩阵见[详细稿](memgov-memory-skill-design-detail.md)。
