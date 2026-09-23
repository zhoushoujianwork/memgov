package core

import (
	"context"
	"testing"
	"time"
)

func reopenRuntimeStore(t *testing.T, s *Store) *Store {
	t.Helper()
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Close() })
	return next
}

// This closes the real SQLite connection, rather than invoking recovery on the
// same store. Both consumers share archived DWS context but keep separate waits.
func TestDualModeRestartPreservesSourceAndConsumerCheckpoints(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 20, 300)
	var source DataSource
	runtimeMutate(t, f.s, "restart.source", func(tx *Tx) (any, error) {
		var e error
		source, e = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "archive", Channel: f.channel.ID, Workspace: "global"})
		return source, e
	})
	if _, err := f.s.DB.Exec("UPDATE data_sources SET reconcile_cursor=7,last_reconciled_at=? WHERE id=?", "2026-01-01T00:00:00Z", source.ID); err != nil {
		t.Fatal(err)
	}
	proactive := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "restart-proactive", Sender{IDType: "staff_id", IDValue: "peer"}, "new request", time.Now().Add(time.Hour))
	runtimeMutate(t, f.s, "restart.proactive.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var appChannel Channel
	var appRoute Route
	var group RuntimeConfig
	runtimeMutate(t, f.s, "restart.group.configure", func(tx *Tx) (any, error) {
		var err error
		appChannel, err = tx.AddChannel(ctx, ChannelInput{Name: "restart-group", Kind: ChannelDingTalkApp, Identity: ChannelIdentity{ExpectedCorpID: f.channel.Tenant, ClientID: "restart-client", RobotCode: "restart-bot", HistoryChannel: f.channel.ID}})
		if err != nil {
			return nil, err
		}
		appRoute, err = tx.AddRoute(ctx, appChannel.ID, RouteInput{ConversationID: f.watch.ConversationID, Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		if err != nil {
			return nil, err
		}
		group, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "restart-group", Channel: appChannel.ID, RouteIDs: []string{appRoute.ID}, DeliveryRouteID: appRoute.ID, Owner: f.owner, ApplicationMode: "group_mention", ContextChannel: f.channel.ID})
		if err != nil {
			return nil, err
		}
		return tx.SetRuntimeStatus(ctx, group.ID, "running", "")
	})
	var mentioned IntakeResult
	runtimeMutate(t, f.s, "restart.group.intake", func(tx *Tx) (any, error) {
		var err error
		mentioned, err = tx.Intake(ctx, appChannel.ID, NormalizedEvent{Kind: EventMessage, Adapter: "fake", Origin: "stream", ParseVersion: "1", ProviderMessageID: "restart-group-mention", ConversationID: appRoute.ConversationID, ConversationType: "group", Tenant: appChannel.Tenant, Sender: Sender{IDType: "staff_id", IDValue: "peer"}, Mentioned: true, Body: "@bot question", SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
		return mentioned, err
	})
	runtimeMutate(t, f.s, "restart.group.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, group.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "restart.group.claim", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, group.ID, time.Now())
		return batch, err
	})
	if batch.ID == "" {
		t.Fatal("group mention did not enter analysis")
	}
	before := map[string]string{}
	for _, id := range []string{proactive.MessageID, mentioned.MessageID} {
		var first string
		if err := f.s.DB.QueryRow("SELECT first_seen_at FROM runtime_message_states WHERE message_id=?", id).Scan(&first); err != nil {
			t.Fatal(err)
		}
		before[id] = first
	}
	f.s = reopenRuntimeStore(t, f.s)
	for _, id := range []string{f.config.ID, group.ID} {
		runtimeMutate(t, f.s, "restart.recover."+id, func(tx *Tx) (any, error) { return tx.RecoverRuntime(ctx, id) })
	}
	stored, err := ReadDataSource(ctx, f.s.DB, source.ID)
	if err != nil || stored.ReconcileCursor != 7 || stored.LastReconciledAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("source checkpoint changed: %+v %v", stored, err)
	}
	for id, want := range before {
		var first, state string
		if err = f.s.DB.QueryRow("SELECT first_seen_at,state FROM runtime_message_states WHERE message_id=?", id).Scan(&first, &state); err != nil || first != want || state != "pending" {
			t.Fatalf("consumer checkpoint changed: first=%q state=%q error=%v", first, state, err)
		}
	}
	runtimeMutate(t, f.s, "restart.proactive.wait", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now().Add(301*time.Second))
		return batch, err
	})
	if len(batch.Messages) != 1 || batch.Messages[0].ID != proactive.MessageID {
		t.Fatal("persisted oldest-wait did not trigger after restart")
	}
	runtimeMutate(t, f.s, "restart.group.reclaim", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, group.ID, time.Now())
		return batch, err
	})
	if len(batch.Messages) != 1 || batch.Messages[0].ID != mentioned.MessageID {
		t.Fatal("interrupted group analysis was not recoverable")
	}
}

func TestRuntimeRestartUnknownDeliveryNeverReexecutes(t *testing.T) {
	ctx := context.Background()
	f := newOwnerInteractiveFixture(t)
	task := createRuntimeTask(t, f, "completed-before-crash")
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "restart.execute", func(tx *Tx) (any, error) {
		var err error
		task, attempt, err = tx.ClaimRuntimeTask(ctx, f.config.ID, "crash-execution", "fake-model", "fake-preset", "commit", "")
		return task, err
	})
	runtimeMutate(t, f.s, "restart.complete", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "validated artifact", Summary: "complete"})
	})
	var outbox OutboxView
	runtimeMutate(t, f.s, "restart.delivery", func(tx *Tx) (any, error) {
		var err error
		outbox, err = tx.PrepareTaskDelivery(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		return tx.BeginDelivery(ctx, outbox.ID)
	})
	f.s = reopenRuntimeStore(t, f.s)
	for i := 0; i < 2; i++ {
		runtimeMutate(t, f.s, "restart.recover-delivery", func(tx *Tx) (any, error) { return tx.RecoverRuntime(ctx, f.config.ID) })
	}
	var deliveryState, attemptState string
	if err := f.s.DB.QueryRow("SELECT state FROM outbox WHERE id=?", outbox.ID).Scan(&deliveryState); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow("SELECT state FROM delivery_attempts WHERE outbox_id=?", outbox.ID).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if deliveryState != "unknown" || attemptState != "unknown" {
		t.Fatalf("in-flight delivery not unknown: %s %s", deliveryState, attemptState)
	}
	stored, err := ReadRuntimeTask(ctx, f.s.DB, task.ID)
	if err != nil || stored.Status != "completed" || len(stored.Attempts) != 1 {
		t.Fatalf("completed work changed: %+v %v", stored, err)
	}
	runtimeMutate(t, f.s, "restart.no-reexecute", func(tx *Tx) (any, error) {
		claimed, _, err := tx.ClaimRuntimeTask(ctx, f.config.ID, "must-not-execute", "fake", "fake", "commit", "")
		if claimed.ID != "" {
			t.Fatal("completed task was executed again")
		}
		return claimed, err
	})
	runtimeMutate(t, f.s, "restart.same-outbox", func(tx *Tx) (any, error) {
		same, err := tx.PrepareTaskDelivery(ctx, task.ID)
		if same.ID != outbox.ID || same.State != "unknown" {
			t.Fatal("unknown result generated a new delivery")
		}
		return same, err
	})
	_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "restart.no-resend"}, func(tx *Tx) (any, error) { return tx.BeginDelivery(ctx, outbox.ID) })
	if ErrorCode(err) != "conflict" {
		t.Fatalf("unknown delivery allowed resend: %v", err)
	}
}
