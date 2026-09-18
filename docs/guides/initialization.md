# 初始化与配置

```bash
make install
.memgov/bin/memgov init
.memgov/bin/memgov workspace add project-a --path /absolute/project-a
```

新权威库是 `~/.memgov/state.db`，与旧 `memgov.db` 分开。init 可重复执行；发现旧模型表或未知 Schema 时拒绝覆盖。不要用旧数据库改名冒充新库。

已安装 macOS 系统托管时，`make install` 替换程序后，服务会自动备份并迁移受支持的旧 Schema，再恢复运行；升级前备份位于 `<数据根>/backups/service-upgrades/`。普通前台命令仍通过 `init` 显式升级。备份失败或版本不兼容时查看 `service status` 给出的启动日志，详见[统一服务](../design/unified-service-design.md)。

数据根优先级：`--home` → `MEMGOV_HOME` → `~/.memgov`。

工作区优先级：`--workspace` → `MEMGOV_WORKSPACE` → 当前目录匹配的最长项目路径 → 配置的 default_workspace → global。显式指定 `--workspace global` 可脱离项目绑定。工作区是本机检索和写入作用域，不是多租户身份认证。

配置文件：`--config` → `MEMGOV_CONFIG` → 数据根下 config.yaml。

仓库内的 `config.local.yaml` 不会自动加载。可以复制 [完整示例](../../config.local.yaml.example) 到 `~/.memgov/config.yaml`，或每次使用 `--config config.local.yaml`。

后台观察使用 delivery: record_only，完成只记录；旧 owner_direct 可解析但归一化为 record_only 并提示迁移，旧待发通知失效。新建默认后台 Agent 具备完整能力和 owner_delegated，已有显式 Agent 限制保留，权限扩张仍需预览和显式应用。机器人推荐使用 applications.bots 按 channel 声明默认人设及 Owner 私聊/群覆盖，兼容旧独立应用声明但拒绝冲突。详见[配置兼容](../design/dingtalk-integration-design-detail.md#完成与配置兼容)。源码实现和离线验证已完成；真实模型与平台业务验收仍由部署者执行。

目前支持以下配置：

| 配置 | 用途与生效时间 |
| --- | --- |
| `default_workspace` | 默认工作区，优先级见上文 |
| `format`、`timeout`、`actor` | 普通命令输出格式、期限和审计身份；命令行参数优先 |
| `logging` | 值守日志保留时间、总大小和单文件大小；下次 `runtime start` 生效 |
| `runtime_setup` | dws profile、机器人 Code、忽略群、项目目录、Claude 配置档与模型、触发阈值；用于新建值守实例 |
| `channels` | dws 个人通道或钉钉应用机器人、密钥引用和会话路由；用 `config apply` 显式写入 SQLite |

示例列出系统配置字段，以及两种通道的常用参数和注释。未知字段、旧配置字段及错误类型会报 `invalid_input`，不会悄悄忽略。`--home` / `MEMGOV_HOME` 仍负责选择数据根，不能在 YAML 中改变。

检查配置，不创建数据库、不连接钉钉：

```bash
memgov --config config.local.yaml config show
memgov --config config.local.yaml config validate
```

`config validate` 检查 YAML 字段、系统参数和通道身份；路由策略、目标工作区与数据库版本在 `config apply` 事务中检查。

配置应用机器人：取消示例中 `app-main` 通道的注释，填写企业 ID、应用 Client ID、Robot Code 和会话 ID，将密钥留在 `credential_ref` 指向的环境变量、文件或系统钥匙串中。

```bash
memgov --config config.local.yaml init
memgov --config config.local.yaml config apply app-main --idempotency-key app-main-v1
memgov channel show app-main
```

`apply` 只做本地注册，接收、探测和发送仍分别通过通道命令执行。使用自定义数据根时，后续命令也需携带相同的 `--home`。发送审批仍只是展示信息，示例路由默认只生成草稿。

已有通道修改后，需要从 `channel show` 获取当前 `config_version`；如 YAML 包含已有路由，也需要该路由的 `version`。停止相关接收进程和值守实例后应用，例如两个版本均为 1 时：

```bash
memgov --config config.local.yaml config apply app-main \
  --expected-version 1 --expected-route-version 1 \
  --reason "更新机器人配置" --idempotency-key app-main-v2
```

更新保留未声明的其他路由；省略 `route` 时不修改路由。不能将已有通道改为其他企业、个人账号或应用身份。变更会清空能力验证结果，并使旧草稿失效；启动接收前重新执行 `channel probe app-main`。版本不匹配或路由无效时整个操作回滚。相同幂等键可重放同一次提交；配置内容变化必须使用新键。

`runtime_setup` 是现有 dws 自动接入流程的默认参数：

```bash
memgov --config config.local.yaml runtime setup my-watcher
memgov --config config.local.yaml runtime start my-watcher
```

命令行参数覆盖 YAML，例如 `--robot-code`、`--analysis-model`、`--pilot=false`；显式 `--ignore` 列表替换 YAML 列表。`pilot: true` 固定使用 1 条 / 30 秒 / 10 秒对账参数。默认值不会修改已存在实例，它们继续通过 `runtime configure` 管理。应用机器人通道使用 `config apply` 注册，不使用 dws 的 `runtime setup` 自动发现流程。

`config show`、`config validate`、`version`、帮助和补全不创建数据库。YAML 是系统默认值和通道配置的输入文件，业务配置的生效版本、记忆、任务和审计仍以 SQLite 为准。普通命令 stdout 只输出结果；值守运行日志另外写入 JSONL 文件。旧配置中的 Skill、store、index、legacy bridge 设置不再使用。

从另一台机器恢复：先选定新数据根并执行 init，再 backup verify 与 backup restore。只复制单个正在使用的 state.db 不构成一致性备份，应使用 backup create。

清理构建结果用 `make clean`，它只移除 `.memgov/bin` 和 `dist`，不会删除记忆数据或原始输入。
