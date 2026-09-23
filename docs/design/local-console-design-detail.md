# 轻量本地管理台与 Desktop 演进：详细稿

Status: current console source includes a read-only Agent Workspace browser. The [main design](local-console-design.md) defines scope and the [user guide](../guides/local-console-user-guide.md) defines operation. Historical validation below applies only to its named version.

## 1. 已有基础与需要补齐的部分

代码核对日期：2026-09-16。当前已有 CLI：

| 已有入口 | 管理台可复用的数据 |
| --- | --- |
| `runtime status [runtime]` | 配置状态、待处理数量、任务状态统计 |
| `runtime task list <runtime>` | 任务列表，支持 status、limit |
| `runtime task show <task-id>` | 请求消息、执行尝试、结果、未执行动作及主动沟通记录 |
| `runtime message list <task-id>` | Agent 主动沟通的身份、目标、依据、内容、幂等键与回执 |
| `runtime logs show <runtime>` | 按 task、时间、组件过滤的结构化日志 |
| `data-source status <source>` | 数据源、接收租约、覆盖窗口与诊断信息 |

上述功能以 `internal/core`、`internal/runtime`、`internal/runlog` 为基础。CLI 中组装的查询可提取为小型共享函数，不以网页每次启动 CLI 子进程作为数据接口，不要求先重构整个项目。

现有私聊任务标题可能只是“本人会话”，列表可以临时从未过期原消息生成一行预览，打开详情才展示正文；不额外运行模型生成标题，不持久保存原文副本。普通问答、状态查询也可能有任务记录，应标明入口和记录类型，不把每条记录宣传为独立后台工作事项。

现有运行状态记录不能独立证明进程存活；数据源的接收租约也不能证明 Claude 正在执行。当前 `runtime restart` 会在调用终端前台运行，因此不能直接放进普通 HTTP 请求处理函数。

## 2. 技术组织

`internal/console` 管理 HTTP 路由、查询视图与内嵌静态文件；`internal/observation` 管理可丢弃的运行包装进程心跳，`internal/cli/ui.go` 提供启动入口。查询连接为 SQLite `mode=ro`、`query_only=1`，不创建锁文件或迁移数据库。

```text
浏览器 / 将来的 Desktop WebView
  页面：任务、工作区、运行、设置
       │
  数据客户端       桌面能力适配（打开链接、复制、通知等）
       │
  本地查询 API
       │
  现有 Go 业务与日志读取函数
       │
  state.db / 运行实例观测 / 受管理日志
```

- Go `net/http` 提供 HTTP，`embed` 内嵌页面资源，发布仍为一个 memgov 二进制。
- 前端源码位于 `web/`，采用 React 19、TypeScript 和 Vite，使用语义化 HTML 与 CSS 变量；不引入 SSR、全局状态框架、插件系统或服务端前端运行时。
- 页面只维护筛选、选择和短期加载状态；查询结果由服务端重新提供。共享组件保持少量、按实际重复抽取，不制造通用表格引擎。
- Vite 生产产物写入受版本管理的 `internal/console/static/`，Go 内嵌该目录；生成目录不手工编辑。服务只提供现有页面路径和产物 assets 文件，不提供源码或目录索引。
- `make build` 先按锁文件安装开发依赖、检查类型并构建前端，再生成二进制；`make check` 增加前端类型和测试检查，发布脚本也先构建前端。仅使用 Go 的检出仍有已提交产物可供编译，但前端修改需重新生成。

依据：[Go embed 文档](https://pkg.go.dev/embed)支持编译期内嵌文件并通过文件系统接口使用；[MDN JavaScript modules](https://developer.mozilla.org/en-US/docs/Web/JavaScript/Guide/Modules)说明浏览器模块的 import/export 用法。这里采用通用标准能力，不依赖特定桌面框架 API。

### Frontend implementation

The card-specific React components are removed. Workspace files use a two-pane list/document reader with scoped search and expandable revision metadata. Existing YAML editor and terminal adapters remain. Historical Relayer attribution is retained in source and embedded assets.

四个页面和外壳采用 TSX，接口展示类型集中在 `web/src/types.ts`。原有安全 Markdown、时间、客户端及 YAML 编辑和只读 SSE 控制器保留为 JS 模块；编辑与 SSE 放在 React 管理的隔离 DOM 容器内，通过适配组件挂载、清空和卸载，不建立第二套业务控制器。界面隐藏、断线和来源过期仍清空相应正文；请求以页面及筛选生命周期隔离，过时响应不写回。

`make web-dev` 在 `127.0.0.1:5178` 启动 Vite 热更新，`/api/v1` 代理到已有 `127.0.0.1:8787`（含 SSE）；只适配精确本地开发 Origin，生产服务的 Host、Origin、跨站标记及 JSON 控制校验不变。生产 CSS 和 JS 均为本机静态资源，不需要放宽 CSP 或加载 CDN。开发依赖 Node 22.16+；最终用户无需 Node。操作见 [web/README](../../web/README.md)。依据：[Vite](https://vite.dev/guide/)及 [React 构建说明](https://react.dev/learn/build-a-react-app-from-scratch)。

## 3. 查询与刷新

页面路径为 `/tasks`、`/workspaces`、`/running`、`/settings`，服务器仅对这四个路径及其末尾斜杠形式返回入口 HTML，未知路径仍返回 404。资源和 API 使用根相对地址，直接访问及刷新不受当前路径影响。前端启动从 pathname 选择页面，根地址和 `index.html` 以 replaceState 归一到 `/tasks`；侧栏普通点击用 pushState，popstate 恢复页面并使旧请求失效，不重复写历史。导航采用实际链接，保留键盘、复制地址及修饰键新标签打开；当前页标记 aria-current，标题随页面更新。URL 暂不保存筛选、分页或详情。

使用 `/api/v1/meta`、`runtimes`、`tasks`、`tasks/{id}`、`tasks/{id}/logs`、`sources` 与 `agents` 的 GET 查询。任务列表支持 runtime、status、limit 与 created_at/id 游标；默认 30 条、最大 100 条。不再提供 `/api/v1/session` 登录接口；Agent 声明编辑只写 YAML，见下文。任务继续通过独立控制回调复用核心事务。不提供任意 SQL、文件路径或 shell 执行入口。

`runtimes` 在同一只读快照中用一次配置查询和一次聚合统计查询生成全部运行状态，不随 runtime 数量重复查询。成功快照在服务进程内缓存 1 秒，并合并同时到达的刷新；失败不缓存。该缓存只包含运行状态，不写浏览器或 SQLite，最多带来 1 秒展示延迟，HTTP 仍使用 `Cache-Control: no-store`。

每份响应带采样时间与版本信息。列表按稳定的时间与 ID 排序，服务端分页并限制条数；详情正文和日志按需加载，避免全量轮询聊天数据。未来字段采用兼容增加，错误沿用项目已有 code/message 语义。

首版默认约 3 秒刷新当前可见页面的状态，日志仅在展开时拉取增量；同一个视图不并发堆叠请求。后台标签页暂停刷新，回到前台立即更新。失败使用退避并标记数据陈旧，不能把旧状态继续渲染为实时状态。

状态页面继续使用轮询，不建设事件总线或持久化 UI 事件库。任务执行过程已通过 `/tasks/{id}/terminal` 使用只读 SSE，按尝试选择输出，并持续核对来源可用性。重连时重新校准，不依赖无保证的内存事件恢复。

## 4. 状态真实性与版本

任务记录、执行实例、采集连接和交付结果分别展示。任务详情的 `deliveries` 使用 `purpose` 与 `transport` 区分交互答复、接收回执、处理中标记、完成标记和失败回执，分别呈现“已收到”“处理中”“已完成”“打叉”的平台结果；私聊与群 @ 阶段历史都保留在管理台，原消息上只保留当前标记。接收或阶段标记被平台接受不能解释为最终回答已经送达。新群任务的操作详情与确认入口合入原群答复；当前使用所有者文字确认口令，旧私聊确认记录保留历史分类。首页“需要关注”是展示分组，不新增任务状态或改写数据库。

| 观测情况 | 展示含义 |
| --- | --- |
| 任务 running，运行实例已停止 | 需要关注：实例已停，任务记录未收敛 |
| 任务 running，进程存在但近期无步骤事件 | 执行实例存在；显示最后活动，不推断卡死或虚构当前步骤 |
| 无可靠进程关联证据 | 执行状态未知，展示最后已知信息 |
| 数据源有有效接收租约 | 接收进程持有租约；平台就绪仍依据适配器观测 |
| 任务完成，交付记录缺失或失败 | 处理已完成，交付未确认或失败 |

新版 `runtime start/restart` 的包装进程每 5 秒写独立实例心跳，关联规范化数据目录、运行时 ID、随机启动实例 ID、PID、版本与启动时二进制 SHA256。每个进程仅清除自己的记录；20 秒之外的心跳不可信，多份新鲜心跳显示需要核对。心跳只证明近期上报，不推断 Claude 当前工具或业务健康。旧进程无心跳时显示未知；不得用页面刷新给后台进程续租。

分别显示管理台服务版本、已安装二进制构建标识、执行进程报告的构建标识和已应用配置版本。旧进程未报告版本时显示未知，不把磁盘上的最新二进制当作正在执行的版本。

## 5. 本地访问与正文生命周期

首版仅面向当前本机所有者，监听 loopback；不提供局域网模式、多人登录或群成员 Web 门户。读取范围由服务端固定在选定数据目录与所有者授权范围，不能由前端随意传 actor 参数切换身份。

管理台仅接受 loopback IP 监听地址，CLI 固定监听 `127.0.0.1`。启动 URL 不携带 token，不创建或验证登录 cookie，无会话到期限制；旧地址中的 `#token=` 在页面启动时清除，不再发起登录请求。保留精确 Host、Origin 与 `Sec-Fetch-Site` 跨站检查，不开放跨域访问；写操作仍要求同源 JSON 和 `X-Memgov-Console: 1`。

消息和日志按不可信文本渲染，禁用原始 HTML 与脚本执行；技能、权限和模型页面不得返回环境密钥、令牌或完整凭据配置。诊断日志使用已有结构化字段；任务终端只呈现受管理日志中的公开文字和工具事件，并受来源及任务版本校验。

正文保留规则必须经过所有管理台查询入口统一执行，不能直接把旧的详情结构无过滤返回。检查范围包括任务请求、列表预览、尝试结果和日志摘录。Workspace knowledge has a separate file lifecycle; it is not a raw-message retention cache.对于来源过期的关联正文，按现行治理规则隐藏；对无法安全确认的历史自由文本副本，首版仅提供元数据与错误码。

响应携带正文到期信息；前端在截止时清空相应内存内容，刷新或回前台时重新校验。页面进入后台或断开连接时清空敏感正文，保留无正文状态和必要 ID，恢复后重新读取。HTTP 使用 `Cache-Control: no-store`，不注册 Service Worker，不把正文写入 localStorage 或 IndexedDB。浏览器缓存策略不承诺删除用户自行复制或截图的内容。

## 版本与 tag 检查（2026-09-17）

侧栏 `VersionStatus` 独立轮询 meta（可见时每 15 秒，隐藏时清空并暂停），使用 meta 的 `version/build/installed_build` 显示运行版本及本地一致性，不把本地指纹一致当作最新版本。连接、运行版本或构建变化时单独请求 `GET /api/v1/version/check`，也支持手动检查；旧请求返回不能覆盖重连后的新结果，断线禁用按钮。检查不加入业务页面查询批次，远端故障不会清空正常业务数据。

服务固定查询 `zhoushoujianwork/memgov` 的 tags。优先调用已有 `gh api --hostname github.com ... --paginate --slurp`，沿用用户登录以支持私有仓库；不可用时尝试匿名 GitHub API。整个读取受八秒期限约束，CLI 输出上限 1 MiB；匿名接口每页最多 100 条、最多 10 页，每页上限 1 MiB，只接受固定主机分页，未完整读取则不宣称最新。检查不读取业务数据库，不保存或返回凭据。沿用 Host、Origin 和跨站请求校验。

遍历 tag 的 `name`，接受带可选 `v` 前缀的 SemVer，包括预发布与构建元数据；按 SemVer 选最高版本，不按字符串排序或依赖 Release。非版本 tag 忽略。响应包含 `state`（current/update/ahead/unknown/unavailable/not_found）、可用时的 `latest_version/tag_url`、`checked_at`；tag 链接由固定仓库及转义后的名称生成。空列表或没有可比较 tag 为 not_found；无权限、超时、限流、格式错误和超出边界为 unavailable，不误报最新。相同版本号仅说明版本号一致，不保证本地源码构建等同 tag 的内容。

成功结果在进程内缓存 15 分钟；失败或无 tag 缓存 1 分钟，并合并同时发起的请求。手动检查也遵循该缓存；服务重新启动会清空。没有下载、安装、创建 tag 或推送行为。该能力支持“事项闭环”的运行版本核对，不新增真实平台操作权限。

验证：`make check`、`make web-check` 与 `go test -race ./internal/console -run 'Test(Version|Tag)' -count=1` 覆盖 SemVer、预发布、乱序与分页、空 tag、无权限、限流、异常响应、并发缓存及输出边界。真实 tag、升级提示和业务发送仍由部署者验收。

## 6. Agent 声明编辑与控制操作

Agent 编辑由 CLI 配置适配层提供，界面不直接写 SQLite。`GET /api/v1/agent-config` 返回选定 YAML 的 Agent、内容修订摘要、引用和待应用状态；`POST /api/v1/agent-config/preview` 校验 update/delete，`POST /api/v1/agent-config/save` 保存同一草稿。POST 要求精确 Origin、JSON 与专用请求头；另有统一服务注入的重启与任务继续控制接口，见下文；其余写路径拒绝。

只允许编辑已声明的名字，禁止注入运行时管理的 resolved 技能。重新读取 YAML，以 `loadConfig`、`NormalizeDualModeConfig` 及 Claude 技能发现验证新声明；未知字段、非法权限、缺失技能与失效文件产生明确错误。保存不绕过 `BuildDualConfigPlan` / `apply-runtime` 的运行中依赖、边界再授权和预期版本要求；这些检查仍在实际应用时执行。

删除检查当前 YAML 和已应用配置的所有应用与群绑定引用，包括停用但保留的配置引用。未绑定 Agent 可以删除；删除不涉及任务、Workspace 文件、Preset 和技能目录。默认运行时未声明的 Agent 不支持此编辑接口。

采用 yaml.Node 保留其他配置及文档注释；目标 Agent 字段重新编码，内部字段注释可能变化。Agent 锚点或别名需先改为普通映射。写入采用同目录临时文件、原权限、Sync 和原子替换，保留配置符号链接；固定目标的 `.ui.lock` 防止管理台之间并发覆盖，并在替换前再次校验内容摘要及链接目标。外部不遵守该锁的文件编辑器仍应避免在保存瞬间同时覆盖；不承诺跨 YAML 与数据库原子提交。

待应用状态比较规范化 Agent 声明，排除运行时技能摘要；磁盘技能内容变化继续由原运行时技能机制管理。编辑窗口独立于状态重绘；修改字段使已预览草稿失效，失联或隐藏页面时关闭草稿，不持久缓存。技能清单使用后台扫描与约 30 秒缓存，单份策略不重复派发扫描；扫描中返回明确诊断，HTTP 查询不等待目录遍历，预览新技能需在扫描完成后重试。配置读取失败只显示编辑诊断，运行时已应用查询仍可用。

新增测试覆盖预览不写文件、保存不应用数据库、旧修订拒绝、配置及技能验证、未声明字段、YAML 和已应用引用、符号链接与权限、并发管理台保存及同源请求要求。浏览器操作在独立临时实例执行，不修改真实 Agent 声明。

### 重启入口与时间呈现

重启按钮位于侧栏底部，四个页面共享单一入口，复用现行重启请求与重连流程；等待状态、断线和独立诊断页分别禁用并说明。窄屏在导航下保留入口，不随旧 sidebar-foot 提示一起隐藏。

React `Time` 组件和原 DOM 适配器的 `time.js` 共享相对时间格式：少于一分钟显示“刚刚”，未来的短间隔显示“即将”，其余按分钟、小时、天、月、年显示前/后。缺失显示未记录，非法值显示时间未知；没有结果的结束时间仍显示尚未记录。datetime 保留原始时刻的 ISO 表示，title 与 aria-label 包含完整日期和时区；可见页面每 15 秒更新已有时间节点，避免重绘列表破坏浏览位置。任务、尝试、投递、消息、诊断日志、终端、Workspace 文件与历史、心跳、数据源覆盖及采样时间均使用同一格式，原文中的自由文本不改写。

### 既有控制与后续接入

首版提供查询命令复制，命令含正确数据目录和实例，参数须安全引用。界面可保存 Agent YAML 声明；读取页面不启动采集、迁移数据库或执行任务。统一服务已提供显式重启和失败任务继续入口，沿用既有服务端校验；独立诊断页面不启用这些控制。库版本不兼容时显示诊断与处理命令。

后续写 API 复用核心事务、审计、预期版本和幂等机制。前端依据服务端返回的可操作项显示按钮，不自行推断任务能否重试或取消。取消先表示已请求，确认执行已结束后才表示停止成功；重试不得掩盖尚未核实的外部执行结果。

实例启停需明确管理者和实例身份，启动/停止请求返回操作记录，页面持续观察直到确认结果。关闭网页不得终止用户原有的 CLI 实例，页面也不得自动接管或重启同名实例。现有本人渠道的外部动作确认不因增加管理台而失效或放宽。

## 7. Desktop 复用边界

页面只调用数据客户端，不直接 import 桌面框架 SDK。打开外链、复制、文件选择、通知等能力通过少量可选适配函数提供；浏览器具备默认实现或说明不可用。

桌面阶段优先验证系统 WebView 加同一 Go 本地服务的方式。外壳负责窗口与系统集成；业务仍由 memgov 处理，不复制 SQLite、审批和任务状态机。具体使用 Go 友好的外壳还是其他方案，按当时打包与平台需求选择，本期不承诺 Tauri 或特定框架。

桌面启动时选择连接现有服务或启动自己管理的实例，校验数据目录、协议版本和实例标识。两者的关闭责任必须不同：不关闭外部已有实例；自身启动的管理台服务可随窗口退出，后台采集与任务生命周期由明确设置决定。签名、升级、崩溃恢复和平台验收留到 Desktop 阶段。

## 8. 实施与验证重点

1. 抽取查询视图，补齐运行实例观测与版本辨识，验证 running/进程停止的真实例子。
2. 增加本地只读 HTTP 服务与本机访问检查，复用现有 core 数据，不创建第二套持久存储。
3. 完成任务列表及详情，再接运行页和只读设置页；完成可访问性、窄窗口和大列表分页检查。
4. 使用临时测试数据库验证过期、权限、失联、版本不一致和日志不可用；随后用真实私聊任务核对整条路径。

关键测试包括：同名不同 home 实例不串联；实例退出后任务显示需关注；旧进程版本显示未知；任务详情与原 CLI 对应；分页期间新任务到达不丢失定位；关闭页面不影响运行；跨来源请求无权读私聊；所有正文入口到期后不返回旧内容；刷新和重连不创建任务。

现行测试覆盖无 token 和 cookie 访问、旧 cookie 兼容、外源访问、非 loopback 拒绝、重新打开无需登录、只读数据库、即时过期过滤、游标分页、UI 停止不创建或控制运行时，以及跨 home/运行时心跳隔离、陈旧心跳和独立清理。竞态检查覆盖 console 与 observation。

去标识化的临时数据库验收核对 Source、执行尝试及停止的运行实例。这说明页面能追溯已入库任务，不代替真实消息收流或 Desktop 平台验收。

本版本改善主动值守的可观察性及事项闭环的交付核对，不代替消息关联、任务去重和业务验收。


## 9. Agent Workspace browser

The knowledge page is `/workspaces`. Its GET endpoints are `/api/v1/agent-workspaces`, `/{id}/files`, `/{id}/file?path=...`, and `/{id}/history?path=...`. `/files?query=...` searches only current files in that workspace, with a 500-character query cap and at most 100 matches. The underlying storage package rejects traversal, symlinks, internal metadata and oversized content. All HTTP writes are denied; removed memories APIs return 404. Full field definitions and storage guarantees are in the [Workspace details](agent-workspace-design-detail.md).

The local owner selects a workspace explicitly. Owner and group records are never merged into a global result. The list shows path, bytes and update time; selected content shows its digest and safely rendered Markdown. History contains revision metadata, loaded when expanded. Normal search excludes old revisions and migration archives. Source dates/references remain in note content, rather than in a separate card-evidence API.

Markdown uses text nodes and does not execute raw HTML or automatically fetch images. Allowed external link schemes are http, https and mailto. The page shares existing polling cancellation, hidden-tab clearing, disconnection clearing and no-store behavior. SQLite remains read-only for operational queries; workspace GETs read files and do not initialize or migrate storage.

2026-09-23 package validation: console tests and frontend typecheck/build plus UI/Markdown/time tests pass. Browser interaction, final whole-repository integration and real deployment are separate acceptance layers.

### 任务记录补充

2026-09-17 Cyber owner 调整：任务详情提供独立 `communications` 列表；`blocked` 可筛选且计入需关注。后台没有 outbox 时展示“后台完成仅记录结果”，不计为投递失败。主动沟通的 `accepted` 仅表示平台受理，`unknown` 表示结果未知；已确认消息另有 provider message IDs。旧任务版本、已修订/过期/撤回来源对应的沟通正文、理由和回执按核心读取规则隐藏。运行页单列等待回执核对的消息数；Agent 设置按每条路由展示最终人设与权限，避免只显示第一个群的策略。

现有任务查询补充 result_summary，以及每次尝试的 task_version、output_visible 和 artifacts。产物是记录的路径/描述，只显示文本，不新增任意文件读取接口。尝试摘要、产物和终端输出只在来源可用且对应当前任务版本时显示；历史尝试保留状态、时间、模型及错误码。最终结果和列表摘要同样核对来源及版本。

页面按原始请求、执行记录、过程输出、处理结果、交付状态组织。过程输出复用原有 SSE，可选择当前任务版本的各次尝试；旧版本尝试不可选择。用户验收单独明确显示“尚无可核对的用户验收记录”，不创建虚构验收字段或以 completed 替代验收。

### Historical memory-card validation (superseded)

The following records describe the removed memory-card implementation before the Workspace refactor, not current acceptance. Current knowledge behavior is defined by the [Workspace design](agent-workspace-design.md).

当时使用独立临时数据库与合成记录验证记忆查阅和任务交付核对，属于“事项闭环”的展示验收，不代表真实模型执行、平台投递或用户业务验收。不会安装二进制或重启真实服务。

自动化覆盖中文正文/摘要搜索、字面通配字符、组合过滤、稳定分页和准确计数、工作区隔离、非法参数、只读查询、来源过期/撤回/隐藏后的详情与历史过滤，以及任务版本和请求修订变化后产物、结果与终端访问失效。


2026-09-16 本次验证结果：

- `make check`（格式、vet、全量 Go 测试）通过；`node --test tests/console-markdown.test.mjs` 的 3 项安全与阅读测试通过。Node 仅用于开发测试，不是用户运行依赖。
- 浏览器使用临时数据库，验证 30 条全局有效记忆的 24/6 分页、六类卡面、中文搜索、组合筛选、显式跨工作区、空结果和退役提示；点击来源、历史版本、Enter 打开和 Esc 返回焦点均通过。
- 最终独立验收二进制构建成功，并验证来源加载重绘后焦点保留在对应展开按钮。
- 390×844 窄屏详情无横向溢出，完整正文正常阅读；注入样例显示为文本，未生成 script、img 或危险协议链接节点。
- 任务页验证执行尝试、记录产物、只读终端输出、最终结果、交付与未记录验收的独立呈现；运行和设置只读页面回归正常。
- 暂停临时测试服务后，浏览器超时清空任务和过程输出；恢复测试服务后重新读取成功。没有测试真实外部投递或真实用户验收，没有安装或重启用户现有服务。

## 本机免登录变更验证

已移除 token 与登录 cookie 验证；`make check`、Markdown 测试及 `make build` 覆盖无 cookie 查询、安装程序替换后重启、原端口及前台 PID 保持，以及任务来源、版本与工作区边界。该验证不涉及真实平台发送或部署环境验收。

页面路径、侧栏重启、相对时间、React/TypeScript 构建与热更新由前端测试、Go 接口测试和隔离的浏览器验收覆盖。本机数据、PID、构建指纹和真实运行记录不进入公开仓库。
