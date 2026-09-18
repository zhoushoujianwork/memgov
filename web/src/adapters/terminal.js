import { timeElement } from "../shared/time.js";
// A read-only terminal for public Claude events, with bounded local memory.
const labels = {
  status: "状态",
  assistant: "Claude",
  tool: "工具调用",
  output: "工具输出",
  error: "错误",
  result: "结果",
  limit: "截断",
};
let current = null;
function el(tag, text = "", cls = "") {
  const node = document.createElement(tag);
  node.textContent = text;
  if (cls) node.className = cls;
  return node;
}

export function clearTerminal() {
  if (current) {
    current.source?.close();
    clearTimeout(current.retry);
    current.lines = [];
    current.host?.replaceChildren();
  }
  current = null;
}

function paint() {
  if (!current?.host?.isConnected) return;
  const body = current.host.querySelector(".terminal-body");
  const scroll = current.scrollTop || 0;
  body.replaceChildren();
  if (!current.lines.length)
    body.append(
      el(
        "div",
        "暂无可用过程输出。等待新事件；旧版本调用或已到期日志无法回补。",
        "terminal-empty",
      ),
    );
  for (const event of current.lines) {
    const row = el("div", "", `terminal-line ${event.kind}`);
    const label = el("span", "", "terminal-label");
    label.append(
      timeElement(event.timestamp, { empty: "" }),
      el("span", ` ${labels[event.kind] || "提示"}`),
    );
    row.append(label, el("pre", event.text));
    body.append(row);
  }
  current.host.querySelector(".terminal-status").textContent = current.status;
  const follow = current.host.querySelector(".terminal-follow");
  follow.textContent = current.follow ? "暂停跟随" : "跟随最新输出";
  body.scrollTop = current.follow ? body.scrollHeight : scroll;
  current.scrollTop = body.scrollTop;
}

function add(event) {
  current.lines.push(event);
  current.bytes += event.text.length;
  while (current.lines.length > 500 || current.bytes > 256 * 1024)
    current.bytes -= current.lines.shift().text.length;
  paint();
}

function connect() {
  if (!current || document.hidden || !current.open) return;
  const selected = current;
  selected.source?.close();
  const source = new EventSource(
    `/api/v1/tasks/${encodeURIComponent(selected.task)}/terminal?${new URLSearchParams({ attempt: selected.attempt })}`,
  );
  selected.source = source;
  selected.status = "正在连接执行输出…";
  paint();
  const read = (event, callback) => {
    if (current !== selected || selected.source !== source) return;
    try {
      callback(JSON.parse(event.data));
    } catch {
      selected.status = "输出格式不可用";
      paint();
    }
  };
  source.addEventListener("output", (event) => read(event, add));
  source.addEventListener("notice", (event) =>
    read(event, (data) => add({ kind: "limit", text: data.text })),
  );
  source.addEventListener("status", (event) =>
    read(event, (data) => {
      selected.status =
        data.status === "running"
          ? "输出已连接 · 等待新事件"
          : "执行尝试已结束";
      paint();
    }),
  );
  source.addEventListener("finished", (event) =>
    read(event, (data) => {
      selected.status = data.available
        ? `执行尝试已结束 · ${data.status}`
        : "执行尝试已结束 · 未采集到可用过程输出";
      source.close();
      paint();
    }),
  );
  source.addEventListener("invalidated", (event) =>
    read(event, (data) => {
      source.close();
      selected.lines = [];
      selected.bytes = 0;
      selected.status = data.reason;
      selected.invalid = true;
      paint();
    }),
  );
  source.onerror = () => {
    if (current !== selected || selected.source !== source || selected.invalid)
      return;
    source.close();
    selected.lines = [];
    selected.bytes = 0;
    selected.status = "连接中断 · 正在重连";
    paint();
    selected.retry = setTimeout(() => {
      if (current === selected) connect();
    }, 3000);
  };
}

export function mountTerminal(root, data) {
  const task = data.task;
  if (task.redacted || !data.can_view_output || !data.attempts.length) {
    clearTerminal();
    if (data.attempts.length && !data.can_view_output) {
      const section = el("section", "", "detail-section");
      section.append(
        el("h3", "执行过程"),
        el("p", "原请求已变化或不可用，执行过程不再展示。", "muted"),
      );
      root.append(section);
    }
    return;
  }
  const available = data.attempts.filter((a) => a.output_visible !== false);
  if (!available.length) {
    clearTerminal();
    const section = el("section", "", "detail-section");
    section.append(
      el("h3", "执行过程"),
      el("p", "旧版本的执行输出已失效，保留执行元数据供核对。", "muted"),
    );
    root.append(section);
    return;
  }
  const latest = available.at(-1).id;
  if (
    !current ||
    current.task !== task.id ||
    current.version !== task.version ||
    !available.some((a) => a.id === current.attempt)
  ) {
    clearTerminal();
    current = {
      task: task.id,
      version: task.version,
      attempt: latest,
      lines: [],
      bytes: 0,
      follow: true,
      open: task.status === "running",
      status: "尚未连接",
      host: null,
      source: null,
    };
  }
  const section = el("section", "", "detail-section");
  section.append(el("h3", "执行过程"));
  const toggle = el(
    "button",
    current.open ? "收起 Web Terminal" : "查看 Web Terminal",
    "button quiet",
  );
  toggle.id = "terminal-toggle";
  section.append(toggle);
  root.append(section);
  toggle.onclick = () => {
    current.open = !current.open;
    current.source?.close();
    current.source = null;
    clearTimeout(current.retry);
    mountContents();
  };
  function mountContents() {
    current.host?.remove();
    current.host = null;
    toggle.textContent = current.open
      ? "收起 Web Terminal"
      : "查看 Web Terminal";
    if (!current.open) return;
    const host = el("div", "", "web-terminal");
    current.host = host;
    const toolbar = el("div", "", "terminal-toolbar");
    const attempts = el("select");
    attempts.id = "terminal-attempt";
    attempts.setAttribute("aria-label", "选择执行尝试");
    data.attempts.forEach((a, i) => {
      const option = el(
        "option",
        `尝试 ${i + 1} · ${a.status} · ${a.model || "Claude"}`,
      );
      option.value = a.id;
      option.disabled = a.output_visible === false;
      option.selected = a.id === current.attempt;
      attempts.append(option);
    });
    attempts.onchange = () => {
      current.attempt = attempts.value;
      current.lines = [];
      current.bytes = 0;
      current.scrollTop = 0;
      current.invalid = false;
      clearTimeout(current.retry);
      connect();
    };
    const follow = el("button", "", "terminal-follow");
    follow.onclick = () => {
      current.follow = !current.follow;
      paint();
    };
    const status = el("span", "", "terminal-status");
    status.setAttribute("role", "status");
    toolbar.append(attempts, follow);
    host.append(toolbar, status);
    const body = el("div", "", "terminal-body");
    body.setAttribute("role", "region");
    body.setAttribute("aria-label", "Claude 执行输出");
    body.tabIndex = 0;
    body.onscroll = () => {
      if (current?.host !== host) return;
      current.scrollTop = body.scrollTop;
      if (
        body.scrollHeight - body.clientHeight - body.scrollTop > 24 &&
        current.follow
      ) {
        current.follow = false;
        host.querySelector(".terminal-follow").textContent = "跟随最新输出";
      }
    };
    host.append(
      body,
      el(
        "div",
        "只读 · 展示公开文字及工具事件 · 工具结果不代表验收或投递完成",
        "terminal-foot",
      ),
    );
    section.append(host);
    paint();
    if (!current.source && !current.invalid) connect();
  }
  mountContents();
}
