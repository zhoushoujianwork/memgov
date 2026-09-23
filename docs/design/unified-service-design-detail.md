# 统一本地服务实现与验证

用户范围以[主文档](unified-service-design.md)为准。

## 实现

`internal/service` 管理进程锁、代际停止请求、心跳观察与模块监督。锁以规范化数据目录隔离；停止请求匹配服务 UUID，不按名称搜索或向可能复用的 PID 发信号。观察文件可丢弃，SQLite `state.db` 仍是配置与任务的唯一真相源。

CLI 为启用的数据源、已管理应用及机器人通道生成模块。数据源负责个人 DWS 接收；同一应用机器人通道只生成一个接收模块，Agent 的 `ExternalReceiver` 禁用其自身接收循环。采集、Agent 和 HTTP 管理台在 goroutine 中运行；DWS CLI 和 Claude 等外部工具仍可能产生子进程。

监督器等待旧模块退出后替换，避免同一模块不同代际重叠。声明应用版本变化触发重新加载；自动发现路由及控制状态更新不触发无意义重启。普通失败退避重试，权限、输入和所有权冲突阻塞该模块。退出会取消全部模块并等待清理。

迁移通过当前数据目录的数据库控制状态停止旧模块，以该目录的有效运行心跳和接收租约确认退出。暂停状态在迁移后恢复；等待超时不会另起统一服务。兼容入口在统一服务持锁时拒绝另外启动接收者；独立管理台仍可作为诊断入口使用。

管理台 meta 返回服务观察，运行页显示单一服务及应用模块，不将数据库中的内部运行记录当作独立进程。配置禁用的应用不会启动；启用模块在服务启动时运行，模块显式停止后不会被监督器立即拉起，恢复控制可重新启动。

### macOS 系统托管

`service install` 在当前用户的 `~/Library/LaunchAgents/` 写入私有 plist，通过 `launchctl bootstrap gui/<uid>` 启动前台子命令 `service run`。Label 使用规范化数据目录的摘要隔离实例。安装固定二进制绝对路径、数据目录、配置路径、工作目录及端口；只保存 HOME 和 PATH，不复制调用者的秘密环境变量。依赖 `env://` 的凭据需另行配置 launchd 环境，或改用现有 file/keychain 引用；Claude profile 继续读取既有 zsh alias。

`RunAtLoad` 与 `KeepAlive` 覆盖登录启动和进程退出恢复，`ThrottleInterval=5` 限制失败重试频率。`service stop` 先 disable 再 bootout，防止停止后被拉起，禁用跨登录保留；start 再 enable/bootstrap，restart 停止旧代际后启动。launchd 的 bootout 可能异步完成；start 对 bootstrap 使用每次 1 秒、最长 15 秒的有界重试（若调用方超时更短则随调用方截止时间结束），并在重试间重新检查 job 状态；状态检查本身最多等待 2 秒，超时返回 `unavailable`。卸载保留数据与日志。安装与启动命令等待服务心跳，返回实际模块状态；接受 bootstrap 不等于已就绪。系统退出宽限为 30 秒，超时由 launchd 结束进程；运行时原有任务与租约恢复规则继续适用。

托管进程每秒检测安装路径的文件身份、大小与修改时间；连续两次观察稳定、具有执行权限且能解析为本机可执行文件格式后，取消模块并退出，由 launchd 从原路径启动新程序。路径缺失、空文件、无执行权限和无法解析的文件不触发替换。文件格式检查不等于版本兼容性保证；损坏程序或不受支持的数据库仍需人工修复。构建先生成 `.new` 再原子改名，构建失败不覆盖现有程序。前台模式不启用自动更新检测。

托管入口 `service run` 在监督器进程锁内、任何模块启动前调用 `PrepareServiceDatabase`。已达到目标 Schema 时不重复备份；旧版本先验证完整迁移账本，再取得数据库独占锁并复核版本，通过 `VACUUM INTO` 创建一致性副本，校验完整性、外键、权威库角色和旧版本迁移指纹，fsync 后发布到 `<home>/backups/service-upgrades/v<旧>-to-v<新>-<UUID>.db`（0600）。只有备份成功才在事务中执行二进制内置迁移，随后释放独占锁并正常打开服务。缺库、未知/过新版本和异常账本拒绝升级；其他客户端未退出时等待数据库锁至期限失败，下次启动重试。迁移失败回滚并保留备份与路径诊断；不会自动应用 YAML 或扩大已声明权限。

升级前副本保留旧 Schema，单独存放以免混入当前版本的 `backup list`。不能直接用新程序的 `backup restore` 恢复旧 Schema；需保留原副本，复制后使用对应版本检查，或先对副本执行 `init` 升级、验证后再按常规恢复。自动备份目前不自动清理。启动预检失败使用包含实际原因的错误输出，模块运行后的日志仍按现有脱敏规则处理。

模块日志沿用现有受管理 JSONL；启动及标准输出位于 `<home>/runtime/service/launchd.log`，该启动日志目前不自动轮转。SQLite 统一写入口在排队、取得写事务或事务执行任一阶段达到 250ms 时向该标准日志记录 `command`、`queue_ms`、`begin_ms`、`transaction_ms`、结果和错误码；不记录路径、消息正文或输入载荷。该指标先用于识别具体慢业务，再决定是否将历史导入、保留清理或派生状态更新进一步分批，不能据此延迟平台消息的持久化 ACK。来源绑定 Runtime 每轮仍在 SQLite 只读快照中核验来源、发现凭证和处理范围；范围未变化时不再取得写锁，发生变化时才在写事务内重新核验并更新，避免高频空写阻塞回执和租约续期。状态命令额外返回 manager 的 installed、loaded、label、path 与 log_path。macOS 用户会话托管不提供睡眠唤醒、注销后值守、Linux systemd 或进程无响应的健康探测。


Schema 27 Workspace upgrade: stop the old service and run `memgov --config /path/to/config.yaml config migrate-workspaces` first. The service and explicit `init` path both verify a full old database/knowledge archive before destructive migration. Old AgentHome notes are archived and new workspaces start empty; configuration conversion does not apply runtime state. See [Workspace migration](agent-workspace-design-detail.md#destructive-migration).

### Web 重启

统一服务向管理台注入重启回调；独立 `ui` 不注入。`POST /api/v1/service/restart` 沿用本机访问、同源 JSON 与 `X-Memgov-Console` 校验，先检查安装路径仍可执行，并拒绝重复请求。响应发出后取消监督器，等待模块、数据库连接、监听端口及进程锁释放，再以 `exec` 在原前台进程加载安装路径的程序。沿用启动参数并固定实际端口（包括首次 `--port 0` 选出的端口），不新建后台进程。

管理台不再要求 token 或 cookie；替换进程无需恢复登录会话，原地址可直接重新查询。启动时清除旧版本可能传入的会话环境变量，不传给 Agent 子进程。Web 以新服务 UUID 和运行状态确认重启完成；等待期间禁用按钮并快速重连，超过两分钟提示核对终端及 `service status`。安装程序在确认后被删除、损坏或与数据库不兼容时，重启可能失败，需要终端修复；页面不会把请求已接收当作成功。

## 验证

自动化检查覆盖同目录互斥、跨目录停止隔离、代际替换等待、模块禁用、暂停与恢复、显式停止、权限失败隔离和过期停止请求。CLI 检查接收器生成、禁用应用不启动、自动路由版本不重启与本人私聊绑定默认值；现有运行时和钉钉模拟接收测试继续覆盖消息路由及身份边界。

Web 重启检查覆盖无需登录、非同源、缺少控制请求头、非法 JSON、重复请求及响应先于关闭；隔离测试运行仅含管理台的统一服务，替换安装文件后通过重启接口核对新版本与构建指纹、服务 UUID 更新、原前台 PID、原端口及无 cookie 仍可查询。该验证不操作实际钉钉服务，也不代表真实平台业务验收。

自动化测试不是钉钉真实业务验收。安装及现场运行结果以交付说明为准。

### 系统托管与数据库升级验证

`make check` 与定向 service/CLI 竞态检查覆盖 LaunchAgent 安装、退出恢复、程序替换、停止与恢复。显式启用的 `MEMGOV_TEST_LAUNCHD=1` 隔离测试操作临时实例并自动清理。数据库测试覆盖升级前一致性备份、迁移失败回滚、活动客户端互斥和不兼容版本拒绝。真实安装、PID、本机路径与业务数据验收保留在部署者环境，不进入公开仓库。
