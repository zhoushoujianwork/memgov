package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

type groupBarrierExecutor struct {
	started   chan ExecutionInput
	release   chan struct{}
	cancelled chan struct{}
}

func (e *groupBarrierExecutor) Execute(ctx context.Context, in ExecutionInput) (core.RuntimeAttemptResult, error) {
	if len(in.Task.Messages) != 1 {
		return core.RuntimeAttemptResult{}, fmt.Errorf("expected one trigger, got %d", len(in.Task.Messages))
	}
	e.started <- in
	id := in.Task.Messages[0].ProviderMessageID
	if id == "alice-1" {
		select {
		case <-e.release:
		case <-ctx.Done():
			close(e.cancelled)
			return core.RuntimeAttemptResult{}, ctx.Err()
		}
	}
	return core.RuntimeAttemptResult{Result: "reply:" + id, Summary: "finished " + id}, nil
}

func TestGroupServiceRunsIndependentRequestersWhilePreservingFollowupOrder(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish-first", true: "cancel-first"}[cancelFirst], func(t *testing.T) {
			service, cfg, app, group, _ := setupGroupMentionService(t)
			if _, err := service.Store.DB.Exec("UPDATE runtime_configs SET concurrency=2 WHERE id=?", cfg.ID); err != nil {
				t.Fatal(err)
			}
			models := &fakeModels{}
			executor := &groupBarrierExecutor{started: make(chan ExecutionInput, 4), release: make(chan struct{}), cancelled: make(chan struct{})}
			service.Analyzer, service.Executor, service.Actioner = models, executor, models
			service.ExternalReceiver = true
			service.Tick = time.Hour
			wake := make(chan struct{}, 1)
			service.Wake = wake
			source := &blockingGroupSource{started: make(chan struct{}, 1), release: make(chan struct{})}
			service.GroupSource = source
			if _, err := agent.Enable(context.Background(), service.Home, "claude", "claude-default"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- service.Run(ctx, cfg.ID) }()
			defer func() {
				cancel()
				close(source.release)
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("runtime stop: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("runtime did not stop")
				}
			}()
			select {
			case <-source.started:
			case <-time.After(5 * time.Second):
				t.Fatal("runtime did not start")
			}
			intake := func(id, sender string) {
				t.Helper()
				_, err := service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "group.concurrent.intake"}, func(tx *core.Tx) (any, error) {
					return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: id,
						ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: sender}, Mentioned: true, Body: "handle " + id, SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
				})
				if err != nil {
					t.Fatal(err)
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			}
			next := func(want string) ExecutionInput {
				t.Helper()
				select {
				case in := <-executor.started:
					if in.Task.Messages[0].ProviderMessageID != want {
						t.Fatalf("started %s before %s", in.Task.Messages[0].ProviderMessageID, want)
					}
					return in
				case <-time.After(8 * time.Second):
					t.Fatalf("%s did not start independently", want)
					return ExecutionInput{}
				}
			}
			waitState := func(id, want string) {
				t.Helper()
				deadline := time.Now().Add(8 * time.Second)
				for {
					task, err := core.ReadRuntimeTask(ctx, service.Store.DB, id)
					if err == nil && task.Status == want {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("task %s state=%s error=%v, want %s", id, task.Status, err, want)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			intake("alice-1", "alice")
			first := next("alice-1")
			intake("alice-2", "alice")
			intake("bob-1", "bob")
			second := next("bob-1")
			waitState(second.Task.ID, "completed")
			firstState, err := core.ReadRuntimeTask(ctx, service.Store.DB, first.Task.ID)
			if err != nil || firstState.Status != "running" {
				t.Fatalf("alice did not remain running while bob completed: %+v %v", firstState, err)
			}
			select {
			case extra := <-executor.started:
				t.Fatalf("same requester overlapped alice-1: %+v", extra.Task)
			default:
			}
			if first.AttemptID == second.AttemptID || first.Task.ID == second.Task.ID || first.WorkDir == second.WorkDir {
				t.Fatal("independent requesters shared execution identity or scratch directory")
			}
			adapter := service.Adapter.(*fakeAdapter)
			var reply channel.SendRequest
			deadline := time.Now().Add(8 * time.Second)
			for {
				adapter.mu.Lock()
				for _, request := range adapter.requests {
					if request.ReplyTo == "bob-1" {
						reply = request
					}
				}
				adapter.mu.Unlock()
				if reply.ReplyTo != "" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("bob reply waited for alice's long task")
				}
				time.Sleep(10 * time.Millisecond)
			}
			var card core.RuntimeCard
			if err := json.Unmarshal([]byte(reply.Content), &card); err != nil {
				t.Fatal(err)
			}
			if reply.ConversationID != group.ConversationID || len(card.Mentions) != 1 || card.Mentions[0].IDValue != "bob" {
				t.Fatalf("reply routed to wrong requester: %+v %+v", reply, card)
			}
			if cancelFirst {
				_, err := service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "group.concurrent.cancel"}, func(tx *core.Tx) (any, error) { return tx.SetRuntimeTaskStatus(ctx, first.Task.ID, "cancelled") })
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-executor.cancelled:
				case <-time.After(5 * time.Second):
					t.Fatal("cancelled requester process did not stop")
				}
			} else {
				close(executor.release)
			}
			followup := next("alice-2")
			waitState(followup.Task.ID, "completed")
			if followup.WorkDir == first.WorkDir || followup.WorkDir == second.WorkDir || followup.AttemptID == first.AttemptID {
				t.Fatal("followup reused another request's execution identity")
			}
		})
	}
}

type groupActionFunction func(context.Context, ActionExecutionInput) (core.RuntimeAttemptResult, error)

func (f groupActionFunction) ExecuteConfirmedAction(ctx context.Context, in ActionExecutionInput) (core.RuntimeAttemptResult, error) {
	return f(ctx, in)
}

func queueGroupActionLifecycleTask(t *testing.T, s *Service, cfg core.RuntimeConfig, app core.Channel, group core.Route, id, sender string) core.RuntimeTask {
	t.Helper()
	ctx := context.Background()
	var tasks []core.RuntimeTask
	err := s.mutate(ctx, "global", "group.action-lifecycle.queue", func(tx *core.Tx) (any, error) {
		message, err := tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: id,
			ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: sender}, Mentioned: true, Body: id, SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SyncRuntimeMessages(ctx, cfg.ID); err != nil {
			return nil, err
		}
		batch, err := tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now())
		if err != nil {
			return nil, err
		}
		tasks, err = tx.CompleteRuntimeBatch(ctx, batch, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "mention:" + message.MessageID, Title: id, Instructions: id, MessageIDs: []string{message.MessageID}}}})
		return tasks, err
	})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("queue task: %+v %v", tasks, err)
	}
	return tasks[0]
}

func claimedGroupActionLifecycleFixture(t *testing.T) (*Service, core.RuntimeConfig, agent.Preset, core.RuntimePendingAction, core.RuntimeActionAttempt, core.RuntimeTask, core.RuntimeTask) {
	t.Helper()
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	models := &fakeModels{}
	s.Analyzer, s.Executor, s.Actioner = models, models, models
	preset, err := agent.Enable(ctx, s.Home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	queueGroupActionLifecycleTask(t, s, cfg, app, group, "action-request", "alice")
	var action core.RuntimePendingAction
	var attempt core.RuntimeActionAttempt
	err = s.mutate(ctx, "global", "group.action-lifecycle.claim", func(tx *core.Tx) (any, error) {
		task, first, err := tx.ClaimRuntimeTask(ctx, cfg.ID, "", "fake", preset.Name, preset.Commit, t.TempDir())
		if err != nil {
			return nil, err
		}
		task, err = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, first.ID, core.RuntimeAttemptResult{Result: "prepared", Actions: []core.RuntimeAction{{Kind: "external_change", Target: "fixture", Payload: "approved fixture operation"}}})
		if err != nil {
			return nil, err
		}
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='confirmed',confirmed_by=?,confirmation_origin='test' WHERE task_id=?", cfg.OwnerPrincipalID, task.ID); err != nil {
			return nil, err
		}
		action, attempt, err = tx.ClaimRuntimeAction(ctx, cfg.ID, "", "fake")
		return action, err
	})
	if err != nil || action.ID == "" {
		t.Fatalf("claim confirmed group action: %+v %v", action, err)
	}
	followup := queueGroupActionLifecycleTask(t, s, cfg, app, group, "alice-followup", "alice")
	other := queueGroupActionLifecycleTask(t, s, cfg, app, group, "bob-request", "bob")
	return s, cfg, preset, action, attempt, followup, other
}

func TestGroupConfirmedActionReleasesCapacityAfterEveryServiceExit(t *testing.T) {
	for _, outcome := range []string{"completed", "failed-before-invocation", "unknown-after-invocation"} {
		t.Run(outcome, func(t *testing.T) {
			s, cfg, preset, action, attempt, followup, other := claimedGroupActionLifecycleFixture(t)
			ctx := context.Background()
			invocations := 0
			s.Actioner = groupActionFunction(func(context.Context, ActionExecutionInput) (core.RuntimeAttemptResult, error) {
				invocations++
				if outcome == "unknown-after-invocation" {
					return core.RuntimeAttemptResult{}, core.Fail("unavailable", "fixture connection lost")
				}
				return core.RuntimeAttemptResult{Result: "confirmed fixture finished"}, nil
			})
			if outcome == "failed-before-invocation" {
				s.HarnessName = "incompatible-fixture-harness"
			}
			s.executeClaimedAction(ctx, cfg, preset, action, attempt)
			s.HarnessName = ""
			var released int
			if err := s.Store.DB.QueryRow("SELECT released FROM runtime_work_leases WHERE id=?", attempt.ID).Scan(&released); err != nil || released != 1 {
				t.Fatalf("finished action retained group capacity: released=%d err=%v", released, err)
			}
			var status string
			if err := s.Store.DB.QueryRow("SELECT status FROM runtime_pending_actions WHERE id=?", action.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"completed": "executed", "failed-before-invocation": "failed", "unknown-after-invocation": "unknown"}[outcome]
			if status != want || (outcome == "failed-before-invocation" && invocations != 0) {
				t.Fatalf("action outcome=%s want=%s invocations=%d", status, want, invocations)
			}
			for _, task := range []core.RuntimeTask{followup, other} {
				if !s.executeOne(ctx, cfg, preset) {
					t.Fatal("finished action blocked a subsequent requester task")
				}
				current, err := core.ReadRuntimeTask(ctx, s.Store.DB, task.ID)
				if err != nil || current.Status != "completed" {
					t.Fatalf("subsequent requester task did not complete: %+v %v", current, err)
				}
			}
			if s.executeOneConfirmedAction(ctx, cfg, preset) {
				t.Fatal("completed/failed/unknown action was dispatched again")
			}
		})
	}
}

func TestGroupConfirmedActionCancellationHoldsCapacityUntilExecutorReturns(t *testing.T) {
	s, cfg, preset, action, attempt, followup, _ := claimedGroupActionLifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	started, cancelled, reaped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.Actioner = groupActionFunction(func(ctx context.Context, _ ActionExecutionInput) (core.RuntimeAttemptResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-reaped // Process cleanup has not finished merely because cancellation arrived.
		return core.RuntimeAttemptResult{}, ctx.Err()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.executeClaimedAction(ctx, cfg, preset, action, attempt)
	}()
	defer func() {
		cancel()
		select {
		case <-reaped:
		default:
			close(reaped)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("confirmed-action worker did not stop")
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("confirmed-action worker did not start")
	}
	if err := s.mutate(ctx, "global", "group.action-lifecycle.cancel", func(tx *core.Tx) (any, error) { return tx.SetRuntimeTaskStatus(ctx, action.TaskID, "cancelled") }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("task cancellation did not reach the confirmed-action worker")
	}
	if ready, err := core.RuntimeTaskReady(ctx, s.Store.DB, cfg.ID); err != nil || ready {
		t.Fatalf("unreaped confirmed action released group capacity: %v %v", ready, err)
	}
	var released int
	if err := s.Store.DB.QueryRow("SELECT released FROM runtime_work_leases WHERE id=?", attempt.ID).Scan(&released); err != nil || released != 0 {
		t.Fatalf("unreaped action lease=%d error=%v", released, err)
	}
	close(reaped)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reaped action did not finish")
	}
	if !s.executeOne(ctx, cfg, preset) {
		t.Fatal("reaped action retained its requester lane")
	}
	current, err := core.ReadRuntimeTask(ctx, s.Store.DB, followup.ID)
	if err != nil || current.Status != "completed" {
		t.Fatalf("followup did not finish after cancellation cleanup: %+v %v", current, err)
	}
	if s.executeOneConfirmedAction(ctx, cfg, preset) {
		t.Fatal("cancelled unknown action was retried")
	}
}
