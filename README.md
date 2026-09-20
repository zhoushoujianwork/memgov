# memgov Owner Assistant

面向 DWS Owner 的托管式 Owner Assistant 平台。托管机器人接收 Owner 私聊和 DWS 主动值守事项，组织消息、历史、本机环境与受治理记忆，直接处理或派发 Agent，执行已授权工作，并通过消息反馈结果。Source、Candidate、Review、Memory 是这套服务的长期记忆与证据底座；其他 Agent 可通过 `memgov-memory` skill 接入同一套治理能力。

**私聊机器人是日常入口，CLI 和本地管理台是运维入口，SQLite 是唯一真相源。** 新库位于 `~/.memgov/state.db`。记忆核心不直接调用模型；值守运行时通过外部 Claude CLI 执行模型任务；Go 二进制不保存模型 API Key，不包含通用 Skill 执行器。`memgov ui` 在本机提供任务、记忆、运行和配置审计页面，统一服务另提供重启与失败任务继续入口；使用说明见[本地管理台](docs/guides/local-console-user-guide.md)。已安装版本的能力请以 `memgov ui --help` 为准。


macOS 长期运行使用唯一活动配置 `memgov service install --config ~/.memgov/config.yaml`，由系统托管 Owner 私聊、DWS 主动值守、群挂载 Jarvis 和管理台；登录后启动，进程退出或二进制替换后自动恢复。前台调试仍可使用 `memgov service start`。`config.dual.yaml` 只作为迁移备份，不再作为运行入口。参见[统一本地服务](docs/design/unified-service-design.md)。
当前使用四类概念：**来源 Source** 保存工作材料与证据，**候选 Candidate** 表达新建或修订建议，**复核 Review** 记录核对结果，**正式记忆 Memory** 用于日常召回。记忆有版本和历史，可以表达事实、偏好、约束、决定、操作方法或经验教训。

“记忆原子、卡片、战斗、三轴”只属于[旧版存档](docs/archive/decisions/README.md)，不再是现行模型。管理台的“记忆卡片”仅是现行 Memory 的展示方式。Markdown/JSON 用于交换，`index rebuild` 只重建检索索引，不能代替数据库备份。

正式记忆重点记录：做了什么能力和改动、为什么这样做、明确决定接下来做什么，以及可复用的操作和验证方法。“已推送”“分支领先一个提交”“部署中”等短期状态通常只留在来源证据中，不单独生成记忆或触发修订。具体缺陷、重要限制和必要的验证边界仍应保留；用户明确要求记录历史里程碑时可按该范围保存。版本历史用于追溯知识变化。

## 构建与使用

后续产品设计与业务验收统一参照[最佳落地场景与对齐标准](docs/architecture/best-practice-scenarios.md)：主动接住工作、群内复用专用 Agent、让事项闭环产生可信经验。该标准约定目标，不代表全部能力已验收。

普通使用者可从 GitHub Releases 下载与系统架构匹配的压缩包，校验 `SHA256SUMS` 后安装；完整步骤见 [INSTALL.md](INSTALL.md)。从源码构建：

```bash
make install
.memgov/bin/memgov init
.memgov/bin/memgov workspace add my-project --path "$PWD"
.memgov/bin/memgov config show
.memgov/bin/memgov doctor
```

开发需要 Go（版本见 `go.mod`）、Node 22.16+ 和 npm；发布包内的 `memgov` 可直接运行，无须 Go、Node 或 Python。前端源码位于 `web/`，已有本地服务运行后用 `make web-dev` 启动 React/TypeScript 热更新页面，详见 [Web 开发说明](web/README.md)。后续示例假设 `memgov` 已在 PATH 中。

更新源码后，`make build` 或 `make install` 会先构建临时文件，再原子替换 `.memgov/bin/memgov`。从该路径安装的系统托管服务会自动加载新程序；前台服务仍使用管理台的“重启服务”。从其他路径启动的服务需更新其对应二进制。仅刷新浏览器不会更新服务内嵌的页面。

管理台左下角展示运行版本与本机同步状态，并可对照 GitHub 最新版本 tag 检查更新（包含预发布 tag，不依赖 Release）。公开仓库使用匿名 GitHub API；私有 fork 可复用已有 `gh` 登录。无 tag 或检查失败会明确提示。

```bash
# 保存工作材料
printf '%s' '{"uri":"task://release-42","kind":"note","content":"测试环境发布前已检查权限；生产环境尚未验证。"}' \
  | memgov source ingest --input - --idempotency-key release-42

memgov recall '发布 权限' --limit 10 --budget-chars 4000 --explain
memgov memory list --human
memgov memory show MEMORY_ID
memgov memory history MEMORY_ID
```

`MEMORY_ID` 替换为列表或召回返回的 ID。

## 命令

`memory` 可简写为 `m`，例如 `memgov m list --limit 20`、`memgov m show MEMORY_ID`；与完整命令使用相同参数和行为。

| 命令组 | 功能 |
| --- | --- |
| `init / config show,validate,apply / workspace add,list / doctor` | 初始化、YAML 系统与机器人配置、作用域与完整性检查 |
| `source ingest,show,list` | 不可变来源快照、片段与去重 |
| `candidate submit,show,list,reviews,validate,diff,apply,reject` | 新建或修订建议；显式录入可由调用方审阅后应用 |
| `memory list,show,history,diff,retire,restore` | 正式记忆、版本、生命周期 |
| `search / recall` | 搜索记忆及证据；召回有效 active 记忆 |
| `memory merge preview,apply,undo / links / graph` | 事务内合并、撤销及 JSON/Mermaid 关系 |
| `memory purge preview,apply,status` | 清除关联正文副本，保留墓碑 |
| `backup create,list,verify,restore / export / index rebuild` | 一致性备份、完整交换包与派生索引重建 |
| `agent preset enable,status,sync,disable` | 管理带独立 Git 历史的 Claude 运行规则 |
| `channel add,route,probe,pull,run / message / audience / outbox` | 钉钉通道、增量消息、受众和投递队列 |
| `service install,uninstall,start,status,stop,restart` | 统一服务及 macOS 系统托管 |
| `data-source status,start,pause,resume,stop / data-source history` | 独立采集、覆盖与历史导入；参数以帮助为准 |
| `runtime setup,configure,start,restart,status,pause,resume,stop` | 自动接入并持续运行钉钉 AI 值守 |
| `runtime task list,show,cancel,retry,resume,confirm / runtime logs list,show,follow` | 值守任务、直聊确认和独立日志 |
| `ui --port PORT --open` | 本地管理台：记忆卡片、任务执行记录与运行、Agent 声明编辑和删除 |
| `version / completion` | 版本与 Shell 补全 |

全局参数：`--home`、`--workspace`、`--config`、`--format`、`--timeout`、`--input 文件或-`、`--idempotency-key`、`--actor`、`--expected-version`。用 `memgov version` 确认正在运行的版本，用 `memgov COMMAND --help` 查询该版本的具体参数。

[配置示例](config.local.yaml.example) 支持系统默认值、Owner Assistant、DWS 与钉钉应用机器人。默认读取 `~/.memgov/config.yaml`；仓库内 `config.local.yaml` 仅作为开发入口链接，不复制另一份运行配置。通道通过 `config apply NAME` 显式入库，详见[配置说明](docs/guides/initialization.md)。

默认 stdout 为 JSON envelope：`schema_version / request_id / ok / data / error`，诊断写 stderr。退出码：0 成功，1 内部错误，2 输入错误，3 冲突，4 未找到，5 暂时不可用/超时，6 被规则拒绝。帮助和补全输出对应的文本。

## 数据保护与交换

修改通过候选或治理操作追加新版本。更新必须绑定期望版本；应用候选或合并/清除计划必须绑定预览指纹。墓碑防止同内容、同来源再次进入，并在备份恢复时重新执行。

```bash
memgov backup create --idempotency-key before-upgrade
memgov backup verify /path/to/backup.db
memgov backup restore /path/to/backup.db --expected-digest SHA256
memgov export > exchange-envelope.json
memgov export --format markdown > memories.md
memgov index rebuild
```

JSON 包含来源、片段、正式记忆、历史、操作、候选、复核、任务和墓碑；FTS 与响应缓存不在交换包中。可直接恢复的格式是 SQLite 备份。Markdown 用于阅读。

敏感清除会列出共享证据带来的关联影响，清除数据库正文、历史、候选、任务载荷和缓存，并整理 SQLite。旧文件、备份、已有导出及其他外部副本需分别处理，不宣称整机物理擦除。详见 [治理与恢复](docs/guides/governance.md)。

## 验证与发布

```bash
make check
make test-scenarios test-scenario1-acceptance
go test -race ./internal/core ./internal/cli
make release VERSION=2.0.0-rc1
```

发布脚本在 `dist/` 生成 macOS/Linux × arm64/amd64 单二进制压缩包、安装说明、配置示例和 SHA256SUMS，不上传或部署。CI 使用冻结输出验证流水线，不调用真实模型。Actions 用法依据 [checkout](https://github.com/actions/checkout) 和 [setup-go](https://github.com/actions/setup-go) 官方文档。

原有 `scenario-driver` 与场景测试保留为独立采集工具及测试基础。离线框架回放、模拟 dws 验收和真实业务验收分别报告，详见 [场景测试说明](docs/guides/testing-scenario1.md)。

实现与验收状态见 [交付状态](docs/implementation-status.md)。

文档总入口与维护标准见 [docs/README](docs/README.md)，后续顺序见[实施路线](docs/roadmap.md)。

更多说明：[初始化](docs/guides/initialization.md) · [架构](docs/architecture/architecture.md) · [常见问题](docs/architecture/positioning.md#检索定位) · [CLI 协议](docs/reference/cli-contract.md) · [旧设计存档](docs/archive/decisions/README.md)。

钉钉后台观察和 Owner 私聊统一进入 Owner Assistant 根任务；简单事项直接回答，复杂事项可派发 `owner-executor`、`researcher`、`verifier` 或 `communicator`，结果、阻塞和确认请求按通知策略回到 Owner。历史 `record_only` 任务继续兼容。群内挂载的另一个 Jarvis 保持原有机器人、群路由、preset、技能、工具、记忆可见范围和回复通道，不因本次重构停用或削减；它不会自动继承 Owner 私聊历史或 Owner 私有授权。详见[接入主设计](docs/design/dingtalk-integration-design.md)、[Owner Assistant 主设计](docs/design/owner-assistant-design.md)、[运行时设计](docs/design/agent-runtime-design.md)和[用户手册](docs/guides/runtime-user-guide.md)。本轮源码实现、离线验证和真实平台验收分别记录在[交付状态](docs/implementation-status.md)中。

运行日志位于 `<MEMGOV_HOME>/runtime/logs/<runtime-id>/`，同时以 NDJSON 输出到 stdout。结构化运行日志不保存聊天原文、完整模型输入输出或凭据；独立的只读过程输出保存经过脱敏的公开文字与工具事件，按来源可用性控制展示，见[实时终端](docs/design/task-terminal.md)。首期代码与离线故障测试已完成，真实企业账号的权限、收发和业务任务仍需在部署环境验收。

## Agent Skill

仓库提供正式的 [memgov-memory](.agents/skills/memgov-memory/SKILL.md) 跨 Agent 接入协议。获准的 Agent 可以通过稳定 CLI/API 契约查询和读取 Source，创建与修订 Candidate，提交与读取 Review，应用、修订、退休和恢复 Memory，并查看证据、版本和操作历史；每次操作记录 Agent 身份、workspace、actor、request ID、幂等键和证据。Skill 的“全部权限”只指 memgov 数据治理能力，不自动授予本机 shell、DWS 发消息、云平台或生产权限；这些仍由宿主 Agent 的 capability 配置决定。普通对话仍不会自动写入长期记忆，写入需有明确任务授权并经过证据与复核流程。

技能唯一维护副本在 `.agents/skills/`。仓库根的 `CLAUDE.md` 是指向 `AGENTS.md` 的符号链接，`.claude/skills` 是指向 `.agents/skills` 的符号链接，Claude Code 在本仓库内可直接发现并加载该技能，无需额外安装。

在本仓库外（例如另一个项目目录或直聊会话）使用 Claude Code 或 Codex 时，链接到各自的用户级技能目录，保留仓库版本作为唯一维护副本：

```bash
ln -s "$PWD/.agents/skills/memgov-memory" "${HOME}/.claude/skills/memgov-memory"

mkdir -p "${CODEX_HOME:-$HOME/.codex}/skills"
ln -s "$PWD/.agents/skills/memgov-memory" "${CODEX_HOME:-$HOME/.codex}/skills/memgov-memory"
```

安装后可用 `$memgov-memory` 显式调用；其元数据也允许在符合描述的任务中自动触发。

## License

memgov is available under the [Apache License 2.0](LICENSE).
