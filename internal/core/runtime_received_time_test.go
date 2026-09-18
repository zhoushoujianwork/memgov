package core

import (
	"context"
	"testing"
	"time"
)

func TestRuntimeSchedulesByReceivedTimeNotFutureSourceClock(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 20, 30)
	first := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "first-local", f.owner, "first received", time.Now().Add(8*time.Hour))
	second := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "second-local", f.owner, "second received", time.Now().Add(7*time.Hour))
	stamp := time.Now().Add(-40 * time.Second).UTC().Format(time.RFC3339Nano)
	if _, err := f.s.DB.Exec("UPDATE messages SET created_at=? WHERE id=?", stamp, first.MessageID); err != nil {
		t.Fatal(err)
	}
	runtimeMutate(t, f.s, "sync.received", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "claim.received", func(tx *Tx) (any, error) {
		var e error
		batch, e = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now())
		return batch, e
	})
	if len(batch.Messages) != 2 || batch.Messages[0].ID != first.MessageID || batch.Messages[1].ID != second.MessageID {
		t.Fatalf("source clock affected local readiness/order: %+v", batch)
	}
}
