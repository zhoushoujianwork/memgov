package core

import (
	"context"
	"testing"
	"time"
)

func TestRuntimeWorkProbesAvoidDormantWritesAndWakeAtThreshold(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 2, 300)
	message := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "probe-one", Sender{IDType: "union_id", IDValue: "alice"}, "one pending item", time.Now().Add(time.Hour))

	ready, err := RuntimeMessagesNeedSync(ctx, f.s.DB, f.config.ID)
	if err != nil || !ready {
		t.Fatalf("new message sync probe: ready=%v err=%v", ready, err)
	}
	runtimeMutate(t, f.s, "probe.sync", func(tx *Tx) (any, error) {
		return tx.SyncRuntimeMessages(ctx, f.config.ID)
	})
	ready, err = RuntimeMessagesNeedSync(ctx, f.s.DB, f.config.ID)
	if err != nil || ready {
		t.Fatalf("stable message sync probe: ready=%v err=%v", ready, err)
	}
	ready, err = RuntimeBatchReady(ctx, f.s.DB, f.config.ID, time.Now())
	if err != nil || ready {
		t.Fatalf("fresh below-threshold batch probe: ready=%v err=%v", ready, err)
	}
	ready, err = RuntimeBatchReady(ctx, f.s.DB, f.config.ID, time.Now().Add(6*time.Minute))
	if err != nil || !ready {
		t.Fatalf("elapsed batch probe: ready=%v err=%v", ready, err)
	}

	var state string
	if err = f.s.DB.QueryRowContext(ctx, "SELECT state FROM runtime_message_states WHERE runtime_id=? AND message_id=?", f.config.ID, message.MessageID).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("probe changed durable state: state=%q err=%v", state, err)
	}
}

func TestRuntimeTaskProbeStopsWhileAnAttemptIsActive(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 1, 300)
	task := createRuntimeTask(t, f, "probe-task")
	ready, err := RuntimeTaskReady(ctx, f.s.DB, f.config.ID)
	if err != nil || !ready {
		t.Fatalf("pending task probe: ready=%v err=%v", ready, err)
	}
	var claimed RuntimeTask
	runtimeMutate(t, f.s, "probe.task.claim", func(tx *Tx) (any, error) {
		var claimErr error
		claimed, _, claimErr = tx.ClaimRuntimeTask(ctx, f.config.ID, "probe-attempt", "model", "preset", "commit", "")
		return claimed, claimErr
	})
	if claimed.ID != task.ID {
		t.Fatalf("claimed task=%q want=%q", claimed.ID, task.ID)
	}
	ready, err = RuntimeTaskReady(ctx, f.s.DB, f.config.ID)
	if err != nil || ready {
		t.Fatalf("active attempt probe: ready=%v err=%v", ready, err)
	}
}
