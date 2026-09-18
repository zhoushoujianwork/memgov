import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type MouseEvent,
} from "react";
import { flushSync } from "react-dom";
import { get, post } from "./api";
import { clearLegacyToken } from "./shared/api.js";
import { refreshRelativeTimes } from "./shared/time.js";
import { clearTerminal } from "./adapters/terminal.js";
import { dismissAgentEditor } from "./adapters/agent-editor.js";
import { ClockProvider, Empty, Time } from "./components/common";
import { Tasks, type TaskFilters } from "./pages/Tasks";
import {
  Memories,
  memoryParams,
  memoryPath,
  type MemoryFilters,
} from "./pages/Memories";
import { Running } from "./pages/Running";
import { Settings } from "./pages/Settings";
import { VersionStatus } from "./components/VersionStatus";
import type {
  AgentConfig,
  AgentView,
  LogEvent,
  MemoryCard,
  MemoryDetail,
  MemoryHistory,
  MemoryList,
  MemorySources,
  Meta,
  Runtime,
  SourceView,
  Task,
  TaskDetail,
  Workspace,
} from "./types";

export const pagePaths = {
  tasks: "/tasks",
  memories: "/memories",
  running: "/running",
  settings: "/settings",
};
type Page = keyof typeof pagePaths;
const names = {
  tasks: "任务",
  memories: "记忆",
  running: "运行",
  settings: "设置",
};
export function pageFromPath(path: string): Page {
  return (
    (Object.keys(pagePaths) as Page[]).find(
      (p) => pagePaths[p] === path.replace(/\/$/, ""),
    ) || "tasks"
  );
}
interface View {
  meta: Meta;
  runtimes: Runtime[];
  sampled: string;
  tasks?: { tasks: Task[]; next_cursor: string };
  detail?: TaskDetail;
  logs?: LogEvent[];
  sources?: SourceView[];
  agents?: AgentView[];
  config?: AgentConfig;
  memories?: MemoryList;
  workspaces?: Workspace[];
  memory?: MemoryDetail;
  memorySources?: MemorySources;
  memoryHistory?: MemoryHistory;
}
export function expiryTimes(view: View) {
  return [
    ...(view.tasks?.tasks || []).map((t) => t.expires_at),
    view.detail?.task.expires_at,
    view.memory?.expires_at,
    view.memorySources?.expires_at,
    view.memoryHistory?.expires_at,
  ]
    .filter((v): v is string => !!v)
    .map(Date.parse)
    .filter(Number.isFinite);
}
export function noticeForTask(
  notice: { id: string; text: string } | undefined,
  taskID: string,
) {
  return notice?.id === taskID ? notice.text : "";
}
export function App() {
  const [page, setPage] = useState<Page>(() => pageFromPath(location.pathname));
  const [filters, setFilters] = useState<TaskFilters>({
    status: "active",
    runtime: "",
    cursor: "",
    history: [],
  });
  const [memoryFilters, setMemoryFilters] = useState<MemoryFilters>({
    q: "",
    workspace: "global",
    category: "",
    status: "active",
    page: 1,
  });
  const [settledMemory, setSettledMemory] = useState(memoryFilters);
  const [selected, setSelected] = useState(""),
    [memorySelected, setMemorySelected] = useState<MemoryCard>();
  const [showLogs, setShowLogs] = useState(false),
    [showSources, setShowSources] = useState(false),
    [showHistory, setShowHistory] = useState(false);
  const [data, setData] = useState<View>(),
    [error, setError] = useState(""),
    [connected, setConnected] = useState(false),
    [refresh, setRefresh] = useState(0);
  const [continuing, setContinuing] = useState(""),
    [message, setMessage] = useState<{ id: string; text: string }>(),
    [editMessage, setEditMessage] = useState(""),
    [restarting, setRestarting] = useState(false);
  const restart = useRef<{ id?: string; deadline: number } | undefined>(
      undefined,
    ),
    pause = useRef(false);
  const reload = useCallback(() => setRefresh((n) => n + 1), []);
  const saved = useCallback(
    (text: string) => {
      setEditMessage(text);
      reload();
    },
    [reload],
  );
  const closeMemory = () => {
    setMemorySelected(undefined);
    setShowSources(false);
    setShowHistory(false);
  };
  useEffect(() => {
    const timer = setTimeout(() => setSettledMemory(memoryFilters), 200);
    return () => clearTimeout(timer);
  }, [memoryFilters]);
  const navigate = useCallback(
    (next: Page, push = true) => {
      if (push && location.pathname !== pagePaths[next])
        history.pushState(null, "", pagePaths[next]);
      clearTerminal();
      dismissAgentEditor();
      setData(undefined);
      setMemorySelected(undefined);
      setShowSources(false);
      setShowHistory(false);
      setShowLogs(false);
      setPage(next);
      reload();
    },
    [reload],
  );
  useEffect(() => {
    clearLegacyToken();
    if (location.pathname !== pagePaths[page])
      history.replaceState(
        null,
        "",
        pagePaths[page] + location.search + location.hash,
      );
    const pop = () => navigate(pageFromPath(location.pathname), false);
    const hide = () => {
      pause.current = true;
      clearTerminal();
      dismissAgentEditor();
      flushSync(() => {
        setData(undefined);
        setConnected(false);
        setMemorySelected(undefined);
      });
    };
    const visibility = () => {
      if (document.hidden) hide();
      else {
        pause.current = false;
        reload();
      }
    };
    window.addEventListener("popstate", pop);
    window.addEventListener("pagehide", hide);
    document.addEventListener("visibilitychange", visibility);
    const timer = setInterval(() => {
      if (!document.hidden) refreshRelativeTimes();
    }, 15000);
    return () => {
      window.removeEventListener("popstate", pop);
      window.removeEventListener("pagehide", hide);
      document.removeEventListener("visibilitychange", visibility);
      clearInterval(timer);
    };
  }, [navigate]);
  useEffect(() => {
    document.title = `memgov · ${names[page]}`;
  }, [page]);
  useEffect(() => {
    let cancelled = false,
      busy = false,
      timer: ReturnType<typeof setTimeout>,
      expiry: ReturnType<typeof setTimeout>,
      failures = 0;
    // One polling chain per view: navigation/selection invalidates in-flight responses.
    async function poll() {
      if (cancelled || busy || document.hidden || pause.current) return;
      if (
        page === "memories" &&
        JSON.stringify(memoryFilters) !== JSON.stringify(settledMemory)
      )
        return;
      busy = true;
      try {
        const [meta, runtimes] = await Promise.all([
          get<Meta>("meta"),
          get<Runtime[]>("runtimes"),
        ]);
        const view: View = {
          meta: meta.data,
          runtimes: runtimes.data,
          sampled: meta.sampled_at,
        };
        if (page === "tasks") {
          const [tasks, detail, logs] = await Promise.all([
            get<View["tasks"]>(
              `tasks?${new URLSearchParams({ status: filters.status, runtime: filters.runtime, cursor: filters.cursor, limit: "30" })}`,
            ),
            selected
              ? get<TaskDetail>(`tasks/${encodeURIComponent(selected)}`)
              : undefined,
            showLogs && selected
              ? get<{ events: LogEvent[] }>(
                  `tasks/${encodeURIComponent(selected)}/logs`,
                )
              : undefined,
          ]);
          view.tasks = tasks.data;
          view.detail = detail?.data;
          view.logs = logs?.data.events.slice(-200);
          view.sampled = tasks.sampled_at;
        } else if (page === "memories") {
          const [list, workspaces, detail, sources, history] =
            await Promise.all([
              get<MemoryList>(`memories?${memoryParams(settledMemory)}`),
              get<Workspace[]>("memory-workspaces"),
              memorySelected
                ? get<MemoryDetail>(memoryPath(memorySelected))
                : undefined,
              memorySelected && showSources
                ? get<MemorySources>(memoryPath(memorySelected, "/sources"))
                : undefined,
              memorySelected && showHistory
                ? get<MemoryHistory>(memoryPath(memorySelected, "/history"))
                : undefined,
            ]);
          view.memories = list.data;
          view.workspaces = workspaces.data;
          view.memory = detail?.data;
          view.memorySources = sources?.data;
          view.memoryHistory = history?.data;
          view.sampled = list.sampled_at;
        } else if (page === "running")
          view.sources = (await get<SourceView[]>("sources")).data;
        else {
          const [agents, config] = await Promise.all([
            get<AgentView[]>("agents"),
            meta.data.agent_editing
              ? get<AgentConfig>("agent-config")
              : undefined,
          ]);
          view.agents = agents.data;
          view.config = config?.data;
        }
        if (cancelled || document.hidden || pause.current) return;
        const deadlines = expiryTimes(view);
        if (deadlines.some((d) => d <= Date.now())) {
          setData(undefined);
          timer = setTimeout(poll, 100);
          return;
        }
        if (restart.current) {
          if (
            view.meta.service?.id !== restart.current.id &&
            ["running", "degraded"].includes(view.meta.service?.state || "")
          ) {
            restart.current = undefined;
            setRestarting(false);
          } else if (Date.now() >= restart.current.deadline)
            throw new Error(
              "未能确认重启完成，请查看终端或运行 memgov service status。",
            );
        }
        setData((previous) => {
          // Keep editor/terminal adapters stable when only the sample clock changes.
          if (previous) {
            if (JSON.stringify(previous.detail) === JSON.stringify(view.detail))
              view.detail = previous.detail;
            if (JSON.stringify(previous.config) === JSON.stringify(view.config))
              view.config = previous.config;
          }
          return view;
        });
        setConnected(true);
        setError("");
        failures = 0;
        clearTimeout(expiry);
        if (deadlines.length)
          expiry = setTimeout(
            () => {
              setData(undefined);
              clearTerminal();
              dismissAgentEditor();
              clearTimeout(timer);
              poll();
            },
            Math.min(
              2000000000,
              Math.max(1, Math.min(...deadlines) - Date.now()),
            ),
          );
        timer = setTimeout(poll, 3000);
      } catch (e) {
        if (cancelled || document.hidden || pause.current) return;
        setData(undefined);
        setConnected(false);
        clearTerminal();
        dismissAgentEditor();
        const waiting =
          restart.current && Date.now() < restart.current.deadline;
        if (!waiting) {
          restart.current = undefined;
          setRestarting(false);
          setError(e instanceof Error ? e.message : String(e));
        }
        timer = setTimeout(
          poll,
          waiting ? 1000 : Math.min(60000, 3000 * 2 ** Math.min(++failures, 5)),
        );
      } finally {
        busy = false;
      }
    }
    poll();
    return () => {
      cancelled = true;
      clearTimeout(timer);
      clearTimeout(expiry);
    };
  }, [
    page,
    filters,
    memoryFilters,
    settledMemory,
    selected,
    memorySelected,
    showLogs,
    showSources,
    showHistory,
    refresh,
  ]);
  async function restartService() {
    if (restarting || !data?.meta.service_restart || !connected) return;
    restart.current = {
      id: data.meta.service?.id,
      deadline: Date.now() + 120000,
    };
    setRestarting(true);
    setError("");
    try {
      await post("service/restart", {});
      reload();
    } catch (e) {
      restart.current = undefined;
      setRestarting(false);
      setError(e instanceof Error ? e.message : String(e));
    }
  }
  async function resume(task: Task) {
    if (continuing) return;
    setContinuing(task.id);
    try {
      await post(`tasks/${encodeURIComponent(task.id)}/resume`, {
        expected_version: task.version,
      });
      setMessage({
        id: task.id,
        text: "已提交继续请求，并恢复所属模块，等待 Agent 接手。",
      });
      setData(undefined);
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setContinuing("");
    }
  }
  const link = (event: MouseEvent<HTMLAnchorElement>, next: Page) => {
    if (
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    )
      return;
    event.preventDefault();
    navigate(next);
  };
  const restartNote = restarting
    ? "等待退出并加载已安装程序"
    : !connected
      ? "连接恢复后可重启"
      : !data?.meta.service_restart
        ? "独立诊断页：请在终端重启服务"
        : "加载已安装程序，页面自动重连";
  return (
    <ClockProvider>
      <aside className="sidebar">
        <a className="brand" href="/tasks" onClick={(e) => link(e, "tasks")}>
          <span className="brand-mark">m</span>memgov
          <span className="brand-tag">LOCAL</span>
        </a>
        <div className="nav-label">工作台</div>
        <nav aria-label="主导航">
          {(Object.keys(pagePaths) as Page[]).map((p, i) => (
            <a
              href={pagePaths[p]}
              key={p}
              data-page={p}
              className={`nav ${page === p ? "active" : ""}`}
              aria-current={page === p ? "page" : undefined}
              onClick={(e) => link(e, p)}
            >
              {names[p]}
              <span>0{i + 1}</span>
            </a>
          ))}
        </nav>
        <div className="sidebar-bottom">
          <div className="sidebar-foot">
            <span className="dot" />
            本机 · 轻量管理台
            <p>
              任务、证据和运行状态
              <br />
              在同一处核对。
            </p>
          </div>
          <VersionStatus />
          <div className="sidebar-controls">
            <button
              id="restart-service"
              className="button sidebar-restart"
              disabled={restarting || !connected || !data?.meta.service_restart}
              onClick={restartService}
            >
              {restarting ? "正在重启…" : "重启服务"}
            </button>
            <p id="restart-note" className="sidebar-control-note" role="status">
              {restartNote}
            </p>
          </div>
        </div>
      </aside>
      <main>
        <header className="topbar">
          <span id="breadcrumb">工作台 / {names[page]}</span>
          <div className="connection">
            <span
              id="connection-dot"
              className={`dot ${connected ? "" : "offline"}`}
            />
            <span id="connection">
              {restarting
                ? "正在重启 · 等待重新连接"
                : connected
                  ? "已连接"
                  : "连接已断 · 数据已清空"}
            </span>
            {data && <Time value={data.sampled} prefix="更新于 " />}
          </div>
        </header>
        <div id="context" className="context">
          {data
            ? `${data.meta.home} · ${data.meta.version} · 配置 v${data.meta.applied_version} · ${data.meta.agent_editing ? "Agent 声明可编辑" : "只读"}`
            : "读取本地服务…"}
        </div>
        {error && (
          <div id="error" className="error" role="alert">
            {error}
            <button className="button quiet" onClick={reload}>
              重试连接
            </button>
          </div>
        )}
        <section id="content">
          {page === "tasks" && (
            <Tasks
              runtimes={data?.runtimes || []}
              tasks={data?.tasks?.tasks || []}
              next={data?.tasks?.next_cursor || ""}
              detail={data?.detail}
              logs={data?.logs || []}
              filters={filters}
              selected={selected}
              showLogs={showLogs}
              continuing={continuing === selected}
              message={noticeForTask(message, selected)}
              onFilter={(value) => {
                setFilters(value);
                setSelected("");
                setData(undefined);
                setShowLogs(false);
              }}
              onSelect={(id) => {
                setSelected(id);
                setMessage(undefined);
                setShowLogs(false);
                setData((old) =>
                  old ? { ...old, detail: undefined } : undefined,
                );
              }}
              onPage={(direction) => {
                const history = [...filters.history];
                const cursor =
                  direction > 0
                    ? data?.tasks?.next_cursor || ""
                    : history.pop() || "";
                if (direction > 0) history.push(filters.cursor);
                setFilters({ ...filters, cursor, history });
              }}
              onLogs={() => setShowLogs((v) => !v)}
              onResume={resume}
              onRefresh={reload}
            />
          )}
          {page === "memories" && (
            <Memories
              filters={memoryFilters}
              list={data?.memories}
              workspaces={data?.workspaces || []}
              selected={connected ? memorySelected : undefined}
              detail={data?.memory}
              sources={data?.memorySources}
              history={data?.memoryHistory}
              showSources={showSources}
              showHistory={showHistory}
              onFilter={(value) => {
                setMemoryFilters(value);
                closeMemory();
                setData((old) =>
                  old
                    ? {
                        ...old,
                        memories: undefined,
                        memory: undefined,
                        memorySources: undefined,
                        memoryHistory: undefined,
                      }
                    : undefined,
                );
              }}
              onSelect={(value) => {
                setMemorySelected(value);
                setData((old) =>
                  old
                    ? {
                        ...old,
                        memory: undefined,
                        memorySources: undefined,
                        memoryHistory: undefined,
                      }
                    : undefined,
                );
              }}
              onClose={closeMemory}
              onSources={setShowSources}
              onHistory={setShowHistory}
              onRefresh={reload}
            />
          )}
          {page === "running" && (
            <Running
              meta={data?.meta}
              runtimes={data?.runtimes || []}
              sources={data?.sources || []}
            />
          )}
          {page === "settings" && (
            <Settings
              meta={data?.meta}
              agents={data?.agents || []}
              config={data?.config}
              message={editMessage}
              onSaved={saved}
            />
          )}
          {!data && !error && <Empty>正在读取本地服务…</Empty>}
        </section>
        <footer>
          memgov · SQLite 为唯一真相源
          <span>关闭浏览器标签页不影响服务；service stop 停止全部模块</span>
        </footer>
      </main>
    </ClockProvider>
  );
}
