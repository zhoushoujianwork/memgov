package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type cardFixture struct {
	s     *Store
	app   Channel
	cfg   RuntimeConfig
	route Route
	task  RuntimeTask
	out   OutboxView
	cb    RuntimeCardCallback
}

func newCardFixture(t *testing.T) cardFixture {
	t.Helper()
	ctx := context.Background()
	f := newGroupMentionSyncFixture(t)
	runtimeMutate(t, f.s, "card.owner", func(tx *Tx) (any, error) { return tx.AttestDWSOwner(ctx, f.dwsChannel.ID, f.dwsChannel.ConfigVersion) })
	var owner string
	if err := f.s.DB.QueryRow(`SELECT principal_id FROM identity_aliases WHERE tenant=? AND id_type='user_id' AND id_value='user1'`, f.appChannel.Tenant).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	identity := f.appChannel.Identity
	identity.ConfirmationCardTemplate = "test-confirm.schema"
	if _, err := f.s.DB.Exec(`UPDATE channels SET identity=? WHERE id=?`, JSON(identity), f.appChannel.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec(`UPDATE runtime_configs SET owner_principal_id=?,owner_id_type='user_id',owner_id_value='user1' WHERE id=?`, owner, f.config.ID); err != nil {
		t.Fatal(err)
	}
	runtimeMutate(t, f.s, "card.intake", func(tx *Tx) (any, error) {
		return tx.Intake(ctx, f.appChannel.ID, NormalizedEvent{Kind: EventMessage, Adapter: "dingtalk_app", ParseVersion: "1", Origin: "stream", ProviderMessageID: "question", ConversationID: f.appA.ConversationID, ConversationType: "group", Tenant: f.appChannel.Tenant, Sender: Sender{IDType: "user_id", IDValue: "requester", DisplayName: "冒充所有者"}, Mentioned: true, Body: "请执行", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	})
	runtimeMutate(t, f.s, "card.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "card.batch", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now())
		return batch, err
	})
	runtimeMutate(t, f.s, "card.create", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "card-task", Title: "执行", Instructions: "准备", MessageIDs: []string{batch.Messages[0].ID}}}})
	})
	var task RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "card.claim", func(tx *Tx) (any, error) {
		var err error
		task, attempt, err = tx.ClaimRuntimeTask(ctx, f.config.ID, "", "model", "preset", "commit", "")
		return task, err
	})
	runtimeMutate(t, f.s, "card.complete", func(tx *Tx) (any, error) {
		var err error
		task, err = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "已准备", Actions: []RuntimeAction{{Kind: "git_push", Target: "origin/main", Payload: "push abc"}, {Kind: "infra_change", Target: "test", Payload: "change capacity"}}})
		return task, err
	})
	var out OutboxView
	runtimeMutate(t, f.s, "card.prepare", func(tx *Tx) (any, error) {
		var err error
		out, err = tx.PrepareTaskDelivery(ctx, task.ID)
		return out, err
	})
	runtimeMutate(t, f.s, "card.send", func(tx *Tx) (any, error) {
		attempt, err := tx.BeginDelivery(ctx, out.ID)
		if err != nil {
			return nil, err
		}
		return nil, tx.FinishDelivery(ctx, out.ID, attempt, "accepted", "receipt")
	})
	app, _ := ReadChannel(ctx, f.s.DB, f.appChannel.ID)
	cfg, _ := ReadRuntime(ctx, f.s.DB, f.config.ID)
	return cardFixture{s: f.s, app: app, cfg: cfg, route: f.appA, task: task, out: out, cb: RuntimeCardCallback{EventID: "click", CorpID: app.Tenant, CardID: out.ID, SpaceID: f.appA.ConversationID, SpaceType: "IM_GROUP", UserID: "user1", UserIDType: 1, Action: "confirm"}}
}
func (f cardFixture) confirm(cb RuntimeCardCallback) error {
	ctx := context.Background()
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "card.confirm"}, func(tx *Tx) (any, error) { return tx.ConfirmRuntimeCard(ctx, f.app.ID, cb) })
	return err
}
func TestCardApprovesOnlyDisplayedActionsAndCannotUseTextToken(t *testing.T) {
	f := newCardFixture(t)
	ctx := context.Background()
	var card RuntimeCard
	if err := json.Unmarshal([]byte(f.out.Content), &card); err != nil {
		t.Fatal(err)
	}
	if f.out.Format != "confirmation_card" || len(card.Mentions) != 2 || card.Mentions[0].IDValue != "requester" || card.Mentions[1].IDValue != "user1" || strings.Contains(card.Text, "确认口令") {
		t.Fatalf("card=%+v", card)
	}
	control := intakeRuntimeMessage(t, f.s, f.app, f.route.ConversationID, "typed", Sender{IDType: "user_id", IDValue: "user1"}, ConfirmationToken(f.task.Actions[0]), time.Now().Add(time.Hour))
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "card.token"}, func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeActionFromMessage(ctx, f.task.Actions[0].ID, control.MessageID)
	})
	if ErrorCode(err) != "denied" {
		t.Fatalf("token bypass: %v", err)
	}
	later := NewID()
	if _, err = f.s.DB.Exec(`INSERT INTO runtime_pending_actions(id,task_id,task_version,kind,target,payload,payload_digest,status,created_at,updated_at) VALUES(?,?,?,'send','target','later',?,'pending',?,?)`, later, f.task.ID, f.task.Version, Hash([]byte("later")), Now(), Now()); err != nil {
		t.Fatal(err)
	}
	if err = f.confirm(f.cb); err != nil {
		t.Fatal(err)
	}
	if err = f.confirm(f.cb); err != nil {
		t.Fatal("duplicate", err)
	}
	task, _ := ReadRuntimeTask(ctx, f.s.DB, f.task.ID)
	for _, a := range task.Actions {
		if a.ID == later {
			if a.Status != "pending" {
				t.Fatal("approved an undisplayed action")
			}
		} else if a.Status != "confirmed" || a.ConfirmedBy != f.cfg.OwnerPrincipalID || !strings.HasPrefix(a.ConfirmationOrigin, "dingtalk_card:"+f.out.ID+":") {
			t.Fatalf("action=%+v", a)
		}
	}
}
func TestCardOwnerCanRejectDisplayedActionsWithoutExecution(t *testing.T) {
	f := newCardFixture(t)
	ctx := context.Background()
	cb := f.cb
	cb.Action = "reject"
	var status string
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "card.reject"}, func(tx *Tx) (any, error) {
		var decideErr error
		status, decideErr = tx.ConfirmRuntimeCard(ctx, f.app.ID, cb)
		return status, decideErr
	})
	if err != nil || status != "reject" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	task, err := ReadRuntimeTask(ctx, f.s.DB, f.task.ID)
	if err != nil || task.Status != "cancelled" || task.ErrorCode != "owner_rejected" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	for _, a := range task.Actions {
		if a.Status != "rejected" || a.ConfirmedBy != f.cfg.OwnerPrincipalID || !strings.HasPrefix(a.ConfirmationOrigin, "dingtalk_card:"+f.out.ID+":") {
			t.Fatalf("action=%+v", a)
		}
	}
	_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "card.reject.duplicate"}, func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeCard(ctx, f.app.ID, cb)
	})
	if err != nil {
		t.Fatalf("duplicate rejection failed: %v", err)
	}
	cb.Action = "agree"
	_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "card.reject.switch"}, func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeCard(ctx, f.app.ID, cb)
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("rejected decision changed: %v", err)
	}
	var claimed RuntimePendingAction
	_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "card.reject.execute"}, func(tx *Tx) (any, error) {
		a, _, claimErr := tx.ClaimRuntimeAction(ctx, f.cfg.ID, "", "model")
		claimed = a
		return a, claimErr
	})
	if err != nil || claimed.ID != "" {
		t.Fatalf("rejected action became executable: action=%+v err=%v", claimed, err)
	}
}
func TestCardRejectsWrongActorScopeAndStaleState(t *testing.T) {
	for _, name := range []string{"member", "corp", "group", "user_type", "action", "unknown_delivery", "task_version", "payload", "target", "recall", "owner_revoked", "route_revoked", "template"} {
		t.Run(name, func(t *testing.T) {
			f := newCardFixture(t)
			cb := f.cb
			var err error
			switch name {
			case "member":
				cb.UserID = "requester"
			case "corp":
				cb.CorpID = "other"
			case "group":
				cb.SpaceID = "other"
			case "user_type":
				cb.UserIDType = 2
			case "action":
				cb.Action = "accept"
			case "unknown_delivery":
				_, err = f.s.DB.Exec(`UPDATE outbox SET state='unknown' WHERE id=?`, f.out.ID)
			case "task_version":
				_, err = f.s.DB.Exec(`UPDATE runtime_tasks SET version=version+1 WHERE id=?`, f.task.ID)
			case "payload":
				_, err = f.s.DB.Exec(`UPDATE runtime_pending_actions SET payload='changed',payload_digest=? WHERE id=?`, Hash([]byte("changed")), f.task.Actions[0].ID)
			case "target":
				_, err = f.s.DB.Exec(`UPDATE runtime_pending_actions SET target='other' WHERE id=?`, f.task.Actions[0].ID)
			case "recall":
				_, err = f.s.DB.Exec(`UPDATE messages SET availability='recalled' WHERE id=?`, f.task.Messages[0].ID)
			case "owner_revoked":
				_, err = f.s.DB.Exec(`UPDATE identity_aliases SET verified=0 WHERE principal_id=?`, f.cfg.OwnerPrincipalID)
			case "route_revoked":
				_, err = f.s.DB.Exec(`UPDATE channel_routes SET send_policy='draft_only' WHERE id=?`, f.route.ID)
			case "template":
				identity := f.app.Identity
				identity.ConfirmationCardTemplate = "other.schema"
				_, err = f.s.DB.Exec(`UPDATE channels SET identity=? WHERE id=?`, JSON(identity), f.app.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = f.confirm(cb); err == nil {
				t.Fatal("unsafe callback accepted")
			}
			task, _ := ReadRuntimeTask(context.Background(), f.s.DB, f.task.ID)
			for _, a := range task.Actions {
				if a.Status != "pending" {
					t.Fatalf("partial approval leaked: %+v", a)
				}
			}
		})
	}
}
func TestCardAcceptsDocumentedCallbackWithoutOptionalSpaceMetadata(t *testing.T) {
	f := newCardFixture(t)
	cb := f.cb
	cb.SpaceID, cb.SpaceType = "", ""
	if err := f.confirm(cb); err != nil {
		t.Fatal(err)
	}
	task, err := ReadRuntimeTask(context.Background(), f.s.DB, f.task.ID)
	if err != nil || len(task.Actions) == 0 {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	for _, action := range task.Actions {
		if action.Status != "confirmed" {
			t.Fatalf("action not confirmed: %+v", action)
		}
	}
}
func TestCardApprovalDoesNotSurvivePayloadOrIdentityChangeBeforeExecution(t *testing.T) {
	for _, kind := range []string{"payload", "identity", "recall"} {
		t.Run(kind, func(t *testing.T) {
			f := newCardFixture(t)
			if err := f.confirm(f.cb); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "payload":
				f.s.DB.Exec(`UPDATE runtime_pending_actions SET payload='changed',payload_digest=? WHERE task_id=?`, Hash([]byte("changed")), f.task.ID)
			case "identity":
				f.s.DB.Exec(`UPDATE identity_aliases SET verified=0 WHERE principal_id=?`, f.cfg.OwnerPrincipalID)
			case "recall":
				f.s.DB.Exec(`UPDATE messages SET availability='recalled' WHERE id=?`, f.task.Messages[0].ID)
			}
			ctx := context.Background()
			_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "card.execute"}, func(tx *Tx) (any, error) {
				a, _, err := tx.ClaimRuntimeAction(ctx, f.cfg.ID, "", "model")
				return a, err
			})
			if err == nil {
				t.Fatal("stale approval was executable")
			}
		})
	}
}

func TestRuntimeMentionAppearsOnceAtEnd(t *testing.T) {
	got := formatRuntimeMentionsAtEnd("answer\n\n@Requester\n\n---\nfooter", []RuntimeCardMention{{IDType: "user_id", IDValue: "user-1", Name: "Requester"}})
	if strings.Count(got, "@Requester") != 1 || !strings.HasSuffix(got, "footer\n\n@Requester") {
		t.Fatalf("mention was not deduplicated at the end: %q", got)
	}
}
