# 私聊增量采集与七天会话保留

状态：采集与保留已在源码实现，离线覆盖见[详细稿](direct-message-retention-design-detail.md#失败语义与测试)；安装及真实双方收流本次未核验。主动观察的交付策略以[接入主设计](dingtalk-integration-design.md)为准。

本功能让独立钉钉数据源在明确开启后，同时采集已授权群聊和当前账号的同事私聊，并只保留最近 7 天原文。开启前及停用期间的私聊不导入；超过本地窗口的问题由 Agent 查询正式记忆或临时调用 DWS 回查。

实现细节与测试边界见[详细稿](direct-message-retention-design-detail.md)。

## 使用方式

```yaml
data_sources:
  work_chat:
    channel: dws-main
    groups:
      member_robot: app-main
    direct:
      enabled: true
    retention:
      days: 7
    backfill_after_enable: true
    history_import:
      enabled: false
      days: 30
```

配置预览和应用沿用 `memgov config plan` 和 `memgov config apply-runtime`。预览会显示待清理的过期消息数量。`direct.enabled` 默认关闭，旧配置不会自动扩大私聊采集范围。启用后，实时流保存新收到的同事消息；周期补漏发现新私聊，并补齐双方消息。普通重启继续原启用起点，关闭后再次开启会建立新起点。

`memgov message query <channel>` 支持 `--type direct|group`、`--contact-id-type`、`--contact-id`、会话和时间筛选。结果同时给出启用时间、可用起点、七天策略、水位和缺口。显示名只用于搜索候选。

## 核心行为

- DWS 群聊和私聊使用两个消费者，共享个人事件总线；任一侧失败都会明确显示，私聊故障不抢占或伪装成群采集成功。
- 私聊始终使用真实会话 ID。本人通知地址和应用机器人会话不会被当成同事私聊。
- 同事私聊只进入所有者主动值守。群 Agent 的群证据范围不包含私聊路由；采集内容也不授予外部操作权限。
- 查询立即排除超过七天的原文。启动时及每小时分批清理消息修订、事件载荷、来源片段、附件引用、搜索索引和运行时原文副本，包括关联的 Agent 沟通正文与理由。
- 到期后保留消息哈希、去重、审计、事项状态、正式记忆结论和来源定位；证据状态标记为已到期，需要回查平台才能再次验证。

## 本期范围

本期只实现 Claude/DWS 运行环境中的钉钉个人账号采集。主动值守复用现有事项识别、取消、完成和去重流程。DWS 临时历史查询不导入缓存，也不触发主动事项。
