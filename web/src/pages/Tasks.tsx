import type { Runtime, Task, TaskDetail, LogEvent } from "../types";
import {
  Badge,
  Command,
  Empty,
  Heading,
  KV,
  Section,
  Time,
  labels,
  modes,
  processLabels,
} from "../components/common";
import { Terminal } from "../components/Adapters";
export interface TaskFilters {
  status: string;
  runtime: string;
  cursor: string;
  history: string[];
}
export function Tasks({
  runtimes,
  tasks,
  next,
  detail,
  logs,
  filters,
  selected,
  showLogs,
  continuing,
  message,
  onFilter,
  onSelect,
  onPage,
  onLogs,
  onResume,
  onRefresh,
}: {
  runtimes: Runtime[];
  tasks: Task[];
  next: string;
  detail?: TaskDetail;
  logs: LogEvent[];
  filters: TaskFilters;
  selected: string;
  showLogs: boolean;
  continuing: boolean;
  message: string;
  onFilter: (filters: TaskFilters) => void;
  onSelect: (id: string) => void;
  onPage: (direction: number) => void;
  onLogs: () => void;
  onResume: (task: Task) => void;
  onRefresh: () => void;
}) {
  const totals: Record<string, number> = {};
  runtimes.forEach((r) =>
    Object.entries(r.tasks || {}).forEach(([k, v]) => {
      totals[k] = (totals[k] || 0) + v;
    }),
  );
  const unverified = runtimes
    .filter((r) => r.status !== "running" || r.process_state !== "heartbeat")
    .reduce((n, r) => n + (r.tasks?.running || 0), 0);
  const stats: [string, number, string][] = [
    ["运行记录", totals.running || 0, "结合模块心跳判断执行"],
    ["待处理", totals.pending || 0, "等待 Agent 接手"],
    [
      "需要核对",
      [
        "failed",
        "stale",
        "awaiting_confirmation",
        "clarification",
        "blocked",
        "action_failed",
        "action_unknown",
      ].reduce((n, k) => n + (totals[k] || 0), unverified),
      "异常、待确认或执行未验证",
    ],
    ["已完成", totals.completed || 0, "处理结果可追溯"],
  ];
  const update = (key: "status" | "runtime", value: string) =>
    onFilter({ ...filters, [key]: value, cursor: "", history: [] });
  const runtimeGroups = [
    ["direct", "本人私聊 Agent"],
    ["group_mention", "群 Agent"],
    ["proactive", "主动值守 Agent"],
  ] as const;
  const knownModes = new Set<string>(runtimeGroups.map(([mode]) => mode));
  const otherRuntimes = runtimes.filter((r) => !knownModes.has(r.mode));
  return (
    <>
      <Heading title="任务">从原始请求到处理结果，核对每一次执行。</Heading>
      <div className="stats">
        {stats.map(([label, count, note]) => (
          <div className="stat" key={label}>
            <span className="muted">{label}</span>
            <strong>{count}</strong>
            <small>{note}</small>
          </div>
        ))}
      </div>
      <div className="filters">
        <select
          id="status-filter"
          aria-label="按任务状态筛选"
          value={filters.status}
          onChange={(e) => update("status", e.target.value)}
        >
          {[
            ["active", "进行中与需处理"],
            ["all", "全部记录"],
            ["running", "运行记录"],
            ["pending", "待处理"],
            ["failed", "失败"],
            ["stale", "已失效"],
            ["clarification", "待澄清"],
            ["blocked", "处理受阻"],
            ["action_failed", "操作失败"],
            ["action_unknown", "结果未知"],
            ["awaiting_confirmation", "待确认"],
            ["completed", "已完成"],
            ["cancelled", "已取消"],
          ].map(([v, t]) => (
            <option value={v} key={v}>
              {t}
            </option>
          ))}
        </select>
        <select
          id="runtime-filter"
          aria-label="按处理 Agent 筛选"
          value={filters.runtime}
          onChange={(e) => update("runtime", e.target.value)}
        >
          <option value="">全部处理 Agent</option>
          {runtimeGroups.map(([mode, label]) => {
            const items = runtimes.filter((r) => r.mode === mode);
            return items.length ? (
              <optgroup label={label} key={mode}>
                {items.map((r) => (
                  <option value={r.id} key={r.id}>
                    {r.name}
                  </option>
                ))}
              </optgroup>
            ) : null;
          })}
          {!!otherRuntimes.length && (
            <optgroup label="其他处理 Agent">
              {otherRuntimes.map((r) => (
                <option value={r.id} key={r.id}>
                  {r.name}
                </option>
              ))}
            </optgroup>
          )}
        </select>
        <span className="muted count">本页 {tasks.length} 条</span>
        <button className="button quiet" onClick={onRefresh}>
          刷新
        </button>
      </div>
      <div className="task-workspace">
        <div className="task-list">
          {!tasks.length && (
            <Empty>此筛选下暂无任务。新消息入库后会自动刷新。</Empty>
          )}
          {tasks.map((task) => (
            <button
              key={task.id}
              id={`task-${task.id}`}
              className={`task-row ${selected === task.id ? "selected" : ""}`}
              onClick={() => onSelect(task.id)}
            >
              <div className="row-head">
                <strong>{task.title}</strong>
                {task.attention ? (
                  <span className="badge attention">需要关注</span>
                ) : (
                  <Badge status={task.status} />
                )}
              </div>
              <p className="preview">{task.preview || "查看执行记录与来源"}</p>
              <div className="row-foot">
                <span>
                  {modes[task.mode] || task.mode}
                  {task.mode === "direct" ? "轮次" : ""} · {task.runtime_name}
                </span>
                <Time value={task.updated_at} prefix="最近活动 " />
              </div>
              {task.result_summary && (
                <p className="preview result-preview">
                  结果：{task.result_summary}
                </p>
              )}
            </button>
          ))}
          <div className="pagination">
            <button
              className="button quiet"
              disabled={!filters.history.length}
              onClick={() => onPage(-1)}
            >
              上一页
            </button>
            <button
              className="button quiet"
              disabled={!next}
              onClick={() => onPage(1)}
            >
              下一页
            </button>
          </div>
        </div>
        <div className="detail-panel">
          {detail ? (
            <Detail
              key={detail.task.id}
              data={detail}
              logs={logs}
              showLogs={showLogs}
              continuing={continuing}
              message={message}
              onLogs={onLogs}
              onResume={onResume}
            />
          ) : (
            <Empty>
              {selected
                ? "正在读取任务详情…"
                : "选择一条任务，查看请求、执行尝试和结果。"}
            </Empty>
          )}
        </div>
      </div>
    </>
  );
}
function Detail({
  data,
  logs,
  showLogs,
  continuing,
  message,
  onLogs,
  onResume,
}: {
  data: TaskDetail;
  logs: LogEvent[];
  showLogs: boolean;
  continuing: boolean;
  message: string;
  onLogs: () => void;
  onResume: (task: Task) => void;
}) {
  const t = data.task;
  const deliveryLabels: Record<string, string> = {
    accepted: "平台已接受",
    sent: "已发送",
    delivered: "已送达",
    failed: "失败",
    unknown: "结果未知",
    sending: "发送中",
    draft: "草稿",
    prepared: "已准备",
  };
  return (
    <>
      <div className="detail-title">
        <span className="eyebrow">任务详情</span>
        <h2>{t.title}</h2>
        <Badge status={t.status} />
      </div>
      <code className="id">{t.id}</code>
      {t.attention && (
        <div className="notice">
          {t.runtime_status !== "running"
            ? "需要关注：处理 Agent 未处于运行状态，任务记录可能尚未收敛。"
            : "需要关注：请核对任务状态及 Agent 活性，当前信息不能证明任务正在推进。"}
        </div>
      )}
      <Section title="执行状态">
        <KV label="处理 Agent">{t.runtime_name}</KV>
        <KV label="Agent 记录">
          {labels[t.runtime_status] || t.runtime_status}
        </KV>
        <KV label="Agent 活性">{processLabels[t.process_state]}</KV>
        {t.work_phase && <KV label="当前阶段">{t.work_phase}</KV>}
        {t.work_deadline && <KV label="执行截止"><Time value={t.work_deadline} /></KV>}
        {t.work_heartbeat && <KV label="控制心跳（不代表进展）"><Time value={t.work_heartbeat} /></KV>}
        {t.work_phase && <KV label="最近模型输出"><Time value={t.model_activity_at} empty="尚未收到输出" /></KV>}
        <KV label="最近状态变化">
          <Time value={t.updated_at} />
        </KV>
      </Section>
      {t.memory_status && <Section title="记忆沉淀（与业务结果分开）">
        <KV label="状态">{({pending: "等待审查", reviewing: "独立审查中", applied: "已应用", rejected: "候选被拒绝", failed: "沉淀失败"} as Record<string,string>)[t.memory_status] || t.memory_status}</KV>
        {t.memory_error_code && <KV label="原因">{t.memory_error_code}</KV>}
      </Section>}
      {data.can_resume ? (
        <Section title="继续处理">
          <button
            id="resume-task"
            className="button"
            disabled={continuing}
            onClick={() => onResume(t)}
          >
            {continuing ? "正在提交…" : "继续任务"}
          </button>
          <p className="muted">
            {data.resume_mode === "native"
              ? "恢复上次会话，核对已有成果后继续处理原请求。"
              : "旧会话未保存，AI 会结合原请求和已有文件检查进度后继续。"}
          </p>
        </Section>
      ) : (
        t.status === "failed" &&
        data.resume_reason && (
          <p className="notice">当前无法继续：{data.resume_reason}</p>
        )
      )}
      {message && <p className="notice">{message}</p>}
      <Section title="原始请求">
        {data.messages.map((m) => (
          <div key={m.id}>
            <small className="muted">
              <Time value={m.sent_at} /> ·{" "}
              {m.availability === "available" ? "本地来源" : "原文不可用"}
            </small>
            <pre>{m.body || "原文已到期或不可用，可按来源定位回查平台。"}</pre>
            <code className="id">
              {m.source_id ? `Source ${m.source_id}` : `Message ${m.id}`}
            </code>
          </div>
        ))}
        {!data.messages.length && <Empty>未关联消息。</Empty>}
      </Section>
      <Section title="执行记录">
        {data.attempts.map((a, i) => (
          <div className="attempt" key={a.id}>
            <h3>
              第 {i + 1} 次执行 · 任务 v{a.task_version}
            </h3>
            <Badge status={a.status} />
            <KV label="模型">{a.model}</KV>
            <KV label="开始">
              <Time value={a.started_at} />
            </KV>
            <KV label="结束">
              <Time value={a.finished_at} empty="尚未记录" />
            </KV>
            <KV label="配置版本">{a.applied_version}</KV>
            {!!a.tools?.length && (
              <KV label="使用工具">{a.tools.join(" · ")}</KV>
            )}
            {a.summary && <pre>{a.summary}</pre>}
            {a.error_code && <KV label="错误">{a.error_code}</KV>}
            {!a.output_visible ? (
              <p className="muted">
                来源或任务版本已变化，此次输出与产物不再展示。
              </p>
            ) : a.artifacts?.length ? (
              <>
                <h3>记录的产物</h3>
                {a.artifacts.map((artifact, j) => (
                  <p key={j} className="artifact-reference">
                    {artifact}
                  </p>
                ))}
              </>
            ) : (
              <p className="muted">未记录产物。</p>
            )}
          </div>
        ))}
        {!data.attempts.length && <Empty>尚无执行尝试。</Empty>}
      </Section>
      <Terminal detail={data} />
      <Section title="处理结果">
        <pre>
          {data.result ||
            (t.redacted || !data.can_view_output
              ? "关联原文或请求已失效，旧结果已隐藏。"
              : "尚无当前任务版本的处理结果。")}
        </pre>
      </Section>
      <Section title="交付状态">
        {!data.deliveries?.length && (
          <p className="muted">
            {t.mode === "proactive"
              ? "后台完成仅记录结果；Agent 自主发送的消息单独列在沟通记录中。"
              : "尚无可核对的投递记录，处理完成不代表已经送达。"}
          </p>
        )}
        {data.deliveries?.map((d) => (
          <KV label={d.id.slice(0, 8)} key={d.id}>
            {d.purpose === "runtime_receipt"
              ? "接收回执"
              : d.purpose === "runtime_processing_receipt"
                ? "处理中标记"
              : d.purpose === "runtime_completion_receipt"
                ? "完成标记"
              : d.purpose === "runtime_failure_receipt"
                ? "失败回执"
              : d.purpose === "confirmation"
                ? "私聊确认"
                : d.transport === "bot_group"
                  ? "群聊答复"
                  : "答复"}{" "}
            · {deliveryLabels[d.state] || d.state} ·{" "}
            <Time value={d.updated_at} />
          </KV>
        ))}
      </Section>
      {(t.mode === "proactive" || !!data.communications?.length) && (
        <Section title="Agent 沟通记录">
          {!data.communications?.length && (
            <p className="muted">
              未记录主动发送，无需沟通也可以完成后台消费。
            </p>
          )}
          {data.communications?.map((action) => (
            <div className="attempt" key={action.id}>
              <KV label={action.target_type === "group" ? "目标群" : "联系人"}>
                {action.target_id}
              </KV>
              <KV label="发送身份">
                DWS owner · {action.owner_user_id} · {action.owner_profile}
              </KV>
              <KV label="发送状态">
                {deliveryLabels[action.state] || action.state} ·{" "}
                <Time value={action.updated_at} />
              </KV>
              {action.reason && <p>{action.reason}</p>}
              {action.content && <pre>{action.content}</pre>}
            </div>
          ))}
        </Section>
      )}
      <Section title="用户验收">
        <p className="muted">
          尚无可核对的用户验收记录。处理完成或平台送达均不代表已通过验收。
        </p>
      </Section>
      {!!data.actions.length && (
        <Section title="相关操作">
          {data.actions.map((a, i) => (
            <div key={i}><KV label={a.kind}>
              {labels[a.status] || a.status}
            </KV>
            {a.target && <KV label="具体目标">{a.target}</KV>}
            {a.payload && <pre>{a.payload}</pre>}
            {a.confirmation_token && <><p className="muted">核对目标、影响及恢复方式后，在已绑定的 Owner 私聊中发送以下完整确认。历史消息或引用不能授权。</p><pre>{a.confirmation_token}</pre></>}
            </div>
          ))}
          <small className="muted">
            {t.mode === "proactive"
              ? "未执行操作保留原因，后台不会自动发送确认通知。"
              : "外部动作确认沿用已验证的本人渠道。"}
          </small>
        </Section>
      )}
      <Section title="诊断日志">
        <button className="button quiet" onClick={onLogs}>
          {showLogs ? "收起日志" : "查看相关日志"}
        </button>
        {showLogs && (
          <>
            {logs.map((e, i) => (
              <div className="log-line" key={i}>
                <small className="muted">
                  <Time value={e.timestamp} />
                </small>
                <code>
                  {e.component} / {e.event}
                  {e.error_code ? ` · ${e.error_code}` : ""}
                </code>
              </div>
            ))}
            {!logs.length && (
              <p className="muted">
                暂无匹配日志；仅显示结构化诊断，不展示工具载荷。
              </p>
            )}
          </>
        )}
      </Section>
      <Section title="CLI 查询">
        <Command text={t.command} />
        <Command text={data.logs_command} />
        {t.status === "failed" && <Command text={data.resume_command} />}
      </Section>
    </>
  );
}
