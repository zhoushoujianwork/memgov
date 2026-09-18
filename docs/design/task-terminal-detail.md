# 任务实时终端的实现与验证

用户范围以[主文档](task-terminal.md)为准。

## 输出采集

运行时将真正的尝试 ID 与原生 Claude 会话 ID 分开传递，避免 resume 或私聊进程复用把输出写到旧任务。`Claude.Execute` 为每次尝试建立独立临时记录，结束时关闭记录；分析、记忆复核和单独确认动作不接入此过程流。

本人私聊在现有 stream-json 读取器中观察公开事件，继续保留原来的结果解析、工具分类和原生进程复用。非私聊执行改用 `--output-format stream-json --verbose`，逐行采集并取最终 result 信封，继续按原 structured_output 合约解析和计费；不改变权限参数或模型调用次数。能力依据本地 `claude --help` 及[Claude 流式输出文档](https://code.claude.com/docs/en/headless)。按消息和工具事件更新，不启用隐藏推理或逐 token 的部分消息采集。

只保存 system/init 的模型状态、assistant 的 text/tool_use、user 中的 tool_result 和 result；原始 user 提示、system 配置、thinking 块、原生会话文件和环境变量不入流。工具文本及 CLI stderr 经过常见凭据、Bearer、令牌及链接脱敏并去掉终端控制序列。CLI stderr 为进程诊断，私聊复用时按当前活动尝试采集，不作为工具成功或任务完成的凭据。

`runtime/output/<runtime-id>/<task-id>/<attempt-id>.jsonl` 与 SQLite、结构化 runlog 分开。目录 0700、文件 0600，身份路径不能含分隔符，拒绝静态符号链接。每条最多 8000 字符，单尝试 4 MiB，达到上限写入提示后停止采集而继续执行。读取最多最近 256 KiB，只返回完整 JSONL 行，游标为记录末尾字节偏移。日志保留 30 天，创建记录时清理旧文件并维护 128 MiB 总量目标；最近一小时的文件保留，因此总量清理是尽力执行。读取时也检查到期，不等待物理清理。

## 实时接口与页面

本机 loopback 同源访问 `GET /api/v1/tasks/<id>/terminal?attempt=<attempt-id>`。SSE output 事件携带时间、分类、脱敏正文及偏移 ID；status/finished 根据 SQLite 中的尝试状态发送，支持 Last-Event-ID 或 cursor 恢复读取。运行期间每秒进行短读取，不持有长期 SQLite 快照；每轮重查任务版本、原文可见性和私聊 clear 屏障。失效时发送 invalidated 并断流，页面清除过程内容。写入设 5 秒期限，连接关闭只结束读取。

页面使用本地模块和终端样式窗口，无外部 CDN、PTY、Shell 输入或键盘执行接口。所有内容以 textContent 显示，脚本和 ANSI 不解释执行。每次尝试隔离，最多保留 500 条/256 Ki 字符，支持暂停跟随、滚动位置保留、收起及切换尝试。网络错误立即清除内容，三秒后重连读取保留的尾部；页面隐藏、切换任务或退出时关闭连接。日志区域不把过程事件当作结果验收或送达状态。

## 验证

- 本地假 Claude 子进程在最终结果之前输出文字、工具及 stderr；测试确认这些内容可立即读取，凭据被隐藏，最终 structured_output 与用量合约不变。
- 取消调用保留已收到的过程，不阻塞退出；同一私聊原生进程两轮执行的输出分别归属各自尝试。
- 覆盖并发写入、单尝试上限、完整行游标、静态路径与符号链接拒绝、到期不可见、SSE 实时追加与读取期间撤回，及当时的会话、同源和尝试归属检查（免登录变更后的入口边界见[管理台详细稿](local-console-design-detail.md#5-本地访问与正文生命周期)）。
- 离线浏览器点按验证自动展开、工具内容以文本展示、实时追加、自动滚动、暂停位置保持及历史尝试切换；只启动本地测试控制台，不调用真实模型或钉钉。

全仓库 `make check` 与过程流相关 `go test -race` 通过；真实安装、真实数据库及平台任务未修改。本次验证对应进度可见与验收可核对，不代表真实业务交付验收完成。
