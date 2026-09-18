package core

import (
	"context"
	"testing"
	"time"
)

func TestRuntimeCompletionLinksMatterAndDoesNotReopen(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	task := createRuntimeTask(t, f, "replace-resource")
	ctx := context.Background()
	for i, kind := range []string{"complete", "complete", "task"} {
		msg := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, NewID(), f.owner, "资源已更换并验证", time.Now().Add(time.Hour))
		runtimeMutate(t, f.s, "completion.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
		var batch RuntimeBatch
		runtimeMutate(t, f.s, "completion.claim", func(tx *Tx) (any, error) {
			var err error
			batch, err = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now().Add(2*time.Hour))
			return batch, err
		})
		if len(batch.Matters) != 1 || batch.Matters[0].CanonicalKey != task.CanonicalKey {
			t.Fatalf("missing existing matter: %+v", batch.Matters)
		}
		var results []RuntimeTask
		runtimeMutate(t, f.s, "completion.apply", func(tx *Tx) (any, error) {
			var err error
			results, err = tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: kind, CanonicalKey: task.CanonicalKey, Instructions: "资源更换完成，验证通过", MessageIDs: []string{msg.MessageID}}}})
			return results, err
		})
		if (i == 0 && len(results) != 1) || (i > 0 && len(results) != 0) {
			t.Fatalf("duplicate completion report: %d", len(results))
		}
		got, err := ReadRuntimeTask(ctx, f.s.DB, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "completed" || got.Version != task.Version+1 || got.ResultSummary != "资源更换完成，验证通过" {
			t.Fatalf("completion reopened or lost conclusion: %+v", got)
		}
		if len(got.Messages) != i+2 {
			t.Fatal("completion evidence was not linked")
		}
	}
}
