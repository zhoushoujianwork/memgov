package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

type deliveryReactionAdapter struct {
	fakeAdapter
	sendState string
	added     []string
	removed   []string
}

func (a *deliveryReactionAdapter) Send(context.Context, channel.Config, channel.SendRequest) (channel.SendResult, error) {
	a.sends++
	return channel.SendResult{State: a.sendState, Receipt: "local-receipt"}, nil
}

func (a *deliveryReactionAdapter) AddReaction(_ context.Context, _ channel.Config, req channel.ReactionRequest) error {
	a.added = append(a.added, req.Emoji)
	return nil
}

func (a *deliveryReactionAdapter) RemoveReaction(_ context.Context, _ channel.Config, req channel.ReactionRequest) error {
	a.removed = append(a.removed, req.Emoji)
	return nil
}

func TestProactiveCompletionNeverCallsNotificationAdapter(t *testing.T) {
	for _, state := range []string{"accepted", "failed", "unknown"} {
		t.Run(state, func(t *testing.T) {
			service, cfg, preset, _, _ := setupService(t)
			adapter := &deliveryReactionAdapter{sendState: state}
			service.Adapter = adapter
			ctx := context.Background()
			_, err := service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "message.intake"}, func(tx *core.Tx) (any, error) {
				return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderEventID: "request", ProviderMessageID: "question", ConversationID: "watch", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "alice"}, Body: "answer this", SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
			})
			if err != nil {
				t.Fatal(err)
			}
			service.tick(ctx, cfg, preset)
			if adapter.sends != 0 {
				t.Fatalf("send count: %d", adapter.sends)
			}
			var notifications int
			if err = service.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM outbox WHERE job_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)", cfg.ID).Scan(&notifications); err != nil || notifications != 0 {
				t.Fatalf("record-only completion generated %d notifications: %v", notifications, err)
			}
			if len(adapter.added) != 0 || len(adapter.removed) != 0 {
				t.Fatalf("observation exposed message reactions: added=%v removed=%v", adapter.added, adapter.removed)
			}
		})
	}
}

type failOnceExecutor struct{ calls int }

func (e *failOnceExecutor) Execute(context.Context, ExecutionInput) (core.RuntimeAttemptResult, error) {
	e.calls++
	if e.calls == 1 {
		return core.RuntimeAttemptResult{}, core.Fail("unavailable", "temporary local execution failure")
	}
	return core.RuntimeAttemptResult{Result: "已回复", Summary: "重试成功"}, nil
}

func TestFailedAndRetriedTasksKeepStatusInLocalAudit(t *testing.T) {
	service, cfg, preset, _, _ := setupService(t)
	adapter := &deliveryReactionAdapter{sendState: "accepted"}
	service.Adapter = adapter
	executor := &failOnceExecutor{}
	service.Executor = executor
	ctx := context.Background()
	_, err := service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "message.intake.retry"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1",
			Origin: "stream", ProviderMessageID: "retry-question", ConversationID: "watch", Tenant: "corp",
			Sender: core.Sender{IDType: "user_id", IDValue: "alice"}, Body: "answer this",
			SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	service.tick(ctx, cfg, preset)
	failed, err := core.RuntimeTaskList(ctx, service.Store.DB, cfg.ID, "failed", 10)
	if err != nil || len(failed) != 1 || len(adapter.added) != 0 {
		t.Fatalf("first attempt was not marked failed: tasks=%+v added=%v error=%v", failed, adapter.added, err)
	}
	_, err = service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "runtime.task.retry"}, func(tx *core.Tx) (any, error) {
		return tx.SetRuntimeTaskStatus(ctx, failed[0].ID, "pending")
	})
	if err != nil {
		t.Fatal(err)
	}
	service.tick(ctx, cfg, preset)
	if adapter.sends != 0 || executor.calls != 2 {
		t.Fatalf("retry did not record success without notification: sends=%d executions=%d added=%v", adapter.sends, executor.calls, adapter.added)
	}
	if len(adapter.added) != 0 || len(adapter.removed) != 0 {
		t.Fatalf("retry exposed message reactions: added=%v removed=%v", adapter.added, adapter.removed)
	}
}
