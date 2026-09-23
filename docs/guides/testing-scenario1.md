# 场景 1：通过 dws 了解与某人的近期沟通事项

本阶段已提供独立的只读 `cmd/scenario-driver`：调用 dws、解析响应、查找联系人和读取消息，
按有限规则提取带证据的事项。它不读取测试答案，也不写入核心记忆库。
“正在发送的事情”在本场景中解释为聊天记录中正在推进的事项；实时消息监听属于后续独立场景。

## 执行方式与通过含义

```bash
make test-scenarios
make check
make build

# 默认自动构建仓库内驱动并执行离线验收。
make test-scenario1-acceptance
make build-scenario-driver

# 可选：指定其他驱动。路径必须绝对，不支持附加命令参数。
MEMGOV_SCENARIO_DRIVER=/absolute/path/to/scenario-adapter make test-scenario1-acceptance
```

默认测试不要求安装 dws、登录钉钉或提供真实联系人。`TestFrameworkReplay` 使用刻意读取
期望结果的回放驱动，只验证框架能正确传递输入、模拟 CLI、记录调用和判断结果，**不能证明业务功能已实现**。
`TestFrameworkRejectsBrokenDrivers` 验证少调用、多调用、错误命令、异常退出、无效 JSON、多余输出、
错误状态、无来源证据、沿用过期状态和错误完整性都会失败。

`TestScenario1Acceptance` 使用真实业务驱动。未设置环境变量时自动构建本仓库的
`cmd/scenario-driver`，也包含在 `go test ./...` 中，不再跳过。设置 `MEMGOV_SCENARIO_DRIVER`
时改用该绝对路径；普通测试仍通过假的 dws 执行，不访问线上。

## 框架协议

框架为每个用例建立独立临时目录、HOME 和 PATH，将一个假的 `dws` 放在 PATH 首位。
驱动必须通过 PATH 调用 `dws`，逐条执行命令，不能使用绝对 dws 路径、网络请求或读取 fixture。
这是一项测试约定，临时环境不是操作系统安全沙箱。
测试夹具路径环境变量仅供假 CLI 与框架自测使用，不属于业务驱动输入协议。

驱动从 stdin 读取一个 JSON 对象：

```json
{"person":"测试同事","start":"2026-09-07T00:00:00+08:00","end":"2026-09-14T00:00:00+08:00"}
```

stdout 只能输出一个结果对象，诊断写 stderr。预期业务错误也返回结构化结果并以 0 退出；
驱动崩溃以非零退出。每例有 10 秒总超时。测试不会将 stderr 或聊天正文打印到失败报告。

可选 `profile` 字段会将读取固定到一个 dws 账号。它必须精确等于
`dws profile list --format json` 返回的 `profiles[].profile`，例如
`{"profile":"fixture-corp",...}`。找不到唯一匹配时返回
`identity_context_unavailable`，不会继续查询联系人或聊天。

```json
{
  "status":"ok",
  "self_id":"fixture-self",
  "person_id":"fixture-peer",
  "complete":true,
  "findings":[
    {"topic":"接口联调","status":"in_progress","evidence":["fixture-m2","fixture-m3"]},
    {"topic":"上线时间","status":"pending_confirmation","evidence":["fixture-m4"]}
  ],
  "candidates":null
}
```

事实按 topic 比较，不要求自然语言措辞一致；事项数量、状态、证据 ID 必须正确，不允许额外虚构事项。
证据列表按用例指定的时间顺序输出。同名候选按搜索顺序返回。无事项使用 null 或空数组。
`complete` 仅表示请求时间窗内的消息读取完整性，不能解释为已知某人所有工作。

假 CLI 逐项匹配真实 argv（包括参数顺序），返回该步 fixture JSON 与退出码；未知命令直接失败并记录。
验收同时检查调用日志，因此即使驱动吞掉错误，额外发送消息或跳过查询仍会失败。

## 用例

| 用例 | 验收重点 |
|---|---|
| happy_path_latest_state | 本人 → 唯一联系人 → 双向聊天；较新消息解除旧阻塞，联调为进行中，上线时间待确认 |
| ambiguous_person | 同名时补查部门详情，返回候选并停止，不擅自选第一个或读取其聊天 |
| person_not_found | 无搜索命中，停止，不拼造 userId |
| no_messages | 完整空记录返回 no_messages，不虚构工作状态 |
| auth_required | 本人信息认证失败即停止，不继续查询或自动登录 |
| permission_denied | 聊天读取无权限，不当成空记录 |
| partial_history | 已取得消息仍可提炼，但保留 partial 与 complete=false |
| untrusted_message | 聊天中的指令不触发发送、上传或其他 CLI 调用；无法提炼时返回 needs_analysis |
| invalid_response | JSON null 不伪装成成功或空记录 |
| timeout | CLI 超时独立返回，不静默忽略或无限重试 |

命令路由：`contact user get-self` → `aisearch person --dimension name` →
唯一 userId 的 `chat +chat-messages --user ...`。同名追加 `contact user get --ids ...` 后停止。
读取范围固定为 `[start,end)`，保留双方消息；分页交给 shortcut，限制每页 30 条、最多 5 页/150 条。
所有调用使用 `--format json`，不发送消息，不轮询历史，不自动写入记忆库。

## 当前用户身份缓存

场景驱动每次先执行只读的 `dws profile list --format json`，从唯一的当前 Profile 读取
组织、clientId、登录时间和令牌到期时间。随后将该 Profile 传给全部 dws 查询，避免“当前账号”
在一次联系人或聊天查询中被切换到另一账号。

`contact user get-self` 的结果会缓存 15 分钟，缓存键是 Profile selector、corpId、clientId
与登录/令牌时间组成的 SHA-256 指纹。登录或令牌上下文变化会生成新缓存键，无法使用旧身份。
Profile 不存在、多个候选或缺少指纹字段时，驱动会返回 `identity_context_unavailable`；不会降级为
不带 Profile 的读取。

缓存位于系统用户缓存目录的 `memgov/dws-identity-v1/`，目录权限为 `0700`、文件权限为 `0600`。
文件只保存 userId、缓存时间和 Profile 指纹，不保存 Token、联系人、聊天正文或事项。写入使用临时文件
和原子重命名，因此并发进程最多重复一次 `get-self` 查询，不会覆盖其他 Profile 的身份。

测试覆盖缓存命中、登录上下文变更后重新读取、Profile 作用域传递、文件权限和无效 Profile 拒绝。

## 夹具与后续接入

`tests/scenarios/testdata/scenario1.json` 包含请求、严格命令序列、模拟响应与期望结果。
所有人物、ID、消息都是虚构。命令及参数已依据本机 dws 叶子帮助和产品技能核对；
**响应 JSON 是测试设计用的简化数据，不是经真实服务验证的 wire schema**。
解析器同时支持简化夹具与已观察到的 `success/result` 人员搜索封装、本人信息的 `result[].orgEmployeeModel`，以及 `createTime` 消息时间。
独立单元测试覆盖这两类输入；未知封装会失败，不降级为空记录。当前未对线上所有错误码和
所有产品版本做兼容验证。此阶段不提供自动访问真实账号的测试开关。

## 当前架构与能力边界

`internal/core` retains SQLite operational state and source evidence, while `internal/agentworkspace` stores long-term knowledge files. The scenario driver remains an independent read-only query adapter and does not write workspace knowledge or task state. Any later integration must preserve source references and the current task/audience boundary; the removed candidate/memory commands are not an integration path.

事项提取采用有限中文状态规则（被…阻塞、已恢复进行、正在进行、已完成、还没定）。
唯一后缀可关联已有主题；清除依赖后的“请继续”只说明 ready，后续明确恢复才升级为 in_progress。
规则无法理解的非空对话返回 `needs_analysis`；它不是通用语义摘要模型，也不会自动得出镜像故障根因。
返回 `ok` 仅表示识别到了规则支持的事项，不能代表穷尽所有潜在事项。

`complete` 保守合并完整性、更多分页、截断、partial 和失败计数。时间范围在驱动内再次过滤，
按时间排序后更新状态；非法消息时间或缺少消息 ID 返回错误。
查询 stdout 只输出状态、身份和规则命中的消息引用，不包含原始聊天正文；外部命令 stderr 不回显。

手动真实查询可将请求 JSON 输入 `.memgov/bin/scenario-driver`。它使用当前 dws 登录账号，
需要已安装且已登录的 dws；与离线测试隔离的 HOME/PATH 不同。请按需要明确联系人和时间范围。
后续扩展语义模型时，应保持证据校验并补充模型评估，而不是用固定答案替换提取逻辑。

2026-09-14 手动联调：独立驱动完成本人信息、陈若霞唯一身份解析及当天聊天读取，
结果为 `needs_analysis`、`complete=true`。这验证了真实读取链路，不代表规则引擎已经理解镜像地址/tag 对话。
没有把真实聊天正文或个人信息写入测试夹具。
