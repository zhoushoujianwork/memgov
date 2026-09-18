package core

import (
	"context"
	"testing"
	"time"
)

func TestDestructiveProposalReleasesTaskAndRequiresExactDetails(t *testing.T) {
	for _, valid := range []bool{false, true} {
		f := newRuntimeFixture(t, 1, 30)
		task := createRuntimeTask(t, f, "destructive")
		ctx := context.Background()
		var attempt RuntimeAttempt
		runtimeMutate(t, f.s, "test.claim", func(tx *Tx) (any, error) {
			var e error
			_, attempt, e = tx.ClaimRuntimeTask(ctx, f.config.ID, NewID(), "m", "p", "c", "")
			return nil, e
		})
		payload := `{"operation":"delete named test artifact","impact":"one artifact removed","recovery":"restore named backup"}`
		if !valid {
			payload = `{"operation":"delete everything"}`
		}
		_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "test.propose"}, func(tx *Tx) (any, error) {
			var e error
			task, e = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "awaiting concrete approval", Actions: []RuntimeAction{{Kind: RuntimeDestructiveAction, Target: "/named/test/artifact", Payload: payload}}}, "")
			return task, e
		})
		if !valid {
			if ErrorCode(err) != "invalid_input" {
				t.Fatalf("invalid proposal accepted: %v", err)
			}
			continue
		}
		if err != nil || task.Status != "awaiting_confirmation" {
			t.Fatalf("proposal lost: %+v %v", task, err)
		}
		var active int
		if err = f.s.DB.QueryRow("SELECT count(*) FROM runtime_work_leases WHERE released=0 AND task_id=?", task.ID).Scan(&active); err != nil || active != 0 {
			t.Fatalf("proposal kept slot: %d %v", active, err)
		}
	}
}

func TestDestructiveApprovalRejectsHistoricalAndQuotedTokens(t *testing.T) {
	for _, scenario := range []string{"live", "history", "quote", "context_only", "version_changed", "revoked_owner"} {
		t.Run(scenario, func(t *testing.T) {
			f, app, route, a := crossConfirmationFixture(t)
			ctx := context.Background()
			payload := `{"operation":"remove exact artifact","impact":"artifact removed","recovery":"restore backup"}`
			a.Kind, a.Payload, a.PayloadDigest = RuntimeDestructiveAction, payload, Hash([]byte(payload))
			if _, e := f.s.DB.Exec("UPDATE runtime_pending_actions SET kind=?,payload=?,payload_digest=? WHERE id=?", a.Kind, a.Payload, a.PayloadDigest, a.ID); e != nil {
				t.Fatal(e)
			}
			if _, e := f.s.DB.Exec("UPDATE runtime_tasks SET status='awaiting_confirmation' WHERE id=?", a.TaskID); e != nil {
				t.Fatal(e)
			}
			body, origin := ConfirmationToken(a), "stream"
			if scenario == "history" {
				origin = "history"
			}
			if scenario == "quote" {
				body = "> " + body
			}
			message := intake(t, f.s, app.ID, NormalizedEvent{Kind: EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/2", Origin: origin, ProviderMessageID: "approval", ConversationID: route.ConversationID, ConversationType: "direct", Tenant: app.Tenant, Sender: f.owner, Body: body, SentAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)}, "approval")
			switch scenario {
			case "context_only":
				if _, e := f.s.DB.Exec("UPDATE messages SET context_only=1 WHERE id=?", message.MessageID); e != nil {
					t.Fatal(e)
				}
			case "version_changed":
				if _, e := f.s.DB.Exec("UPDATE runtime_tasks SET version=version+1 WHERE id=?", a.TaskID); e != nil {
					t.Fatal(e)
				}
			case "revoked_owner":
				if _, e := f.s.DB.Exec("UPDATE identity_aliases SET verified=0 WHERE principal_id=?", f.config.OwnerPrincipalID); e != nil {
					t.Fatal(e)
				}
			}
			_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "test.approve"}, func(tx *Tx) (any, error) { return tx.ConfirmRuntimeActionFromMessage(ctx, a.ID, message.MessageID) })
			if scenario != "live" {
				if err == nil {
					t.Fatal("untrusted approval accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			runtimeMutate(t, f.s, "test.claim.action", func(tx *Tx) (any, error) {
				action, attempt, e := tx.ClaimRuntimeAction(ctx, f.config.ID, NewID(), "model")
				if e == nil && (action.ID == "" || attempt.ID == "") {
					t.Fatal("confirmed action not claimed")
				}
				return nil, e
			})
		})
	}
}
