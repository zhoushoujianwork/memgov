package core

import (
	"context"
	"testing"
	"time"
)

func groupSchedulingFixture(t *testing.T, concurrency int) groupMentionSyncFixture {
	t.Helper()
	f := newGroupMentionSyncFixture(t)
	runtimeMutate(t, f.s, "group.scheduling.routes", func(tx *Tx) (any, error) {
		var err error
		f.config, _, err = tx.SyncGroupMentionRoutes(context.Background(), f.config.ID, []string{f.dwsA.ConversationID, f.dwsB.ConversationID})
		return f.config, err
	})
	if _, err := f.s.DB.Exec("UPDATE runtime_configs SET concurrency=? WHERE id=?", concurrency, f.config.ID); err != nil {
		t.Fatal(err)
	}
	f.config, _ = ReadRuntime(context.Background(), f.s.DB, f.config.ID)
	return f
}

func queueGroupSchedulingTask(t *testing.T, f groupMentionSyncFixture, conversation, sender, id string) RuntimeTask {
	t.Helper()
	ctx := context.Background()
	var message IntakeResult
	runtimeMutate(t, f.s, "group.scheduling.intake", func(tx *Tx) (any, error) {
		var err error
		message, err = tx.Intake(ctx, f.appChannel.ID, NormalizedEvent{Kind: EventMessage, Adapter: "dingtalk_app", ParseVersion: "1", Origin: "stream", ProviderMessageID: id,
			ConversationID: conversation, ConversationType: "group", Tenant: f.appChannel.Tenant, Sender: Sender{IDType: "staff_id", IDValue: sender}, Body: id, Mentioned: true, SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
		return message, err
	})
	runtimeMutate(t, f.s, "group.scheduling.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "group.scheduling.batch", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now())
		return batch, err
	})
	if batch.ID == "" {
		t.Fatal("group task batch unavailable")
	}
	var tasks []RuntimeTask
	runtimeMutate(t, f.s, "group.scheduling.task", func(tx *Tx) (any, error) {
		var err error
		tasks, err = tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "mention:" + message.MessageID, Title: id, Instructions: id, MessageIDs: []string{message.MessageID}}}})
		return tasks, err
	})
	if len(tasks) != 1 {
		t.Fatalf("tasks=%+v", tasks)
	}
	return tasks[0]
}

func claimGroupSchedulingTask(t *testing.T, f groupMentionSyncFixture) (RuntimeTask, RuntimeAttempt) {
	t.Helper()
	var task RuntimeTask
	var attempt RuntimeAttempt
	ctx := WithInMemoryCapacity(context.Background()) // Durable group cap must still apply.
	runtimeMutate(t, f.s, "group.scheduling.claim", func(tx *Tx) (any, error) {
		var err error
		task, attempt, err = tx.ClaimRuntimeTask(ctx, f.config.ID, "", "model", "preset", "commit", "")
		return task, err
	})
	return task, attempt
}

func completeGroupSchedulingTask(t *testing.T, f groupMentionSyncFixture, task RuntimeTask, attempt RuntimeAttempt) {
	t.Helper()
	runtimeMutate(t, f.s, "group.scheduling.complete", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(context.Background(), task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: task.Title, Summary: "done"})
	})
}

func TestGroupSchedulingSkipsBusyRequesterAndKeepsUnreapedCapacity(t *testing.T) {
	f := groupSchedulingFixture(t, 2)
	ctx := context.Background()
	a1 := queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "alice", "a1")
	a2 := queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "alice", "a2")
	b := queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "bob", "b")
	gotA, attemptA := claimGroupSchedulingTask(t, f)
	if gotA.ID != a1.ID {
		t.Fatalf("first task=%s want %s", gotA.ID, a1.ID)
	}
	if ready, err := RuntimeTaskReady(ctx, f.s.DB, f.config.ID); err != nil || !ready {
		t.Fatalf("busy requester blocked independent task: %v %v", ready, err)
	}
	gotB, attemptB := claimGroupSchedulingTask(t, f)
	if gotB.ID != b.ID {
		t.Fatalf("busy alice blocked bob or allowed alice overlap: %+v", gotB)
	}
	other := queueGroupSchedulingTask(t, f, f.dwsB.ConversationID, "alice", "other-group")
	if ready, err := RuntimeTaskReady(ctx, f.s.DB, f.config.ID); err != nil || ready {
		t.Fatalf("full group capacity ready=%v %v", ready, err)
	}
	if task, _ := claimGroupSchedulingTask(t, f); task.ID != "" {
		t.Fatalf("group exceeded finite cap: %+v", task)
	}
	runtimeMutate(t, f.s, "group.scheduling.cancel", func(tx *Tx) (any, error) { return tx.SetRuntimeTaskStatus(ctx, a1.ID, "cancelled") })
	if task, _ := claimGroupSchedulingTask(t, f); task.ID != "" {
		t.Fatal("cancelled but unreaped process released capacity")
	}
	completeGroupSchedulingTask(t, f, gotB, attemptB)
	gotOther, attemptOther := claimGroupSchedulingTask(t, f)
	if gotOther.ID != other.ID {
		t.Fatalf("same person in another group was blocked: %+v", gotOther)
	}
	completeGroupSchedulingTask(t, f, gotOther, attemptOther)
	if ready, err := RuntimeTaskReady(ctx, f.s.DB, f.config.ID); err != nil || ready {
		t.Fatalf("same-requester work overlapped cancelled process: %v %v", ready, err)
	}
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "group.scheduling.late"}, func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(ctx, a1.ID, a1.Version, attemptA.ID, RuntimeAttemptResult{Result: "late"})
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("cancelled result accepted: %v", err)
	}
	runtimeMutate(t, f.s, "group.scheduling.reap", func(tx *Tx) (any, error) { return nil, tx.ReleaseWorkLease(ctx, attemptA.ID) })
	gotA2, _ := claimGroupSchedulingTask(t, f)
	if gotA2.ID != a2.ID {
		t.Fatalf("next alice task did not resume after reaping: %+v", gotA2)
	}
}

func TestGroupSchedulingExplicitOneRemainsSerial(t *testing.T) {
	f := groupSchedulingFixture(t, 1)
	first := queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "alice", "a1")
	second := queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "bob", "b1")
	got, attempt := claimGroupSchedulingTask(t, f)
	if got.ID != first.ID {
		t.Fatal("first task missing")
	}
	if next, _ := claimGroupSchedulingTask(t, f); next.ID != "" {
		t.Fatal("explicit concurrency one ignored")
	}
	completeGroupSchedulingTask(t, f, got, attempt)
	if next, _ := claimGroupSchedulingTask(t, f); next.ID != second.ID {
		t.Fatal("serial capacity was not released")
	}
}

func TestGroupSchedulingActionsShareRequesterLaneAndCapacity(t *testing.T) {
	f := groupSchedulingFixture(t, 2)
	ctx := context.Background()
	prepared := queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "alice", "prepare-action")
	claimed, attempt := claimGroupSchedulingTask(t, f)
	var pending RuntimeTask
	runtimeMutate(t, f.s, "group.scheduling.propose", func(tx *Tx) (any, error) {
		var err error
		pending, err = tx.CompleteRuntimeTask(ctx, claimed.ID, claimed.Version, attempt.ID, RuntimeAttemptResult{Result: "prepared", Actions: []RuntimeAction{{Kind: "git_push", Target: "origin/main", Payload: "push exact prepared commit"}}})
		return pending, err
	})
	if len(pending.Actions) != 1 {
		t.Fatalf("action missing: %+v", pending)
	}
	if _, err := f.s.DB.Exec("UPDATE runtime_pending_actions SET status='confirmed',confirmed_by=? WHERE id=?", f.config.OwnerPrincipalID, pending.Actions[0].ID); err != nil {
		t.Fatal(err)
	}
	ownerWork := queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "alice", "alice-working")
	gotOwner, ownerAttempt := claimGroupSchedulingTask(t, f)
	if gotOwner.ID != ownerWork.ID {
		t.Fatal("requester task did not start")
	}
	if ready, err := RuntimeActionReady(ctx, f.s.DB, f.config.ID); err != nil || ready {
		t.Fatalf("confirmed action overlapped its requester task: %v %v", ready, err)
	}
	runtimeMutate(t, f.s, "group.scheduling.action.busy", func(tx *Tx) (any, error) {
		action, _, err := tx.ClaimRuntimeAction(WithInMemoryCapacity(ctx), f.config.ID, "", "model")
		if action.ID != "" {
			t.Error("action claim bypassed the requester lane")
		}
		return action, err
	})
	bob := queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "bob", "bob-independent")
	gotBob, bobAttempt := claimGroupSchedulingTask(t, f)
	if gotBob.ID != bob.ID {
		t.Fatal("unrelated requester task did not start")
	}
	completeGroupSchedulingTask(t, f, gotOwner, ownerAttempt)
	if ready, err := RuntimeActionReady(ctx, f.s.DB, f.config.ID); err != nil || !ready {
		t.Fatalf("independent requester blocked a ready action: %v %v", ready, err)
	}
	a2 := queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "alice", "alice-followup")
	var action RuntimePendingAction
	var actionAttempt RuntimeActionAttempt
	runtimeMutate(t, f.s, "group.scheduling.action", func(tx *Tx) (any, error) {
		var err error
		action, actionAttempt, err = tx.ClaimRuntimeAction(WithInMemoryCapacity(ctx), f.config.ID, "", "model")
		return action, err
	})
	if action.TaskID != prepared.ID {
		t.Fatalf("action not claimed: %+v", action)
	}
	completeGroupSchedulingTask(t, f, gotBob, bobAttempt)
	if ready, err := RuntimeTaskReady(ctx, f.s.DB, f.config.ID); err != nil || ready {
		t.Fatalf("action lane allowed overlapping followup: %v %v", ready, err)
	}
	runtimeMutate(t, f.s, "group.scheduling.action.unknown", func(tx *Tx) (any, error) {
		return tx.UnknownRuntimeAction(ctx, action.ID, actionAttempt.ID, action.TaskVersion, "connection_lost")
	})
	if got, _ := claimGroupSchedulingTask(t, f); got.ID != "" {
		t.Fatal("unknown unreaped action released requester lane")
	}
	runtimeMutate(t, f.s, "group.scheduling.action.reap", func(tx *Tx) (any, error) { return nil, tx.ReleaseWorkLease(ctx, actionAttempt.ID) })
	if got, _ := claimGroupSchedulingTask(t, f); got.ID != a2.ID {
		t.Fatal("followup remained blocked after action reaping")
	}
	if ready, err := RuntimeActionReady(ctx, f.s.DB, f.config.ID); err != nil || ready {
		t.Fatalf("unknown action became automatically retryable: %v %v", ready, err)
	}
}
