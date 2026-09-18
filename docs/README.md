# 文档导航与维护标准

先看[定位](architecture/positioning.md)与[最佳落地场景](architecture/best-practice-scenarios.md)，了解产品目标；再看[实现状态](implementation-status.md)与[实施路线](roadmap.md)，区分已有能力、待办和验收缺口。上手使用见[项目 README](../README.md)。仓库内构建、测试和脚本示例默认从仓库根目录执行。

## 分类导航

| 分类 | 阅读入口 |
| --- | --- |
| 架构与目标 | [定位与检索取舍](architecture/positioning.md) · [总体架构](architecture/architecture.md)（[详细稿](architecture/architecture-detail.md)） · [最佳落地场景](architecture/best-practice-scenarios.md)（[验收标准](architecture/best-practice-scenarios-detail.md)） |
| 操作指南 | [初始化](guides/initialization.md) · [治理与恢复](guides/governance.md) · [AI 值守与首次群挂载](guides/runtime-user-guide.md) · [本地管理台](guides/local-console-user-guide.md) |
| 测试指南 | [场景 1 测试](guides/testing-scenario1.md) · [值守离线验收](guides/runtime-offline-acceptance.md) |
| 接口参考 | [CLI 契约](reference/cli-contract.md)；具体版本以该二进制帮助为准 |
| 状态与路线 | [能力状态](implementation-status.md) · [实施路线](roadmap.md) |
| 失效设计 | [旧版决策与 SQL](archive/decisions/README.md)，只解释历史，不作为现行用法 |

专项设计按功能查阅；主文档负责范围，详细稿负责实现与验证。表内状态只用于导航，核验结论统一见[实现状态](implementation-status.md)。

| 主题 | 主文档 | 详细稿 |
| --- | --- | --- |
| DWS 后台观察、Owner 私聊与群机器人 | [接入设计](design/dingtalk-integration-design.md) | [协议与迁移](design/dingtalk-integration-design-detail.md) |
| Agent preset、执行、权限与统一 sysprompt | [运行时设计](design/agent-runtime-design.md) | [执行约束](design/agent-runtime-design-detail.md) · [安全规则维护](design/agent-runtime-design-detail.md#统一系统提示与安全验证) |
| 私聊采集与七天原文保留 | [保留设计](design/direct-message-retention-design.md) | [数据与清理](design/direct-message-retention-design-detail.md) |
| 本人私聊记忆盘点与工具诊断 | [需求](design/owner-private-chat-requirements.md) | [需求依据](design/owner-private-chat-requirements-detail.md) |
| 记忆列表、计数与排序 | [列表设计](design/memory-list-design.md) | [接口草案](design/memory-list-design-detail.md) |
| 语音热词与 Agent 技能 | [配置设计](design/hotword-agent-skills-design.md) | [实现与验证](design/hotword-agent-skills-design-detail.md) |
| 群共享记忆 | [共享范围](design/group-memory-sharing.md) | [受众与查询](design/group-memory-sharing-detail.md) |
| 统一本地服务 | [服务使用与边界](design/unified-service-design.md) | [监督、重启与系统托管](design/unified-service-design-detail.md) |
| 本地管理台、记忆浏览与 Desktop 演进 | [管理台设计](design/local-console-design.md) | [接口与验证](design/local-console-design-detail.md) |
| 中断任务继续 | [继续任务](design/task-continuation.md) | [恢复与验证](design/task-continuation-detail.md) |
| 实时执行过程 | [任务终端](design/task-terminal.md) | [输出与验证](design/task-terminal-detail.md) |

## 维护标准

**新增前先查现有主题。** architecture 放长期目标与边界，design 放专项方案，guides 放现行操作和测试方法，reference 放已实现协议，archive 放已失效设计。新增文档必须加入本页导航；同一主题维护一个主入口，关联文档用链接引用。真实账号、消息、本机路径、数据库统计及现场运行证据不得提交到公开仓库；发布结论使用可复现的测试结果与 GitHub Release notes。

**主文档便于决策。** 用简短文字说明目标、用户怎么用、核心方案、本期范围和实施顺序。字段、接口、状态机、代码组织、异常与测试矩阵放同目录同名 `-detail.md`，双方互链。范围以主文档为准，设计阶段只确定体验、范围和重要取舍；实现细节按开发需要补充。

**保持架构方向。** 设计和验收对齐最佳落地场景；SQLite `state.db` 是唯一真相源，Source、Candidate、Review、Memory 是现行模型。来源证据、临时任务和长期记忆分别治理；明确所有者与群 Agent 的身份、上下文、执行及披露范围。资料正文不能授权操作。新增组件说明职责及必要性，不以工具或 Agent 数量作为效果指标。

**现状、计划和证据分开。** 文档开头标明性质与核对日期或来源。专项主设计决定目标范围；源码与契约说明现行行为；有版本和环境的验收记录证明验证结果。发生冲突时修正文档或记录未解决差异，不将设计当作源码、源码当作安装、安装当作运行。

| 状态维度 | 最低记录要求 |
| --- | --- |
| 设计与范围 | 设计中、已确定或已替代，并链接主设计 |
| 实现 | 未实现、部分实现或源码已实现；说明核对的提交或代码入口 |
| 验证 | 测试/检查方法、日期、结果、场景、环境和未覆盖项；仅阅读既有报告时标注“历史记录，未重跑” |
| 安装与运行 | 分别记录构建/版本及观测时间；未检查就写未核验 |

**AI 在提交与发布前同步文档。** 检查集中在 commit 提交前与发布 tag 前，开发过程不因每次编辑或普通对话反复扫描。项目知识或事实、架构决定、功能或行为变化时，在本次提交前更新受影响的总体架构、主详稿、指南、契约、状态、路线及导航；没有影响就不改文档。更新落实到文件并随变更提交，不能只在对话中说明。仅讨论中的设想、待实现或未核验内容明确标注，历史记录保留当时事实。用户明确要求文档更新时直接处理。

**路线与决策同步。** 路线记录下一步、依赖与完成条件，不复制协议，也不把没有资源和验收证据的目标写成承诺。重大的架构取舍记录原因、替代关系和证据。

**合并不丢决策。** 合并前找出独有决策、理由、可复现验证及未解决项。操作步骤归指南，技术约束归详细稿，版本交付结论归 GitHub Release notes。私有环境的现场日志和数据快照保留在仓库外，只将脱敏且对公开使用者有价值的结论写入文档。失效文档即使单独打开也要说明已失效并链接现行依据。

## 检查时机与缓存

| 时机 | 检查范围 | 复用条件 |
| --- | --- | --- |
| commit 提交前 | 本次暂存变更及受影响的文档；先完成必要更新，再检查最终暂存内容 | 缓存中 commit 检查通过，且暂存树指纹与 HEAD 检查基线均相同 |
| 创建或发布 tag 前 | 目标版本的全部文档导航、链接、架构与实现一致性、交付状态及已知缺口 | 缓存中 tag 全量检查通过，且目标树指纹完全相同；commit 增量结果不能替代 |

缓存由执行提交或发布任务的 AI 维护，路径通过 `git rev-parse --git-path docs-review-cache.json` 获取，随各工作区的 Git 本地目录隔离，不提交到仓库。它记录检查时间，不记录聊天正文，也不是产品状态或验收事实的真相源。

使用 JSON：顶层 `schema_version` 为 `1`，`checks.commit` 与 `checks.tag` 分别保存最近一次成功检查；未检查的项省略。每项记录 `checked_at`（UTC 时间）、`base_commit`（检查基线）、`tree`（受检 Git 树）、`changed_paths`（本次变更路径）、`reviewed_paths`（实际核对范围）、`result: "passed"` 及简短 `summary`。tag 项的 reviewed_paths 覆盖目标版本整套文档。

commit 指纹取 `git write-tree`，基线取当前 HEAD（首次提交时为 null）；tag 指纹取 `git rev-parse '<目标版本>^{tree}'`。检查内容必须来自相应暂存区或目标版本，不能拿未暂存修改替代。树指纹覆盖代码、文档和规范；满足表中条件才复用，不依赖文件 mtime 或固定时间间隔。切换分支或变更父提交后，增量检查基线不同也应重新核对。检查后再次修改或暂存内容，必须重新确认最终树；检查失败不能登记成功。

缓存缺失、损坏、版本不支持、Git 对象已被清理或指纹不同，都按未命中处理。commit 未命中时先看本次提交差异，再检查受影响内容，不自动全量扫描；tag 未命中时全量检查。没有文档影响也可以缓存通过结论；普通检查只更新本地缓存，不为刷新时间改写文档或创建 records 报告。缓存丢失可以重建，不影响 Git 提交历史。

这是 AI 执行约定，尚未安装自动 Git hook；手动执行 `git commit` 或 `git tag` 不会自动运行语义检查。缓存命中仅表示目标内容已按对应范围检查，不替代真实测试、安装和运行状态核验，也不授予 tag、推送或发布权限。

## 每次整理的完成检查

1. 查看 Git 状态，确认本次范围与已有改动；清点文档类型、重复主题和无入口文档。
2. 核对导航可达、本地链接和章节锚点、主详稿互链及迁移后的路径引用；映射表与明确历史上下文中的旧路径不当作现行引用。
3. 抽查相关源码、命令定义和已有验证记录，修正总设计与专项设计的冲突；检查实施路线是否覆盖未完成事项及验收依赖。
4. 区分本次检查与历史测试。纯文档整理执行 `git diff --check` 及链接、路径检查；涉及产品行为才补充相应测试。检查外部资料时另记访问结果，不把未联网核验的外链宣称为通过。
5. 合并迁移或实质性健康审查把公开有价值的结论更新到状态、路线或 Release notes；普通提交检查只维护本地缓存。审阅差异，只暂存本次改动，按上述约定检查最终暂存内容后提交，交付验证结果与提交哈希。

不采用含糊的“健康度分数”代替问题清单。尚缺真实业务验收、安装版本证据或架构决定的事项，应持续出现在状态与路线中，直到有证据关闭。
