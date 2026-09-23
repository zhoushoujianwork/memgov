import assert from "node:assert/strict";
import test from "node:test";
import { renderToStaticMarkup } from "react-dom/server";
import { Time } from "../src/components/common";
import { Tasks } from "../src/pages/Tasks";
import { Running } from "../src/pages/Running";
import { Workspaces, workspacePath } from "../src/pages/Workspaces";
import { pageFromPath, expiryTimes, noticeForTask } from "../src/App";
import type { TaskDetail, Meta, Runtime } from "../src/types";

test("late continuation notifications belong to the original task", () => {
  const receipt = { id: "task-a", text: "已提交继续请求" };
  assert.equal(noticeForTask(receipt, "task-b"), "");
  assert.equal(noticeForTask(receipt, "task-a"), "已提交继续请求");
  assert.equal(noticeForTask(undefined, "task-a"), "");
});

test("workspace browser preserves file boundaries and escapes paths and snippets", () => {
  assert.equal(
    workspacePath("group-a", "file", "notes/中文.md"),
    "agent-workspaces/group-a/file?path=notes%2F%E4%B8%AD%E6%96%87.md",
  );
  assert.equal(
    workspacePath("owner/a", "history", "MEMORY.md"),
    "agent-workspaces/owner%2Fa/history?path=MEMORY.md",
  );
  assert.equal(
    workspacePath("group-a", "files", "a&b"),
    "agent-workspaces/group-a/files?query=a%26b",
  );
  assert.equal(pageFromPath("/workspaces/"), "workspaces");
  assert.equal(pageFromPath("/memories"), "tasks");
  const html = renderToStaticMarkup(
    <Workspaces
      filters={{ workspace: "owner-a", query: "<script>" }}
      workspaces={[
        {
          id: "owner-a",
          kind: "owner",
          owner_principal_id: "owner",
          created_at: "2026-09-23T10:00:00Z",
        },
      ]}
      files={[
        {
          path: "notes/<script>.md",
          digest: "abc",
          bytes: 30,
          updated_at: "2026-09-23T10:00:00Z",
          snippet: "<script>alert(1)</script>",
        },
      ]}
      selected=""
      showHistory={false}
      onFilter={() => {}}
      onSelect={() => {}}
      onHistory={() => {}}
      onRefresh={() => {}}
    />,
  );
  assert.ok(html.includes("notes/&lt;script&gt;.md"));
  assert.ok(html.includes("&lt;script&gt;alert(1)&lt;/script&gt;"));
  assert.ok(html.includes("只读浏览"));
  assert.ok(html.includes('aria-label="搜索当前工作区"'));
  assert.ok(!html.includes("全局"));
});
test("runtime UI separates shared channels from permission-scoped agents", () => {
  const runtimes = [
    { id: "owner", name: "owner-private", mode: "direct" },
    { id: "group", name: "group-mention", mode: "group_mention" },
    { id: "cyber", name: "proactive", mode: "proactive" },
  ] as Runtime[];
  const tasks = renderToStaticMarkup(
    <Tasks
      runtimes={runtimes}
      tasks={[]}
      next=""
      logs={[]}
      filters={{ status: "all", runtime: "", cursor: "", history: [] }}
      selected=""
      showLogs={false}
      continuing={false}
      message=""
      onFilter={() => {}}
      onSelect={() => {}}
      onPage={() => {}}
      onLogs={() => {}}
      onResume={() => {}}
      onRefresh={() => {}}
    />,
  );
  assert.ok(tasks.includes('aria-label="按处理 Agent 筛选"'));
  assert.ok(tasks.includes("全部处理 Agent"));
  assert.ok(tasks.includes('<optgroup label="本人私聊 Agent">'));
  assert.ok(tasks.includes('<optgroup label="群 Agent">'));
  assert.ok(tasks.includes('<optgroup label="主动值守 Agent">'));
  assert.ok(!tasks.includes("全部运行模块"));

  const running = renderToStaticMarkup(
    <Running
      meta={
        {
          service: {
            id: "service",
            pid: 42,
            state: "running",
            heartbeat_at: "2026-09-18T10:00:00Z",
            modules: [
              { key: "source:watch", name: "dingtalk-watch", state: "active" },
              {
                key: "receiver:bot",
                name: "dingtalk-bot-chat",
                state: "active",
              },
              { key: "agent:owner", name: "owner-private", state: "active" },
            ],
          },
        } as Meta
      }
      runtimes={runtimes}
      sources={[]}
    />,
  );
  assert.ok(running.includes("通道与采集"));
  assert.ok(running.includes("处理 Agent"));
  assert.ok(running.includes("共享同一个服务进程"));
  assert.ok(running.includes("不代表独立常驻进程"));
});
test("a finished task without result or receipt never implies delivery or acceptance", () => {
  const detail = {
    task: {
      id: "t1",
      version: 2,
      title: "已处理请求",
      status: "completed",
      runtime_name: "owner",
      runtime_status: "running",
      process_state: "heartbeat",
      updated_at: "2026-09-16T10:00:00Z",
    },
    can_resume: false,
    can_view_output: true,
    messages: [],
    attempts: [],
    deliveries: [],
    actions: [],
    logs_command: "memgov logs",
  } as unknown as TaskDetail;
  const html = renderToStaticMarkup(
    <Tasks
      runtimes={[]}
      tasks={[]}
      next=""
      detail={detail}
      logs={[]}
      filters={{ status: "all", runtime: "", cursor: "", history: [] }}
      selected="t1"
      showLogs={false}
      continuing={false}
      message=""
      onFilter={() => {}}
      onSelect={() => {}}
      onPage={() => {}}
      onLogs={() => {}}
      onResume={() => {}}
      onRefresh={() => {}}
    />,
  );
  assert.ok(html.includes("尚无当前任务版本的处理结果"));
  assert.ok(html.includes("处理完成不代表已经送达"));
  assert.ok(html.includes("尚无可核对的用户验收记录"));
  const invalid = renderToStaticMarkup(
    <Tasks
      runtimes={[]}
      tasks={[]}
      next=""
      detail={{ ...detail, can_view_output: false }}
      logs={[]}
      filters={{ status: "all", runtime: "", cursor: "", history: [] }}
      selected="t1"
      showLogs={false}
      continuing={false}
      message=""
      onFilter={() => {}}
      onSelect={() => {}}
      onPage={() => {}}
      onLogs={() => {}}
      onResume={() => {}}
      onRefresh={() => {}}
    />,
  );
  assert.ok(invalid.includes("旧结果已隐藏"));
  const cyber = renderToStaticMarkup(
    <Tasks
      runtimes={[]}
      tasks={[]}
      next=""
      detail={{
        ...detail,
        task: { ...detail.task, mode: "proactive" },
        communications: [
          {
            id: "send-1",
            target_type: "group",
            target_id: "stable-group",
            owner_profile: "bound-profile",
            owner_user_id: "owner-id",
            state: "unknown",
            reason: "需要补充信息",
            content: "<script>确认环境</script>",
            updated_at: "2026-09-17T10:00:00Z",
          },
        ],
      }}
      logs={[]}
      filters={{ status: "all", runtime: "", cursor: "", history: [] }}
      selected="t1"
      showLogs={false}
      continuing={false}
      message=""
      onFilter={() => {}}
      onSelect={() => {}}
      onPage={() => {}}
      onLogs={() => {}}
      onResume={() => {}}
      onRefresh={() => {}}
    />,
  );
  assert.ok(cyber.includes("后台完成仅记录结果"));
  assert.ok(cyber.includes("Agent 沟通记录"));
  assert.ok(cyber.includes("stable-group"));
  assert.ok(cyber.includes("bound-profile"));
  assert.ok(cyber.includes("结果未知"));
  assert.ok(cyber.includes("&lt;script&gt;确认环境&lt;/script&gt;"));
  assert.ok(!cyber.includes("处理完成不代表已经送达"));

  const failed = renderToStaticMarkup(
    <Tasks
      runtimes={[]}
      tasks={[]}
      next=""
      detail={{
        ...detail,
        task: { ...detail.task, status: "failed" },
        deliveries: [
          {
            id: "processing-receipt",
            purpose: "runtime_processing_receipt",
            state: "accepted",
            transport: "bot_group",
            updated_at: "2026-09-17T10:00:00Z",
          },
          {
            id: "completion-receipt",
            purpose: "runtime_completion_receipt",
            state: "accepted",
            transport: "bot_group",
            updated_at: "2026-09-17T10:00:01Z",
          },
          {
            id: "failure-receipt",
            purpose: "runtime_failure_receipt",
            state: "accepted",
            transport: "bot_group",
            updated_at: "2026-09-17T10:00:02Z",
          },
        ],
      }}
      logs={[]}
      filters={{ status: "all", runtime: "", cursor: "", history: [] }}
      selected="t1"
      showLogs={false}
      continuing={false}
      message=""
      onFilter={() => {}}
      onSelect={() => {}}
      onPage={() => {}}
      onLogs={() => {}}
      onResume={() => {}}
      onRefresh={() => {}}
    />,
  );
  assert.ok(failed.includes("处理中标记"));
  assert.ok(failed.includes("完成标记"));
  assert.ok(failed.includes("失败回执"));
  assert.ok(failed.includes("平台已接受"));
});
test("exact dates and all source deadlines remain available after migration", () => {
  const html = renderToStaticMarkup(<Time value="2026-09-16T10:00:00Z" />);
  assert.ok(html.includes('dateTime="2026-09-16T10:00:00.000Z"'));
  assert.ok(html.includes("aria-label="));
  const dates = expiryTimes({
    tasks: { tasks: [{ expires_at: "2026-09-16T10:01:00Z" }] },
    detail: { task: { expires_at: "2026-09-16T10:02:00Z" } },
  } as Parameters<typeof expiryTimes>[0]);
  assert.equal(dates.length, 2);
});
