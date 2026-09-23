# 值守离线验收

范围说明：脚本使用临时二进制和合成数据，验证当前源码的迁移、恢复、Owner Assistant 任务与发送审计；完整 Agent、机器人权限及上下文测试另由 `make check` 和 `make test-runtime-race` 执行。[验收矩阵](../architecture/best-practice-scenarios-detail.md#personal-jarvis-固定验收案例)分别记录代码、二进制与真实平台结果，历史结果不能代替本轮验证。

运行 `scripts/runtime-offline-acceptance.sh`，脚本会将当前源码构建到临时目录并验收。
验证已经安装的可执行文件时，传入其绝对路径：

```sh
scripts/runtime-offline-acceptance.sh /absolute/path/to/memgov
```

脚本仅使用临时数据库和 fake DWS；不读取本机钉钉、Claude 配置，也不更换正在运行的二进制。
指定的二进制必须支持当前源码的数据库版本。默认构建和测试需要本机 Go 环境。

覆盖范围：

- Schema 27 archive-before-drop migration; new workspaces start empty. The script also exercises the actual Workspace wrapper/CLI against a temporary home. Configuration conversion, config-plan validation and the complete file/concurrency/isolation matrix run in their CLI, storage and runtime tests under `make check`.

- Owner private execution, proactive record_only completion, scoped workspace access and task/delivery audit. Root/child delegation and automatic proactive notifications are separate future acceptance.
- 群内有效 `@` 继续进入原群 Jarvis，检查它保留原有 Agent、技能、工具、Workspace 边界、回复身份和确认通道，不继承 Owner 私聊权限，也不把群结果转到 Owner 私聊。
- 创建真实 schema 6 测试库，将副本交给二进制 `init` 升级到当前版本，检查原副本不变和数据保留。这是隔离的迁移演练，不是产品新增的 `--dry-run` 参数。
- Schema 21 升级到 22 时只停用旧后台自动通知，保留历史结果与显式权限；旧 Outbox 不能经手工派发恢复自动通知。
- Cyber owner 缺少信息仍可进入调查，阻塞只记录；独立发送覆盖本人身份、证据披露、幂等、迟到回执、证据修订及自发消息防循环。
- 关闭并重新打开 SQLite，检查独立采集游标、主动消费者等待起点、群 @ 消费者待分析消息保持，未完成分析可以重新领取。
- 已完成机器人交互任务在投递过程中中断：投递及其尝试恢复为 unknown，任务不重新执行，也不盲目再次发送；proactive record_only completion does not create a notification。
- 用受测二进制启动、停止并再次启动独立采集；fake DWS 返回不完整历史并断开实时连接，检查缺口和失败状态入库、租约释放、无 AI 执行，以及原始 stderr 不进入进程输出。

成功仅代表离线恢复与故障注入通过。真实钉钉回调、机器人成员关系、用户接收结果和模型执行仍需要单独业务验收。
