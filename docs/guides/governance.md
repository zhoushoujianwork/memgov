# 治理、清除与恢复

## 合并

`memory merge preview --input merge.json` 接受：

```json
{"sources":[{"id":"MEMORY_A","version":2},{"id":"MEMORY_B","version":1}],"memory":{"category":"fact","title":"合并标题","summary":"合并摘要","content":"有证据支持的合并正文","evidence":[]},"reason":"说明合并依据"}
```

preview 校验源记忆版本与工作区，自动保留全部证据。它返回 plan.id/digest 和完整目标内容，供人或 Agent 审阅。`memory merge apply PLAN --expected-digest DIGEST` 在一个事务中创建新目标、将源记忆标为 superseded、追加历史与替代关系。

`memory merge undo PLAN --reason TEXT` 仅在所有参与记忆仍处于合并后的版本时成功。撤销追加新版本并撤去当前替代关系；历史保留原合并操作。任何后续修改都返回冲突。

## 冲突关系

`memory links ID` 查询关系；`memory links conflict A B --expected-version VA --other-version VB --reason TEXT` 绑定双方当前版本，原子记录 conflicts_with 并将双方改为 disputed，新版本立即退出默认召回。只允许同一显式工作区中的 active/disputed 记忆参与。

`memory links resolve A B --expected-version VA --other-version VB --reason TEXT` 删除冲突边并追加审计版本，双方仍保持 disputed。随后通过审阅过的修订或显式 restore 重新启用正确结论。存在未解决冲突边时禁止直接恢复 active。

## 敏感正文清除

```bash
memgov memory purge preview MEMORY_ID --expected-version N
memgov memory purge apply PLAN_ID --expected-digest DIGEST
memgov memory purge status PLAN_ID
```

preview 返回受影响记忆、来源、候选、任务、合并计划、操作及墓碑指纹。共享证据、同内容来源和明确的来源派生关系可能扩大影响范围。预览后如果有新候选或版本变更，apply 要求重新预览。

逻辑清除销毁受影响记忆的正文和历史、候选载荷、相关任务输出与问题、来源正文/片段/位置、相关合并计划载荷。保留来源的非正文标识、指纹和墓碑。为覆盖响应与自由文本注释中的副本，清除所有幂等响应缓存及自由文本审计理由，调用方标签改为 redacted；操作类型、时间、ID 和版本事实仍保留。这个全局缓存/注释影响也会在 preview 中明确报告。

物理整理在排他维护锁下执行 WAL checkpoint、VACUUM 和再次 checkpoint。中断后状态仍为 logical_complete；对同一计划重复 apply 可继续整理。`--logical-only` 可显式分离这两个阶段。成功整理状态为 database_compacted。

外部原始文件、之前生成的备份、导出以及系统/磁盘快照不在清除范围内。预览报告已知来源位置和潜在外部残留；不宣称整机物理擦除。普通历史不可变，敏感清除是正文销毁的明确例外。

## 备份与恢复

backup create 使用 SQLite 的一致性快照，文件先校验和 fsync，再以不覆盖既有文件的方式发布。备份包含全部 SQLite 权威状态，可以独立于旧来源目录使用。verify 检查文件指纹、完整性、Schema、库角色与外键。

restore 在隔离暂存数据库中完成：验证快照 → 合并当前墓碑与清除凭据 → 重施清除 → 追加恢复操作 → 整理并验证 → 原子替换主数据库。恢复期间新版 CLI 的共享文件锁被排他锁阻挡。旧任务租约失效，旧响应缓存不重放。

恢复不清除当前墓碑，也不会让已清除正文复活。无法校验、指纹改变、锁冲突、暂存处理失败时，当前业务库保持可用。进程或机器在文件替换后中断时，SQLite 主文件仍是一份完整数据库；再次用 verify/doctor 检查。临时 restore 文件及旧备份可能仍是外部残留，应独立盘点。

JSON export 提供完整权威表交换包；SQLite 备份是当前的恢复入口。Markdown 是阅读材料，不用于重建审计历史。
