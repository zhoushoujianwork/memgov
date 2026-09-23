import { query } from "../shared/api.js";
import { platform } from "../shared/platform.js";

let activeDialog = null;
const capabilities = [
  "conversation_history_read",
  "artifact_create",
  "local_read",
  "local_write",
  "local_test",
];
function el(tag, text = "", cls = "") {
  const e = document.createElement(tag);
  e.textContent = text;
  if (cls) e.className = cls;
  return e;
}
function add(parent, ...children) {
  parent.append(...children);
  return parent;
}
function field(label, control) {
  return add(el("label", "", "editor-field"), el("span", label), control);
}
function input(value) {
  const e = el("input");
  e.value = value || "";
  return e;
}
function select(values, value) {
  const e = el("select");
  for (const [v, label] of values) {
    const o = el("option", label);
    o.value = v;
    o.selected = v === value;
    e.append(o);
  }
  return e;
}
function lines(values) {
  const e = el("textarea");
  e.rows = 3;
  e.value = (values || []).join("\n");
  return e;
}
function split(control) {
  return control.value
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);
}
function button(label, cls = "button quiet") {
  const e = el("button", label, cls);
  e.type = "button";
  return e;
}
async function post(path, body) {
  return query(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Memgov-Console": "1" },
    body: JSON.stringify(body),
  });
}

export function dismissAgentEditor() {
  activeDialog?.remove();
  activeDialog = null;
}

function edit(config, item, operation, onSaved) {
  dismissAgentEditor();
  const dialog = el("dialog", "", "agent-dialog");
  activeDialog = dialog;
  const heading = el(
    "h2",
    `${operation === "delete" ? "删除" : "编辑"} Agent · ${item.name}`,
  );
  heading.id = "agent-dialog-heading";
  dialog.setAttribute("aria-labelledby", heading.id);
  const close = button("关闭");
  close.addEventListener("click", dismissAgentEditor);
  add(
    dialog,
    add(el("div", "", "dialog-heading"), heading, close),
    el("p", "仅修改当前 YAML 声明；运行时配置需要另行预览和应用。", "muted"),
  );
  const form = el("form", "", "agent-form");
  form.addEventListener("submit", (e) => e.preventDefault());
  const a = item.agent;
  const preset = input(a.preset),
    profile = input(a.claude_profile),
    model = input(a.execution_model);
  const inherit = select(
    [
      ["executor", "继承执行器全局技能"],
      ["none", "只加载声明的技能"],
    ],
    a.skills?.inherit || "none",
  );
  const paths = lines(a.skills?.paths),
    directories = lines(a.directories);
  const actions = select(
    [
      ["owner_confirmation", "需所有者确认"],
      ["owner_request", "本人明确请求（仅本人私聊）"],
      ["owner_delegated", "所有者预设委托（仅 Cyber owner）"],
    ],
    a.external_actions,
  );
  const bash = el("input");
  bash.type = "checkbox";
  bash.checked = !!a.bash;
  const caps = new Map();
  const capBox = el("div", "", "capabilities");
  for (const cap of capabilities) {
    const c = el("input");
    c.type = "checkbox";
    c.checked = (a.capabilities || []).includes(cap);
    caps.set(cap, c);
    capBox.append(field(cap, c));
  }
  if (operation === "update")
    add(
      form,
      field("Preset", preset),
      field("Claude 配置别名", profile),
      field("执行模型（使用别名时填 profile）", model),
      field("技能继承", inherit),
      field("技能目录（每行一个，支持 ~ 和相对路径）", paths),
      field("允许读取的目录（每行一个绝对路径）", directories),
      field("外部动作策略", actions),
      field("开启 Bash", bash),
      add(el("fieldset"), el("legend", "能力"), capBox),
    );
  else
    form.append(
      el(
        "div",
        "只移除 Agent 声明。任务历史、Preset 和技能文件继续保留。",
        "notice",
      ),
    );
  if (item.references.length)
    form.append(el("p", `引用：${item.references.join("；")}`, "muted"));
  const error = el("p", "", "error hidden");
  error.setAttribute("role", "alert");
  const previewBox = el("section", "", "edit-preview hidden");
  const preview = button("预览变更"),
    save = button(
      operation === "delete" ? "删除声明并保存 YAML" : "保存到 YAML",
      "button",
    );
  save.disabled = true;
  let reviewed = null,
    working = false;
  const collect = () => ({
    revision: config.revision,
    name: item.name,
    operation,
    agent:
      operation === "delete"
        ? {}
        : {
            preset: preset.value.trim(),
            claude_profile: profile.value.trim(),
            execution_model: model.value.trim(),
            capabilities: [...caps]
              .filter(([, c]) => c.checked)
              .map(([cap]) => cap),
            directories: split(directories),
            skills: { inherit: inherit.value, paths: split(paths) },
            external_actions: actions.value,
            bash: bash.checked,
          },
  });
  form.addEventListener("input", () => {
    reviewed = null;
    save.disabled = true;
    previewBox.classList.add("hidden");
  });
  form.addEventListener("change", () => {
    reviewed = null;
    save.disabled = true;
    previewBox.classList.add("hidden");
  });
  function fail(e) {
    error.textContent = e.message || String(e);
    error.classList.remove("hidden");
  }
  function busy(value) {
    working = value;
    preview.disabled = value;
    save.disabled = value || !reviewed;
    for (const c of form.querySelectorAll("input,select,textarea"))
      c.disabled = value;
  }
  preview.addEventListener("click", async () => {
    if (working) return;
    reviewed = null;
    error.classList.add("hidden");
    busy(true);
    try {
      const draft = collect(),
        response = await post("agent-config/preview", draft);
      if (activeDialog !== dialog) return;
      reviewed = draft;
      const p = response.data;
      previewBox.replaceChildren(el("h3", "变更预览"));
      if (p.change.permission_expansion)
        previewBox.append(
          el(
            "p",
            "能力或技能范围增加；实际应用仍需现有流程中的明确授权。",
            "notice",
          ),
        );
      if (p.change.boundary_change)
        previewBox.append(
          el(
            "p",
            "技能或执行策略发生变化；应用前会重新校验边界。",
            "notice",
          ),
        );
      previewBox.append(el("p", p.message, "muted"));
      const details = add(
        el("details"),
        el("summary", "查看声明内容"),
        el(
          "pre",
          JSON.stringify(
            {
              before: p.before,
              after: operation === "delete" ? null : p.after,
            },
            null,
            2,
          ),
        ),
      );
      previewBox.append(details);
      previewBox.classList.remove("hidden");
    } catch (e) {
      fail(e);
    } finally {
      busy(false);
    }
  });
  save.addEventListener("click", async () => {
    if (working || !reviewed) return;
    error.classList.add("hidden");
    busy(true);
    try {
      await post("agent-config/save", reviewed);
      if (activeDialog !== dialog) return;
      dismissAgentEditor();
      onSaved(
        `${item.name} 的声明已${operation === "delete" ? "删除" : "保存"}，待应用。请先运行 config plan，再按其摘要和版本执行 apply-runtime。`,
      );
    } catch (e) {
      reviewed = null;
      fail(e);
    } finally {
      busy(false);
    }
  });
  add(
    dialog,
    form,
    error,
    previewBox,
    add(el("div", "", "dialog-actions"), preview, save),
  );
  dialog.addEventListener("cancel", dismissAgentEditor);
  document.body.append(dialog);
  dialog.showModal();
}

export function renderAgentDeclarations(root, config, onSaved) {
  root.append(
    el("h2", "Agent 声明", "section-title"),
    el("p", "编辑当前 YAML；与下方运行时的已应用策略分别展示。", "muted"),
  );
  if (config.error) root.append(el("p", config.error, "notice"));
  const grid = el("div", "", "cards");
  for (const item of config.items) {
    const card = add(
      el("article", "", "card"),
      add(
        el("div", "", "card-head"),
        el("h3", item.name),
        el("span", item.pending ? "待应用" : "声明与已应用一致", "badge"),
      ),
    );
    card.append(
      el(
        "p",
        item.references.length
          ? `引用：${item.references.join("；")}`
          : "未被应用引用",
        "muted",
      ),
    );
    const editButton = button("编辑 Agent"),
      remove = button("删除 Agent", "button quiet danger");
    editButton.setAttribute("aria-label", `编辑 Agent ${item.name}`);
    remove.setAttribute("aria-label", `删除 Agent ${item.name}`);
    editButton.addEventListener("click", () =>
      edit(config, item, "update", onSaved),
    );
    remove.disabled = !item.can_delete;
    remove.title = item.can_delete
      ? "删除配置声明"
      : "先解除 YAML 和已应用配置中的绑定";
    remove.addEventListener("click", () =>
      edit(config, item, "delete", onSaved),
    );
    add(card, add(el("div", "", "dialog-actions"), editButton, remove));
    grid.append(card);
  }
  if (!config.items.length)
    grid.append(
      el(
        "p",
        "当前 YAML 尚无 Agent 声明。运行时默认配置继续通过 CLI 管理。",
        "empty",
      ),
    );
  root.append(grid);
  const copy = button("复制配置预览命令");
  copy.addEventListener("click", async () => {
    try {
      await platform.copy(config.plan_command);
      copy.textContent = "已复制";
    } catch (e) {
      onSaved(e.message);
    }
  });
  root.append(
    add(el("div", "", "command"), el("code", config.plan_command), copy),
  );
}
