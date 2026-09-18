# memgov v2 架构：实现约束

范围以[总体架构](architecture.md)为准。核对日期：2026-09-16；这里保留现行模型和数据约束，不作为安装或真实业务验收证明。

## 模块职责

Schema 24 使采集维护与模型调度都可保存断点；历史源时间原值和解析版本属于证据，调度使用本地接收时间。DWS 查询共享两槽，实时长连接独立；SQLite 仍是唯一真相源，不引入队列服务。

2026-09-18 后台调度增量：SQLite 同时持有工作领取、共享并发额度、版本/权限围栏、租约和截止；内存只承载有限活动 worker，不引入外部队列。慢分析与慢执行不阻塞采集和交互通道。运行心跳与业务进展分开观测，协议和阶段验收见[运行时详细稿](../design/agent-runtime-design-detail.md#cyber-并发与可靠性2026-09-18)。

| 模块 | 职责 |
| --- | --- |
| `cmd/memgov`、`internal/cli` | 进程入口、命令和 JSON envelope；适配配置及控制回调 |
| `internal/core` | 数据模型、迁移、证据、记忆治理、配置、任务与投递事务 |
| `internal/channel` | DWS 与应用机器人适配、身份和采集；平台差异不进入记忆模型 |
| `internal/runtime`、`internal/agent` | 执行策略、外部 Claude 调用、任务恢复与工具边界 |
| `internal/sysprompt` | 内置公共自我定位、安全规则和各类入口基础提示；统一组合与内容指纹，维护与验证见[运行时详细稿](../design/agent-runtime-design-detail.md#统一系统提示与安全验证) |
| `internal/service` | 数据目录互斥、模块监督、停止、重启，以及 macOS launchd 托管和二进制替换检测；不复制业务状态机 |
| `internal/console`、`internal/observation`、`internal/runlog` | 本机页面与查询、进程观测、诊断及受管理输出；心跳和日志不替代 SQLite |
| `web/` | React/TypeScript 页面与 Vite 开发构建；产物写入 console/static 并内嵌，不引入服务端前端运行时 |
| `internal/scenario`、`cmd/scenario-driver` | 独立采集与场景测试基础，不属于通用核心执行器 |

统一服务中管理台通过受限回调执行服务重启与任务继续；独立 `ui` 不注入这两个回调。YAML 声明编辑与应用分开；任务过程使用只读 SSE。机制分别见[统一服务](../design/unified-service-design-detail.md)、[管理台](../design/local-console-design-detail.md)、[任务继续](../design/task-continuation-detail.md)和[实时终端](../design/task-terminal-detail.md)。

管理台另提供独立的 GitHub tag 版本检查，优先复用本机 `gh` 登录，匿名 API 为后备；远端结果仅作更新提示，保留内存缓存，不成为 SQLite 业务状态，也不参与业务页面查询事务。

## 正式模型

Memory 的 UUID 不依赖标题和文件名。分类为 fact、preference、constraint、decision、procedure、lesson。保存标题、召回摘要、Markdown 正文、工作区、实体、标签、适用条件、观察时间、有效期、证据、状态和递增版本。全局记忆在交换 JSON 中使用空 workspace_id，SQLite 内部使用保留工作区 global。

Source 保存 UTF-8 文本快照、位置、来源类型、捕获时间、内容指纹和来源关系。SourceFragment 使用行及 Unicode 字符偏移定位，内容逐片拼接可还原快照。Memory 与证据为多对多。证据必须引用实际片段，匹配 source_id、fragment_id 和该片段的 SHA-256；quote 如提供，必须是原文子串。

Candidate 表达 create/update，更新引用 target_id 和 expected_version。调用方审阅完整候选后使用 expected-digest 应用；结构校验不能证明语义正确。

## 事务与历史

业务变更、MemoryRevision、Operation、FTS 触发器更新、请求记录和幂等结果在同一 BEGIN IMMEDIATE 事务提交。CAS 条件包含 ID、工作区和期望版本；发生冲突时整个操作回滚。

正常修订及撤销追加版本。敏感清除是销毁正文历史的例外，保留非正文墓碑与操作事实。restore 命令恢复旧版本为一个新版本，不回拨版本号。

数据库启用外键、WAL、synchronous=FULL、secure_delete、FTS secure-delete 和有界 busy timeout。全部新版进程持共享文件锁；数据库替换及物理整理持排他锁。锁和取消由调用期限约束。数据库目录默认 0700、文件 0600。

同一服务进程内、指向同一 `state.db` 的写事务先经过共享写入门禁，避免多个模块把 SQLite 的正常单写者语义放大成瞬时 busy 失败。门禁只负责串行化，不能降低写放大：运行时的确认、消息同步、批次、动作和任务领取先做精确只读判断，无实际工作时不进入写事务、不记录内部空轮询；命中后仍在事务内复核并条件领取。统一写入口分别测量门禁排队、连接及 `BEGIN IMMEDIATE`、事务执行与提交耗时；任一阶段达到 250ms 时向服务标准日志写入命令名、分段耗时、结果和错误码，不记录数据库路径、业务正文或请求载荷。该观测用于确定是否需要对具体后台业务做小批提交；当前不把实时回调、审批或 Outbox 任意合并成延迟事务。

schema_migrations 记录连续版本与不可变脚本指纹。初始化从 v1 基线开始，应用该二进制包含的后续迁移；已有库通常通过显式执行 init 升级。已安装的系统托管服务在启动前持进程锁及数据库独占锁，先保存并校验一致性备份，再事务迁移受支持的旧 Schema；普通读取与前台命令不自动升级，详见[服务详细稿](../design/unified-service-design-detail.md#macos-系统托管)。迁移链只追加脚本，不修改已发布脚本。读取旧版本库不会静默重建；新于二进制支持范围的库会被拒绝。

## 检索

FTS5 trigram 覆盖记忆和来源，结果区分 kind。短于三个字符的词走子串路径。查询词按字面值处理，不接受任意 FTS 表达式。召回在 SQLite 读事务中固定快照，先按工作区、active、已知有效期过滤，再按 Unicode 字符预算组装上下文。自然语言适用条件交由调用方判断。

FTS 是派生数据，index rebuild 不替换正式数据。doctor 检查 SQLite、外键、当前版本、证据指纹、片段完整性与 FTS 差异。

## 交互投递分离

2026-09-18 交互状态增量：本人私聊与有效群 @ 共用阶段协议，“已收到”“处理中”“已完成”“打叉”各有独立 Outbox 与平台结果，阶段变化先移除上一标记再添加当前标记。Stream 回调在消息短事务提交后广播合并唤醒；每个相关 Runtime 立即检查 SQLite，1 秒扫描只作恢复兜底。阶段请求进入 Runtime 内的有序旁路队列，不等待平台表情接口即可开始 Agent；群目录发现和历史补漏由独立循环运行。`awaiting_confirmation` 保持“处理中”，审批后继续原任务。启动恢复按 SQLite 任务状态补齐尚未开始的处理中、完成或失败阶段，管理台展示完整阶段投递记录；阶段标记不构成已交付回答，不进入恢复的回答历史。群任务进入失败或外部操作结果未知时，只要任务、摘要或动作尝试已有非空结果，就另建幂等失败结果回复，保留结论并明确未完成状态；没有结果时不编造正文。两入口不调用 Haiku，也不展示读取或观察 reaction。协议见[接入详细稿](../design/dingtalk-integration-design-detail.md#交互接收回执)。后台观察完成继续固定为 record_only，独立 Agent 工具操作另记审计，旧自动完成通知不再投递；源码实现与离线验证已通过，真实模型与平台业务验收仍待完成。

2026-09-17 群回复增量：普通答复使用 Markdown，并通过回调期内存 webhook 在末尾原生 @ 发起人；该凭证不持久化，过期或服务重启后降级为一次可读的昵称文本。receiver 与 Agent worker 共享同一个按通道隔离的应用 Adapter，主动群消息接口不伪装支持 @。pending 才显示审批卡片并同时 @ 已核验 DWS 所有者。已发布关联应用的审批模板；仅所有者可同意或拒绝，同意后执行，拒绝后取消，来源、路由、身份或展示动作变更不继承旧卡片授权，口令不能绕过卡片。源码与离线验证已具备，真实群按钮往返待验收，详见[确认卡片](../design/dingtalk-integration-design-detail.md#群回复与确认卡片)。
