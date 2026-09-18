import {
  createContext,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";
import { relativeTime } from "../shared/time.js";
import { platform } from "../shared/platform.js";

export const labels: Record<string, string> = {
  running: "记录为运行中",
  pending: "待处理",
  completed: "已完成",
  failed: "失败",
  stale: "已失效",
  clarification: "待澄清",
  blocked: "处理受阻",
  action_failed: "外部操作失败",
  action_unknown: "外部结果未知",
  cancelled: "已取消",
  awaiting_confirmation: "待确认",
  paused: "已暂停",
  stopped: "已停止",
  degraded: "异常",
};
export const moduleLabels: Record<string, string> = {
  active: "运行中",
  retrying: "等待重试",
  blocked: "需要处理",
  unverified: "未验证",
  stopping: "停止中",
  running: "运行中",
};
export const modes: Record<string, string> = {
  direct: "本人私聊",
  proactive: "Cyber owner",
  group_mention: "群 Agent",
};
export const processLabels: Record<string, string> = {
  heartbeat: "近期心跳正常",
  multiple: "多个执行实例 · 需要核对",
  stopped_record: "Agent 记录已停止",
  unknown: "Agent 活性未验证",
};
const Clock = createContext(Date.now());
const exact = new Intl.DateTimeFormat("zh-CN", {
  year: "numeric",
  month: "numeric",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
  timeZoneName: "short",
});
export function ClockProvider({ children }: { children: ReactNode }) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const tick = () => {
      if (!document.hidden) setNow(Date.now());
    };
    const timer = setInterval(tick, 15000);
    document.addEventListener("visibilitychange", tick);
    return () => {
      clearInterval(timer);
      document.removeEventListener("visibilitychange", tick);
    };
  }, []);
  return <Clock.Provider value={now}>{children}</Clock.Provider>;
}
export function Time({
  value,
  prefix = "",
  empty = "未记录",
}: {
  value?: string;
  prefix?: string;
  empty?: string;
}) {
  const now = useContext(Clock),
    stamp = value ? Date.parse(value) : NaN;
  if (!Number.isFinite(stamp))
    return (
      <span>
        {prefix}
        {value ? "时间未知" : empty}
      </span>
    );
  const text = prefix + relativeTime(value!, Math.max(now, Date.now())),
    title = exact.format(stamp);
  return (
    <time
      dateTime={new Date(stamp).toISOString()}
      title={title}
      aria-label={`${text}（${title}）`}
    >
      {text}
    </time>
  );
}
export function Badge({ status }: { status: string }) {
  return (
    <span className={`badge ${status}`}>
      {labels[status] || status || "未知"}
    </span>
  );
}
export function KV({
  label,
  children,
}: {
  label: string;
  children?: ReactNode;
}) {
  return (
    <div className="kv">
      <span className="muted">{label}</span>
      <span>{children ?? "未知"}</span>
    </div>
  );
}
export function Heading({
  title,
  children,
}: {
  title: string;
  children: ReactNode;
}) {
  return (
    <div className="page-heading">
      <div>
        <h1>{title}</h1>
        <p className="muted">{children}</p>
      </div>
    </div>
  );
}
export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}
export function Section({
  title,
  children,
}: {
  title: string;
  children: ReactNode;
}) {
  return (
    <section className="detail-section">
      <h3>{title}</h3>
      {children}
    </section>
  );
}
export function Command({ text }: { text: string }) {
  const [message, setMessage] = useState("复制命令");
  return (
    <div className="command">
      <code>{text}</code>
      <button
        className="button quiet"
        onClick={async () => {
          try {
            await platform.copy(text);
            setMessage("已复制");
          } catch (e) {
            setMessage(e instanceof Error ? e.message : String(e));
          }
        }}
      >
        {message}
      </button>
    </div>
  );
}
