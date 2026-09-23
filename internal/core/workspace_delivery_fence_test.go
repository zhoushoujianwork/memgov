package core

import (
	"context"
	"testing"
	"time"
)

func TestWorkspaceUpgradeExcludesLegacyKnowledgeFromAutomaticContext(t *testing.T) {
	ctx := context.Background()
	f := newOwnerInteractiveFixture(t)
	legacy := createRuntimeTask(t, f, "legacy-knowledge")
	ordinary := createRuntimeTask(t, f, "ordinary-work")
	for _, task := range []RuntimeTask{legacy, ordinary} {
		runtimeMutate(t, f.s, "fixture.context.turn", func(tx *Tx) (any, error) {
			full, err := ReadRuntimeTask(ctx, tx.Conn, task.ID)
			if err != nil {
				return nil, err
			}
			return tx.BindRuntimeDirectTurn(ctx, f.config, full)
		})
		if _, err := f.s.DB.Exec("UPDATE runtime_tasks SET status='completed',result=?,result_summary=? WHERE id=?", "result "+task.CanonicalKey, "summary "+task.CanonicalKey, task.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.DB.Exec("INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,content,input_digest,state,reason,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'delivered result',?,'accepted','result',?,?)", NewID(), f.channel.ID, f.watch.ID, f.watch.Version, task.ID, f.watch.ConversationID, f.watch.AudienceKey, task.ID, Now(), Now()); err != nil {
			t.Fatal(err)
		}
	}
	dropSchema27(t, f.s)
	if _, err := f.s.DB.Exec("UPDATE runtime_tasks SET kind='memory' WHERE id=?", legacy.ID); err != nil {
		t.Fatal(err)
	}
	path := f.s.Path
	f.s.Close()
	s, err := Open(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	retained, err := ReadRuntimeTask(ctx, s.DB, legacy.ID)
	if err != nil || retained.Kind != "memory" || retained.Result != "result "+legacy.CanonicalKey || retained.ResultSummary != "summary "+legacy.CanonicalKey {
		t.Fatal("legacy task is no longer operator-readable", retained, err)
	}
	message := intakeRuntimeMessage(t, s, f.channel, f.watch.ConversationID, "new-after-upgrade", f.owner, "continue ordinary work", time.Now().Add(2*time.Hour))
	runtimeMutate(t, s, "fixture.context.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, s, "fixture.context.claim", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now().Add(3*time.Hour))
		return batch, err
	})
	if len(batch.Matters) != 1 || batch.Matters[0].CanonicalKey != ordinary.CanonicalKey || batch.Matters[0].Conclusion != "summary "+ordinary.CanonicalKey {
		t.Fatal("analysis reloaded archived knowledge or lost ordinary work", batch.Matters)
	}
	var current RuntimeTask
	var session RuntimeDirectSession
	runtimeMutate(t, s, "fixture.context.complete", func(tx *Tx) (any, error) {
		tasks, err := tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "new-task", Title: "new work", Instructions: "continue ordinary work", MessageIDs: []string{message.MessageID}}}})
		if err != nil {
			return nil, err
		}
		current, err = ReadRuntimeTask(ctx, tx.Conn, tasks[0].ID)
		if err != nil {
			return nil, err
		}
		session, err = tx.BindRuntimeDirectTurn(ctx, f.config, current)
		return session, err
	})
	history, err := RuntimeDirectHistory(ctx, s.DB, f.config, current, session.ID)
	if err != nil || len(history) != 2 || history[1].Body != "result "+ordinary.CanonicalKey || !history[1].SelfAuthored {
		t.Fatal("direct history reloaded archived knowledge or lost ordinary conversation", history, err)
	}
}

func TestWorkspaceUpgradeNeverResurrectsLegacyMemoryDeliveries(t *testing.T) {
	for _, priorStatus := range []string{"running", "completed"} {
		t.Run(priorStatus, func(t *testing.T) {
			ctx := context.Background()
			f := newOwnerInteractiveFixture(t)
			task := createRuntimeTask(t, f, "legacy-delivery")
			dropSchema27(t, f.s)
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := f.s.DB.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec("UPDATE runtime_tasks SET kind='memory',status=?,result='historical memory result',result_summary='historical memory summary' WHERE id=?", priorStatus, task.ID)
			// The accepted receipt is historical delivery evidence, not permission
			// to generate a fresh cancellation notice after the upgrade.
			for id, state := range map[string]string{"accepted-receipt": "accepted", "unknown-send": "unknown"} {
				exec("INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,content,input_digest,state,reason,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'old content',?,?,?,?,?)", id, f.channel.ID, f.watch.ID, f.watch.Version, task.ID, f.watch.ConversationID, f.watch.AudienceKey, id, state, RuntimeReceiptPurpose, Now(), Now())
			}
			path := f.s.Path
			f.s.Close()
			s, err := Open(ctx, path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			current, err := ReadRuntimeTask(ctx, s.DB, task.ID)
			wantStatus := "cancelled"
			if priorStatus == "completed" {
				wantStatus = "completed"
			}
			if err != nil || current.Status != wantStatus || current.Result != "historical memory result" {
				t.Fatalf("historical task changed: %+v %v", current, err)
			}
			for _, purpose := range []string{RuntimeProcessingReceiptPurpose, RuntimeCompletionReceiptPurpose, RuntimeFailureReceiptPurpose} {
				ids, err := RuntimeStageAcknowledgementTaskIDs(ctx, s.DB, f.config.ID, purpose)
				if err != nil || len(ids) != 0 {
					t.Fatalf("retired task reappeared in %s scan: %v %v", purpose, ids, err)
				}
			}
			ids, err := RuntimeFailureNoticeTaskIDs(ctx, s.DB, f.config.ID)
			if err != nil || len(ids) != 0 {
				t.Fatal("retired task reappeared in failure notice scan", ids, err)
			}
			for _, purpose := range []string{"result", RuntimeReceiptPurpose, RuntimeProcessingReceiptPurpose, RuntimeCompletionReceiptPurpose, RuntimeFailureReceiptPurpose, RuntimeFailureNoticePurpose} {
				_, err := s.Mutate(ctx, Request{}, func(tx *Tx) (any, error) { return tx.prepareTaskDelivery(ctx, task.ID, purpose, false) })
				if ErrorCode(err) != "denied" {
					t.Fatalf("legacy %s preparation: %v", purpose, err)
				}
			}
			if _, err = s.Mutate(ctx, Request{}, func(tx *Tx) (any, error) { return tx.PrepareTaskFailureNotice(ctx, task.ID) }); ErrorCode(err) != "denied" {
				t.Fatal("legacy failure notice preparation", err)
			}
			var count int
			if err = s.DB.QueryRow("SELECT count(*) FROM outbox WHERE job_id=?", task.ID).Scan(&count); err != nil || count != 2 {
				t.Fatal("delivery preparation resurrected outbox", count, err)
			}
			// A queued delivery worker must reject the task independently of the
			// preparation fence, even if handed a ready record from an old caller.
			if _, err = s.DB.Exec("INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,content,input_digest,state,reason,created_at,updated_at) VALUES('queued-old',?,?,?,?,?,?,'historical memory result',?,'ready','result',?,?)", f.channel.ID, f.watch.ID, f.watch.Version, task.ID, f.watch.ConversationID, f.watch.AudienceKey, Digest(map[string]any{"task": task.ID, "version": current.Version, "result": "historical memory result"}), Now(), Now()); err != nil {
				t.Fatal(err)
			}
			preview, err := PreviewDraft(ctx, s.DB, "queued-old")
			if err != nil {
				t.Fatal(err)
			}
			blocked := false
			for _, check := range preview.Checks {
				if check.Name == "runtime_trigger" {
					blocked = !check.Passed && check.Detail == "legacy memory task is archived and cannot be delivered"
				}
			}
			if !blocked || preview.Sendable {
				t.Fatal("generic outbox dispatch accepted a legacy memory task", preview)
			}
			for id, want := range map[string]string{"accepted-receipt": "accepted", "unknown-send": "unknown", "queued-old": "ready"} {
				if _, err = s.Mutate(ctx, Request{}, func(tx *Tx) (any, error) { return tx.BeginDelivery(ctx, id) }); ErrorCode(err) != "denied" {
					t.Fatalf("queued legacy delivery %s: %v", id, err)
				}
				var state string
				if err = s.DB.QueryRow("SELECT state FROM outbox WHERE id=?", id).Scan(&state); err != nil || state != want {
					t.Fatal("historical delivery state changed", id, state, err)
				}
			}
		})
	}
}
