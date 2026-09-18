import type { Meta, Runtime, SourceView } from "../types";
import {
  Badge,
  Empty,
  Heading,
  KV,
  Time,
  labels,
  moduleLabels,
  modes,
  processLabels,
} from "../components/common";
export function Running({
  meta,
  runtimes,
  sources,
}: {
  meta?: Meta;
  runtimes: Runtime[];
  sources: SourceView[];
}) {
  const service = meta?.service,
    managed = new Set(
      service?.modules
        .filter((m) => m.key.startsWith("agent:"))
        .map((m) => m.key.slice(6)),
    );
  const channelModules =
    service?.modules.filter((m) => !m.key.startsWith("agent:")) || [];
  const agentModules =
    service?.modules.filter((m) => m.key.startsWith("agent:")) || [];
  const applications = service?.id
    ? runtimes.filter((r) => managed.has(r.id))
    : runtimes;
  const moduleStatus = (m: NonNullable<Meta["service"]>["modules"][number]) => (
    <KV label={m.name} key={m.key}>
      {moduleLabels[m.state] || labels[m.state] || m.state}
      {m.error_code && ` · ${m.error_code}`}
    </KV>
  );
  return (
    <>
      <Heading title="运行">
        一个统一服务负责接收和调度；通道与 Agent 按职责分别显示。
      </Heading>
      {service && (
        <article className="card">
          <div className="card-head">
            <h2>统一服务</h2>
            <span className={`badge ${service.state}`}>
              {moduleLabels[service.state] ||
                labels[service.state] ||
                service.state}
            </span>
          </div>
          <KV label="服务 PID">{service.pid || "—"}</KV>
          <KV label="最近心跳">
            <Time value={service.heartbeat_at} />
          </KV>
          <p className="muted">
            下列项目共享同一个服务进程；分组表示职责边界，不代表独立常驻进程。
          </p>
          {!!channelModules.length && (
            <section>
              <h3>通道与采集</h3>
              {channelModules.map(moduleStatus)}
            </section>
          )}
          {!!agentModules.length && (
            <section>
              <h3>处理 Agent</h3>
              {agentModules.map(moduleStatus)}
            </section>
          )}
        </article>
      )}
      <h2 className="section-title">处理 Agent</h2>
      <p className="muted">
        本人私聊、群内请求和主动值守按身份、上下文与权限边界独立调度。
      </p>
      <div className="cards">
        {applications.map((r) => (
          <article className="card" key={r.id}>
            <div className="card-head">
              <h2>{r.name}</h2>
              <Badge status={r.status} />
            </div>
            <p className="muted">{modes[r.mode] || r.mode}</p>
            <KV label="Agent 活性">{processLabels[r.process_state]}</KV>
            <KV label="待处理消息">{r.pending_messages}</KV>
            {r.mode === "proactive" && r.work && <>
              <KV label="后台共享分析槽位">{r.work.analysis_active} / {r.work.analysis_limit}</KV>
              <KV label="后台共享执行槽位">{r.work.execution_active} / {r.work.execution_limit}（审查 {r.work.review_active}）</KV>
              <KV label="排队任务">{r.work.queued_tasks}</KV>
              <KV label="排队记忆审查">{r.work.queued_reviews}</KV>
              <KV label="最老消息等待">{r.work.oldest_waiting_seconds} 秒</KV>
              <KV label="分析及时性">{({idle:"暂无工作",collecting:"聚合中",delayed:"延迟 / 排队",up_to_date:"已处理至最近批次",coverage_gap:"存在分析缺口"} as Record<string,string>)[r.work.analysis_health] || r.work.analysis_health}</KV>
              <KV label="任务可推进性">{({idle:"暂无工作",running:"执行中",queued:"有排队任务",needs_attention:"存在失败、阻塞或待确认任务"} as Record<string,string>)[r.work.task_health] || r.work.task_health}</KV>
              <KV label="最近成功分析"><Time value={r.work.last_analysis_at}/></KV>
              <KV label="重试消息 / 分析缺口 / 累计超时">{r.work.retry_batches} / {r.work.analysis_gaps} / {r.work.timed_out}</KV>
              <p className="muted">控制心跳仅证明进程存活，不代表模型有进展；采集连续性见下方数据源覆盖。</p>
            </>}
            {!!r.waiting_receipt_messages && (
              <KV label="等待发送回执核对">{r.waiting_receipt_messages}</KV>
            )}
            <KV label={r.mode === "proactive" ? "未执行操作" : "待确认操作"}>
              {r.pending_actions}
            </KV>
            <KV label="运行配置版本">{r.config_version}</KV>
            {r.runners?.map((runner, i) => (
              <div key={i}>
                {!service?.id && <KV label="所在进程 PID">{runner.pid}</KV>}
                <KV label="最近心跳">
                  <Time value={runner.heartbeat_at} />
                </KV>
                <KV label="运行版本">{runner.version}</KV>
                <KV label="运行构建">{runner.build}</KV>
              </div>
            ))}
            {!r.runners?.length && (
              <p className="muted">未发现可验证的进程报告，运行版本未知。</p>
            )}
            {Object.entries(r.tasks || {}).map(([s, n]) => (
              <KV key={s} label={labels[s] || s}>
                {n}
              </KV>
            ))}
          </article>
        ))}
        {!applications.length && <Empty>未配置处理 Agent。</Empty>}
      </div>
      <h2 className="section-title">数据源</h2>
      <div className="cards">
        {sources.map((item) => {
          const d = item.source,
            coverage = item.coverage?.conversations || [];
          return (
            <article className="card" key={d.id}>
              <div className="card-head">
                <h2>{d.name}</h2>
                <Badge status={d.status} />
              </div>
              <KV label="接收租约">
                {item.receiver_active
                  ? "有效 · 平台就绪需看诊断"
                  : "无有效租约"}
              </KV>
              <KV label="私聊采集">{d.direct_enabled ? "已开启" : "关闭"}</KV>
              <KV label="启用时间">
                <Time value={d.direct_enabled_at} />
              </KV>
              <KV label="保留策略">{d.retention_days} 天</KV>
              <KV label="私聊发现覆盖">
                <Time value={d.direct_discovery_covered_until} />
              </KV>
              <KV label="最近私聊收流">
                <Time value={d.last_direct_received_at} />
              </KV>
              <KV label="最近清理">
                <Time value={d.last_retention_at} /> · {d.last_retention_count}{" "}
                条
              </KV>
              {(d.last_error_code || d.retention_error_code) && (
                <div className="notice">
                  {d.last_error_code} {d.retention_error_code}
                </div>
              )}
              <details id={`coverage-${d.id}`}>
                <summary>覆盖与缺口 · {coverage.length} 个会话</summary>
                {coverage.map((c, i) => {
                  const w = c.watermark || c;
                  return (
                    <div key={i} className="coverage">
                      <code className="id">
                        {c.conversation_id || w.conversation_id}
                      </code>
                      <KV label="覆盖至">
                        <Time value={w.covered_until} />
                      </KV>
                      <KV label="最近观测">
                        <Time value={w.observed_at} />
                      </KV>
                      <KV label="缺口">{c.gaps?.length || 0}</KV>
                      {c.gaps?.map((gap, j) => (
                        <p className="muted" key={j}>
                          <Time value={gap.start_at} /> →{" "}
                          <Time value={gap.end_at} /> · {gap.stop_reason}
                        </p>
                      ))}
                    </div>
                  );
                })}
              </details>
            </article>
          );
        })}
        {!sources.length && <Empty>未配置独立数据源。</Empty>}
      </div>
    </>
  );
}
