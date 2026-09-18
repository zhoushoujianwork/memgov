package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func intakeDirectTest(t *testing.T, f directScenario, key, body, sender string) {
	t.Helper()
	_, err := f.service.Store.Mutate(context.Background(), core.Request{Scope: "global", Command: "direct.test.intake"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(context.Background(), f.cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/4", Origin: "stream", ProviderMessageID: key, ConversationID: f.route.ConversationID, ConversationType: "direct", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: sender}, Body: body, SentAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDirectSystemCommandsAndSessionRecovery(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	runtimeTestSkill(t, filepath.Join(userHome, ".claude", "skills"), "clawflow")
	f := runDirectReplyScenario(t, "原始问题\n请保留换行。", false, nil, false)
	ctx := context.Background()
	declaration, _ := json.Marshal(map[string]any{"applications": map[string]any{"owner_private": map[string]any{"enabled": true, "runtime": f.cfg.Name, "agent": ""}}})
	if _, err := f.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.test.apply"}, func(tx *core.Tx) (any, error) {
		return tx.CommitAppliedConfig(ctx, 0, 1, declaration, []core.ManagedConfigObject{{Kind: "application", Name: "owner_private", ObjectType: "runtime", ObjectID: f.cfg.ID}})
	}); err != nil {
		t.Fatal(err)
	}
	first := f.executor.inputs[0]
	if first.SessionID == "" || first.Task.Messages[0].Body != "原始问题\n请保留换行。" || len(first.MemoryContext) != 0 {
		t.Fatal("direct transport modified the owner text or injected memory")
	}
	for _, expected := range []string{"one-to-one DingTalk chat", "Do not search all memgov memory first", `profile "corp:owner"`} {
		if !strings.Contains(first.ChannelSystemPrompt, expected) {
			t.Fatalf("owner direct transport omitted channel behavior %q: %s", expected, first.ChannelSystemPrompt)
		}
	}
	intakeDirectTest(t, f, "status", "/status", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	if f.executor.calls != 1 || f.analyzer.calls != 0 || f.adapter.sends != 2 {
		t.Fatal("status invoked a model or failed to reply")
	}
	var statusResult string
	if err := f.service.Store.DB.QueryRow(`SELECT t.result FROM runtime_tasks t
JOIN runtime_task_messages tm ON tm.task_id=t.id JOIN messages m ON m.id=tm.message_id
JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision
WHERE t.runtime_id=? AND trim(mr.body)='/status' ORDER BY t.created_at DESC LIMIT 1`, f.cfg.ID).Scan(&statusResult); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Agent：owner-default", "技能继承：executor", "clawflow", "memgov-memory", "能力：artifact_create、conversation_history_read、local_read、local_test、local_write、memory_read", "Bash：true", "外部操作：owner_request", "记忆范围：owner_authorized", "普通私聊不会自动召回全部记忆"} {
		if !strings.Contains(statusResult, expected) {
			t.Fatalf("status omitted %q: %s", expected, statusResult)
		}
	}
	intakeDirectTest(t, f, "unknown", "/clear 请解释这个命令", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	if f.executor.calls != 2 || f.executor.inputs[1].SessionID != first.SessionID || len(f.executor.contexts[1]) != 2 {
		t.Fatal("non-command slash text was intercepted or status polluted conversation")
	}
	// A fresh executor models process restart: the durable session and delivered
	// owner/bot turns reconstruct the same conversation.
	restarted := &modelReportingExecutor{}
	f.service.Executor = restarted
	intakeDirectTest(t, f, "restart", "继续", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	if restarted.calls != 1 || restarted.inputs[0].SessionID != first.SessionID || len(restarted.contexts[0]) != 4 {
		t.Fatal("restart lost the logical session or accepted history")
	}
	intakeDirectTest(t, f, "clear", "/clear", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	intakeDirectTest(t, f, "clear", "/clear", "owner") // repeated provider frame
	f.service.tick(ctx, f.cfg, f.preset)
	var sessions int
	if err := f.service.Store.DB.QueryRow("SELECT count(*) FROM runtime_direct_sessions WHERE runtime_id=?", f.cfg.ID).Scan(&sessions); err != nil || sessions != 2 || restarted.calls != 1 {
		t.Fatalf("clear was not idempotent or invoked a model: sessions=%d err=%v", sessions, err)
	}
	intakeDirectTest(t, f, "after-clear", "你好", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	if restarted.calls != 2 || restarted.inputs[1].SessionID == first.SessionID || len(restarted.contexts[1]) != 0 {
		t.Fatal("clear leaked the previous conversation")
	}
}

func TestDirectClearDuringExecutionBlocksOldResult(t *testing.T) {
	f := runDirectReplyScenario(t, "第一轮", false, nil, false)
	ctx := context.Background()
	f.executor.onExecute = func(ExecutionInput) {
		intakeDirectTest(t, f, "clear-during-execution", "/clear", "owner")
	}
	intakeDirectTest(t, f, "long-turn", "正在处理的原始问题", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	if f.adapter.sends != 1 {
		t.Fatal("obsolete reply was delivered after clear arrived")
	}
	tasks, err := core.RuntimeTaskList(ctx, f.service.Store.DB, f.cfg.ID, "failed", 10)
	if err != nil || len(tasks) != 1 || tasks[0].ErrorCode != "conflict" {
		t.Fatalf("old turn did not record the conflict: tasks=%v err=%v", tasks, err)
	}
	f.executor.onExecute = nil
	f.service.tick(ctx, f.cfg, f.preset)
	if f.adapter.sends != 2 || f.executor.calls != 2 {
		t.Fatal("clear did not get its system acknowledgement")
	}
}

func TestDirectForeignClearCannotResetOwnerSession(t *testing.T) {
	f := runDirectReplyScenario(t, "第一轮", false, nil, false)
	f.executor.onExecute = func(ExecutionInput) {
		intakeDirectTest(t, f, "foreign-clear", "/clear", "other")
	}
	intakeDirectTest(t, f, "next", "继续", "owner")
	f.service.tick(context.Background(), f.cfg, f.preset)
	if f.adapter.sends != 2 || f.executor.inputs[1].SessionID != f.executor.inputs[0].SessionID {
		t.Fatal("unverified sender reset the owner's session")
	}
}

func TestDirectRecalledHistoryIsExcluded(t *testing.T) {
	f := runDirectReplyScenario(t, "即将撤回的私聊", false, nil, false)
	ctx := context.Background()
	_, err := f.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.test.recall"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, f.cfg.ChannelID, core.NormalizedEvent{Kind: core.EventRecall, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/4", Origin: "stream", ProviderMessageID: "direct-request", ConversationID: f.route.ConversationID, ConversationType: "direct", Tenant: "corp", RecalledAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	intakeDirectTest(t, f, "after-recall", "新的问题", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	if f.executor.calls != 2 || len(f.executor.contexts[1]) != 0 {
		t.Fatal("recalled private text remained in recovery history")
	}
}

func TestDirectActionToolPreparesOnceAndRejectsStaleAttempt(t *testing.T) {
	f := runDirectReplyScenario(t, "第一轮", false, nil, false)
	ctx := context.Background()
	var taskID, attemptID, actionID string
	f.executor.onExecute = func(in ExecutionInput) {
		taskID = in.Task.ID
		if err := f.service.Store.DB.QueryRow("SELECT id FROM runtime_attempts WHERE task_id=? AND status='running'", taskID).Scan(&attemptID); err != nil {
			t.Fatal(err)
		}
		proposal := core.RuntimeActionProposal{AttemptID: attemptID, Kind: "send_message", Target: "requested-colleague", Payload: "已准备的具体内容"}
		for i := 0; i < 2; i++ {
			_, err := f.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.test.action"}, func(tx *core.Tx) (any, error) {
				out, e := tx.ProposeRuntimeAction(ctx, taskID, proposal)
				if e == nil {
					id := out["action_id"].(string)
					if actionID != "" && actionID != id {
						t.Fatal("repeat tool proposal duplicated the action")
					}
					actionID = id
				}
				return out, e
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	intakeDirectTest(t, f, "propose", "请准备发给同事的消息", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	task, err := core.ReadRuntimeTask(ctx, f.service.Store.DB, taskID)
	if err != nil || task.Status != "awaiting_confirmation" || len(task.Actions) != 1 || task.Actions[0].Status != "pending" {
		t.Fatalf("prepared tool action bypassed confirmation: task=%v err=%v", task, err)
	}
	var content string
	if err = f.service.Store.DB.QueryRow("SELECT content FROM outbox WHERE job_id=? AND state='accepted' AND reason NOT IN ('runtime_receipt','runtime_processing_receipt','runtime_completion_receipt','runtime_failure_receipt')", taskID).Scan(&content); err != nil || !strings.Contains(content, core.ConfirmationToken(task.Actions[0])) {
		t.Fatal("concrete action was not displayed with its bound confirmation")
	}
	_, err = f.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.test.stale.action"}, func(tx *core.Tx) (any, error) {
		return tx.ProposeRuntimeAction(ctx, taskID, core.RuntimeActionProposal{AttemptID: attemptID, Kind: "send_message", Target: "other", Payload: "late"})
	})
	if core.ErrorCode(err) != "denied" {
		t.Fatalf("completed attempt proposed new operation: %v", err)
	}
}
