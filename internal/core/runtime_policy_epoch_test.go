package core

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func policyEpochTask(t *testing.T, f runtimeFixture) RuntimeTask {
	t.Helper()
	ctx := context.Background()
	msg := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "policy-request", f.owner, "please inspect", time.Now().Add(time.Second))
	runtimeMutate(t, f.s, "policy.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "policy.batch.claim", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now().Add(time.Second))
		return batch, err
	})
	if len(batch.Messages) != 1 || batch.Messages[0].ID != msg.MessageID {
		t.Fatalf("policy task batch: %+v", batch)
	}
	var tasks []RuntimeTask
	runtimeMutate(t, f.s, "policy.batch.complete", func(tx *Tx) (any, error) {
		var err error
		tasks, err = tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "policy:request", Title: "inspect", Instructions: "inspect", MessageIDs: []string{msg.MessageID}}}})
		return tasks, err
	})
	if len(tasks) != 1 {
		t.Fatalf("policy tasks: %+v", tasks)
	}
	return tasks[0]
}

func commitPolicyVersion(t *testing.T, s *Store, expected int) {
	t.Helper()
	runtimeMutate(t, s, "policy.version.commit", func(tx *Tx) (any, error) {
		return tx.CommitAppliedConfig(context.Background(), expected, 1, []byte(fmt.Sprintf(`{"agents":{"epoch":%d}}`, expected+1)), []ManagedConfigObject{})
	})
}

func TestExecutionAttemptPinsAppliedConfigurationEpoch(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 1, 300)
	policyEpochTask(t, f)
	var task RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "policy.task.claim", func(tx *Tx) (any, error) {
		var err error
		task, attempt, err = tx.ClaimRuntimeTask(ctx, f.config.ID, "policy-attempt", "model", "preset", "commit", "")
		return task, err
	})
	if attempt.AppliedConfigVersion != 0 {
		t.Fatalf("expected standalone epoch zero: %+v", attempt)
	}
	commitPolicyVersion(t, f.s, 0)
	if err := RuntimeAttemptPolicyCurrent(ctx, f.s.DB, attempt.ID, task.ID, task.Version); ErrorCode(err) != "conflict" {
		t.Fatalf("policy change did not stop execution: %v", err)
	}
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "policy.task.stale-complete"}, func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "stale result"}, "")
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("stale result was committed: %v", err)
	}
	var stored string
	if err = f.s.DB.QueryRowContext(ctx, "SELECT result FROM runtime_tasks WHERE id=?", task.ID).Scan(&stored); err != nil || stored != "" {
		t.Fatalf("stale result persisted %q, %v", stored, err)
	}
}

func TestConfirmedActionAttemptPinsAppliedConfigurationEpoch(t *testing.T) {
	ctx := context.Background()
	f := newOwnerInteractiveFixture(t)
	policyEpochTask(t, f)
	commitPolicyVersion(t, f.s, 0)
	var task RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "policy.action.task.claim", func(tx *Tx) (any, error) {
		var err error
		task, attempt, err = tx.ClaimRuntimeTask(ctx, f.config.ID, "policy-action-attempt", "model", "preset", "commit", "")
		return task, err
	})
	if attempt.AppliedConfigVersion != 1 {
		t.Fatalf("attempt did not pin epoch one: %+v", attempt)
	}
	var completed RuntimeTask
	runtimeMutate(t, f.s, "policy.action.task.complete", func(tx *Tx) (any, error) {
		var err error
		completed, err = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "prepared", Actions: []RuntimeAction{{Kind: "git_push", Target: "origin/main", Payload: "push commit"}}}, "")
		return completed, err
	})
	action := completed.Actions[0]
	intakeRuntimeMessage(t, f.s, f.channel, f.direct.ConversationID, "policy-owner-confirm", f.owner, ConfirmationToken(action), time.Now().Add(time.Hour))
	runtimeMutate(t, f.s, "policy.action.confirm", func(tx *Tx) (any, error) { return tx.ProcessRuntimeConfirmations(ctx, f.config.ID) })
	var claimed RuntimePendingAction
	var actionAttempt RuntimeActionAttempt
	runtimeMutate(t, f.s, "policy.action.claim", func(tx *Tx) (any, error) {
		var err error
		claimed, actionAttempt, err = tx.ClaimRuntimeAction(ctx, f.config.ID, "policy-action-exec", "model")
		return claimed, err
	})
	if claimed.ID != action.ID || actionAttempt.AppliedConfigVersion != 1 {
		t.Fatalf("action attempt epoch: %+v %+v", claimed, actionAttempt)
	}
	commitPolicyVersion(t, f.s, 1)
	if err := RuntimeActionAttemptPolicyCurrent(ctx, f.s.DB, actionAttempt.ID, action.ID, action.TaskVersion); ErrorCode(err) != "conflict" {
		t.Fatalf("policy change did not stop action execution: %v", err)
	}
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "policy.action.stale-complete"}, func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeAction(ctx, action.ID, actionAttempt.ID, action.TaskVersion, RuntimeAttemptResult{Result: "stale external result"})
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("stale action result was committed: %v", err)
	}
}
