import type { Meta, AgentView, AgentConfig } from "../types";
import { Empty, Heading, KV } from "../components/common";
import { AgentDeclarations } from "../components/Adapters";
export function Settings({
  meta,
  agents,
  config,
  message,
  onSaved,
}: {
  meta?: Meta;
  agents: AgentView[];
  config?: AgentConfig;
  message: string;
  onSaved: (message: string) => void;
}) {
  return (
    <>
      <Heading title="设置">
        核对已应用策略与当前磁盘技能；运行会话的加载状态需结合模块观测。
      </Heading>
      {meta && (
        <article className="card instance">
          <h2>本地服务配置</h2>
          {[
            ["数据目录", meta.home],
            ["配置来源", meta.config_path],
            ["管理台版本", `${meta.version} · ${meta.build}`],
            ["已安装构建", meta.installed_build],
            ["数据库版本", meta.database_schema],
            ["已应用配置版本", meta.applied_version],
          ].map(([k, v]) => (
            <KV key={k} label={String(k)}>
              {v}
            </KV>
          ))}
        </article>
      )}
      {message && <p className="notice">{message}</p>}
      {config && <AgentDeclarations config={config} onSaved={onSaved} />}
      <h2 className="section-title">运行时已应用策略</h2>
      <div className="cards">
        {agents.map((a, i) => (
          <article className="card" key={`${a.runtime}-${a.conversation_id || i}`}>
            <span className="eyebrow">{a.runtime}</span>
            <h2>{a.agent || "运行时默认 Agent"}</h2>
            {a.conversation_id && (
              <p className="muted">会话：{a.conversation_id}</p>
            )}
            {[
              ["Preset", a.preset],
              ["执行模型", a.model],
              ["Claude 配置", a.profile],
              ["记忆范围", a.memory_scope],
              ["Bash", a.bash ? "开启" : "关闭"],
              ["外部动作", a.external_actions],
              ["技能继承", a.inherit || "none"],
              ["能力", a.capabilities?.join(" · ") || "无"],
              ["声明目录", a.directories?.join(" · ") || "无"],
            ].map(([k, v]) => (
              <KV key={k} label={k}>
                {v}
              </KV>
            ))}
            {(a.policy_error || a.skill_error) && (
              <div className="notice">
                配置诊断：{a.policy_error} {a.skill_error}
              </div>
            )}
            <details id={`skills-${a.runtime}-${i}`}>
              <summary>技能清单 · {a.skills.length}</summary>
              {a.skills.map((s, j) => (
                <div className="skill" key={j}>
                  <strong>{s.name}</strong>
                  <small className="muted">
                    {s.origin === "runtime_managed"
                      ? "运行时管理"
                      : `${s.path || ""} · ${(s.digest || "").slice(0, 12)}`}
                  </small>
                </div>
              ))}
            </details>
          </article>
        ))}
        {!agents.length && <Empty>暂无 Agent 配置。</Empty>}
      </div>
    </>
  );
}
