package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRuntimeTaskClaimSkipsWithdrawnGroupAndContinuesAdmittedWork(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	old := createRuntimeTask(t, f, "old-group")
	var second Route
	runtimeMutate(t, f.s, "scope.second.group", func(tx *Tx) (any, error) {
		var err error
		second, err = tx.AddRoute(ctx, f.channel.ID, RouteInput{ConversationID: "cid:second", ConversationType: "group", Mode: "collect"})
		if err != nil {
			return nil, err
		}
		_, _, err = tx.SyncRuntimeGroupRoutes(ctx, f.config.ID, []string{f.watch.ConversationID, second.ConversationID})
		return nil, err
	})
	msg := intakeRuntimeMessage(t, f.s, f.channel, second.ConversationID, "task-second-group", Sender{IDType: "union_id", IDValue: "bob"}, "please answer in the second group", time.Now().Add(2*time.Hour))
	runtimeMutate(t, f.s, "scope.second.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "scope.second.batch", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now().Add(3*time.Hour))
		return batch, err
	})
	if batch.RouteID != second.ID {
		t.Fatalf("second group was not analyzed independently: %+v", batch)
	}
	var tasks []RuntimeTask
	runtimeMutate(t, f.s, "scope.second.task", func(tx *Tx) (any, error) {
		var err error
		tasks, err = tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "second-group", Title: "Answer second", Instructions: "Answer this group", MessageIDs: []string{msg.MessageID}}}})
		return tasks, err
	})
	runtimeMutate(t, f.s, "scope.narrow.second", func(tx *Tx) (any, error) {
		_, _, err := tx.SyncRuntimeGroupRoutes(ctx, f.config.ID, []string{second.ConversationID})
		return nil, err
	})
	var claimed RuntimeTask
	runtimeMutate(t, f.s, "scope.claim.second", func(tx *Tx) (any, error) {
		var err error
		claimed, _, err = tx.ClaimRuntimeTask(ctx, f.config.ID, "scope-attempt", "model", "preset", "commit", "")
		return claimed, err
	})
	if claimed.ID != tasks[0].ID || claimed.ID == old.ID {
		t.Fatalf("withdrawn task took worker capacity: old=%s claimed=%s admitted=%s", old.ID, claimed.ID, tasks[0].ID)
	}
	old, err := ReadRuntimeTask(ctx, f.s.DB, old.ID)
	if err != nil || old.Status != "pending" || len(old.Attempts) != 0 {
		t.Fatalf("withdrawn old task was executed: %+v error=%v", old, err)
	}
}

func TestRuntimeTriggerWithdrawalBlocksOwnerDeliveryAndReadyOutbox(t *testing.T) {
	f := newOwnerInteractiveFixture(t)
	ctx := context.Background()
	runtimeMutate(t, f.s, "scope.send.capability", func(tx *Tx) (any, error) {
		_, err := tx.SetChannelCapabilities(ctx, f.channel.ID, Capabilities{Verified: map[string]bool{"send": true}}, "fake")
		return nil, err
	})
	task := createRuntimeTask(t, f, "result-before-withdrawal")
	var claimed RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "scope.task.claim", func(tx *Tx) (any, error) {
		var err error
		claimed, attempt, err = tx.ClaimRuntimeTask(ctx, f.config.ID, "scope-running", "model", "preset", "commit", "")
		return claimed, err
	})
	runtimeMutate(t, f.s, "scope.task.complete", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(ctx, claimed.ID, claimed.Version, attempt.ID, RuntimeAttemptResult{Result: "Verified answer", Summary: "Done"}, "")
	})
	var ready OutboxView
	runtimeMutate(t, f.s, "scope.delivery.ready", func(tx *Tx) (any, error) {
		var err error
		ready, err = tx.PrepareTaskDelivery(ctx, task.ID)
		return ready, err
	})
	before, err := PreviewDraft(ctx, f.s.DB, ready.ID)
	if err != nil || !before.Sendable || ready.RouteID != f.direct.ID {
		t.Fatalf("valid owner direct delivery was blocked: preview=%+v outbox=%+v error=%v", before, ready, err)
	}
	runtimeMutate(t, f.s, "scope.withdraw.all", func(tx *Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET route_ids='[]' WHERE id=?", f.config.ID)
		return nil, err
	})
	_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "scope.delivery.denied"}, func(tx *Tx) (any, error) {
		return tx.PrepareTaskDelivery(ctx, task.ID)
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("old result was prepared through retained owner direct route: %v", err)
	}
	after, err := PreviewDraft(ctx, f.s.DB, ready.ID)
	if err != nil || after.Sendable {
		t.Fatalf("ready outbox remained dispatchable after group withdrawal: %+v error=%v", after, err)
	}
	var sawTriggerFailure bool
	for _, check := range after.Checks {
		if check.Name == "runtime_trigger" && !check.Passed {
			sawTriggerFailure = true
		}
	}
	if !sawTriggerFailure {
		t.Fatalf("outbox preview did not explain withdrawn task trigger: %+v", after.Checks)
	}
	_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "scope.begin.denied"}, func(tx *Tx) (any, error) {
		_, beginErr := tx.BeginDelivery(ctx, ready.ID)
		return nil, beginErr
	})
	if ErrorCode(err) != "denied" {
		t.Fatalf("ready outbox began sending after trigger withdrawal: %v", err)
	}
	var state string
	if err = f.s.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE id=?", ready.ID).Scan(&state); err != nil || state != "ready" {
		t.Fatalf("failed authorization changed delivery state: %s %v", state, err)
	}
}

func TestRuntimeTriggerWithdrawalBlocksConfirmedExternalAction(t *testing.T) {
	f := newOwnerInteractiveFixture(t)
	ctx := context.Background()
	task := createRuntimeTask(t, f, "push-after-answer")
	var claimed RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "scope.action.task.claim", func(tx *Tx) (any, error) {
		var err error
		claimed, attempt, err = tx.ClaimRuntimeTask(ctx, f.config.ID, "scope-action-attempt", "model", "preset", "commit", "")
		return claimed, err
	})
	runtimeMutate(t, f.s, "scope.action.task.complete", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(ctx, claimed.ID, claimed.Version, attempt.ID, RuntimeAttemptResult{Result: "Commit prepared", Actions: []RuntimeAction{{Kind: "git_push", Target: "origin/main", Payload: "push reviewed commit"}}}, "")
	})
	task, err := ReadRuntimeTask(ctx, f.s.DB, task.ID)
	if err != nil || len(task.Actions) != 1 {
		t.Fatalf("external operation was not prepared for owner review: %+v %v", task, err)
	}
	confirmation := intakeRuntimeMessage(t, f.s, f.channel, f.direct.ConversationID, "scope-owner-confirmation", f.owner, ConfirmationToken(task.Actions[0]), time.Now().Add(time.Hour))
	runtimeMutate(t, f.s, "scope.action.confirm", func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeActionFromMessage(ctx, task.Actions[0].ID, confirmation.MessageID)
	})
	runtimeMutate(t, f.s, "scope.action.withdraw", func(tx *Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET route_ids='[]' WHERE id=?", f.config.ID)
		return nil, err
	})
	var action RuntimePendingAction
	runtimeMutate(t, f.s, "scope.action.claim", func(tx *Tx) (any, error) {
		var err error
		action, _, err = tx.ClaimRuntimeAction(ctx, f.config.ID, "scope-action-worker", "model")
		return action, err
	})
	if action.ID != "" {
		t.Fatalf("confirmed external operation was executed after its trigger group withdrew: %+v", action)
	}
	var status string
	if err := f.s.DB.QueryRowContext(ctx, "SELECT status FROM runtime_pending_actions WHERE id=?", task.Actions[0].ID).Scan(&status); err != nil || status != "confirmed" {
		t.Fatalf("blocked operation lost its audit state: status=%s error=%v", status, err)
	}
}

func TestBeginDeliveryKeepsOrdinaryReadyOutboxIndependentOfRuntimeScope(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	ordinaryID := NewID()
	runtimeMutate(t, f.s, "scope.ordinary.outbox", func(tx *Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, `INSERT INTO outbox(id,channel_id,route_id,route_version,conversation_id,audience_key,content,input_digest,send_policy,state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?, 'ready',?,?)`, ordinaryID, f.channel.ID, f.direct.ID, f.direct.Version, f.direct.ConversationID, f.direct.AudienceKey, "Ordinary prepared reply", "ordinary-digest", "dispatch_only", Now(), Now())
		return nil, err
	})
	runtimeMutate(t, f.s, "scope.ordinary.runtime.empty", func(tx *Tx) (any, error) {
		_, _, err := tx.SyncRuntimeGroupRoutes(ctx, f.config.ID, nil)
		return nil, err
	})
	var attempt int
	runtimeMutate(t, f.s, "scope.ordinary.begin", func(tx *Tx) (any, error) {
		var err error
		attempt, err = tx.BeginDelivery(ctx, ordinaryID)
		return attempt, err
	})
	if attempt != 1 {
		t.Fatalf("ordinary outbox was coupled to an unrelated runtime: attempt=%d", attempt)
	}
}

func TestBeginDeliveryAllowsCancelledInteractiveFailureCloseout(t *testing.T) {
	f := newOwnerInteractiveFixture(t)
	ctx := context.Background()
	runtimeMutate(t, f.s, "cancelled.closeout.capability", func(tx *Tx) (any, error) {
		_, err := tx.SetChannelCapabilities(ctx, f.channel.ID, Capabilities{Verified: map[string]bool{"send": true}}, "fake")
		return nil, err
	})
	task := createRuntimeTask(t, f, "cancelled-closeout")
	var claimed RuntimeTask
	runtimeMutate(t, f.s, "cancelled.closeout.claim", func(tx *Tx) (any, error) {
		var err error
		claimed, _, err = tx.ClaimRuntimeTask(ctx, f.config.ID, "cancelled-closeout-attempt", "model", "preset", "commit", "")
		return claimed, err
	})
	if claimed.ID != task.ID {
		t.Fatalf("task was not claimed: claimed=%+v want=%s", claimed, task.ID)
	}
	runtimeMutate(t, f.s, "cancelled.closeout.cancel", func(tx *Tx) (any, error) {
		_, err := tx.SetRuntimeTaskStatus(ctx, task.ID, "cancelled")
		return nil, err
	})
	var receipt OutboxView
	runtimeMutate(t, f.s, "cancelled.closeout.prepare-receipt", func(tx *Tx) (any, error) {
		var err error
		receipt, err = tx.PrepareTaskFailureAcknowledgement(ctx, task.ID)
		return receipt, err
	})
	var receiptAttempt int
	runtimeMutate(t, f.s, "cancelled.closeout.begin-receipt", func(tx *Tx) (any, error) {
		var err error
		receiptAttempt, err = tx.BeginDelivery(ctx, receipt.ID)
		return receiptAttempt, err
	})
	if receiptAttempt != 1 {
		t.Fatalf("cancelled failure reaction did not begin: attempt=%d", receiptAttempt)
	}
	runtimeMutate(t, f.s, "cancelled.closeout.finish-receipt", func(tx *Tx) (any, error) {
		return nil, tx.FinishDelivery(ctx, receipt.ID, receiptAttempt, "accepted", "reaction_added")
	})
	var notice OutboxView
	runtimeMutate(t, f.s, "cancelled.closeout.prepare-notice", func(tx *Tx) (any, error) {
		var err error
		notice, err = tx.PrepareTaskFailureNotice(ctx, task.ID)
		return notice, err
	})
	var noticeAttempt int
	runtimeMutate(t, f.s, "cancelled.closeout.begin-notice", func(tx *Tx) (any, error) {
		var err error
		noticeAttempt, err = tx.BeginDelivery(ctx, notice.ID)
		return noticeAttempt, err
	})
	if noticeAttempt != 1 {
		t.Fatalf("cancelled failure notice did not begin: attempt=%d", noticeAttempt)
	}
	if !strings.Contains(notice.Content, "错误代码：`cancelled`") {
		t.Fatalf("cancelled notice did not expose safe cancellation code: %s", notice.Content)
	}
}
