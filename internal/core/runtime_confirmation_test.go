package core

import (
	"context"
	"testing"
	"time"
)

func crossConfirmationFixture(t *testing.T) (runtimeFixture, Channel, Route, RuntimePendingAction) {
	t.Helper()
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	f.owner = Sender{IDType: "user_id", IDValue: f.channel.Identity.ExpectedUserID}
	var app Channel
	var direct Route
	runtimeMutate(t, f.s, "test.cross.configure", func(tx *Tx) (any, error) {
		if _, err := tx.AttestDWSOwner(ctx, f.channel.ID, f.channel.ConfigVersion); err != nil {
			return nil, err
		}
		owner, _, err := tx.PrincipalFor(ctx, f.channel.Tenant, f.owner)
		if err != nil {
			return nil, err
		}
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET owner_id_type=?,owner_id_value=?,owner_principal_id=? WHERE id=?", f.owner.IDType, f.owner.IDValue, owner, f.config.ID); err != nil {
			return nil, err
		}
		app, err = tx.AddChannel(ctx, ChannelInput{Name: "confirmation-app", Kind: ChannelDingTalkApp, Identity: ChannelIdentity{ExpectedCorpID: f.channel.Tenant, ClientID: "client", RobotCode: "robot", HistoryChannel: f.channel.ID}})
		if err != nil {
			return nil, err
		}
		direct, err = tx.AddRoute(ctx, app.ID, RouteInput{ConversationID: "app-owner-inbox", ConversationType: "direct", Mode: "notify"})
		if err != nil {
			return nil, err
		}
		_, err = tx.UpdateRoute(ctx, direct.ID, direct.Version, RouteInput{SendPolicy: "dispatch_only"}, "owner confirmation inbox")
		return nil, err
	})
	f.config, _ = ReadRuntime(ctx, f.s.DB, f.config.ID)
	direct, _ = ReadRoute(ctx, f.s.DB, direct.ID)
	task := createRuntimeTask(t, f, "cross-channel-action")
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "test.cross.claim", func(tx *Tx) (any, error) {
		var err error
		_, attempt, err = tx.ClaimRuntimeTask(ctx, f.config.ID, NewID(), "model", "preset", "commit", "/tmp/work")
		return nil, err
	})
	runtimeMutate(t, f.s, "test.cross.complete", func(tx *Tx) (any, error) {
		var err error
		task, err = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "准备完成", Actions: []RuntimeAction{{Kind: "git_push", Target: "origin/main", Payload: "push commit abc"}}})
		return nil, err
	})
	return f, app, direct, task.Actions[0]
}

func TestRuntimeBackgroundDoesNotScanOwnerConfirmationInbox(t *testing.T) {
	f, app, route, action := crossConfirmationFixture(t)
	message := intakeRuntimeMessage(t, f.s, app, route.ConversationID, "owner-approval", f.owner, ConfirmationToken(action), time.Now().Add(time.Hour))
	var confirmed []RuntimePendingAction
	runtimeMutate(t, f.s, "test.cross.scan", func(tx *Tx) (any, error) {
		var err error
		confirmed, err = tx.ProcessRuntimeConfirmations(context.Background(), f.config.ID)
		return confirmed, err
	})
	_ = message
	if len(confirmed) != 0 {
		t.Fatalf("background task scanned legacy inbox: %+v", confirmed)
	}
	runtimeMutate(t, f.s, "test.cross.scan.again", func(tx *Tx) (any, error) {
		var err error
		confirmed, err = tx.ProcessRuntimeConfirmations(context.Background(), f.config.ID)
		return confirmed, err
	})
	if len(confirmed) != 0 {
		t.Fatal("approval replayed")
	}
}

func TestRuntimeCrossChannelConfirmationRejectsUntrustedInputs(t *testing.T) {
	for _, scenario := range []string{"other_sender", "same_staff_value", "wrong_conversation", "group", "missing_binding", "wrong_history", "foreign_tenant", "ambiguous_app", "ambiguous_route", "ignored_route", "revoked_alias", "unattested_owner", "staff_owner", "wrong_robot", "old_message", "wrong_token", "recalled", "stale_task", "changed_payload"} {
		t.Run(scenario, func(t *testing.T) {
			f, app, route, action := crossConfirmationFixture(t)
			ctx := context.Background()
			sender, body, conversation, sent := f.owner, ConfirmationToken(action), route.ConversationID, time.Now().Add(time.Hour)
			switch scenario {
			case "other_sender":
				sender = Sender{IDType: "user_id", IDValue: "mallory"}
			case "same_staff_value":
				sender.IDType = "staff_id"
			case "wrong_conversation", "group":
				conversation = "other-conversation"
				runtimeMutate(t, f.s, "test.other.route", func(tx *Tx) (any, error) {
					kind := "direct"
					if scenario == "group" {
						kind = "group"
					}
					return tx.AddRoute(ctx, app.ID, RouteInput{ConversationID: conversation, ConversationType: kind})
				})
			case "old_message":
				sent = time.Now().Add(-time.Hour)
			case "wrong_token":
				body += " 好的"
			}
			msg := intakeRuntimeMessage(t, f.s, app, conversation, "confirmation", sender, body, sent)
			runtimeMutate(t, f.s, "test.cross.reject.setup", func(tx *Tx) (any, error) {
				var err error
				switch scenario {
				case "missing_binding":
					app.Identity.HistoryChannel = ""
					_, err = tx.Conn.ExecContext(ctx, "UPDATE channels SET identity=? WHERE id=?", JSON(app.Identity), app.ID)
				case "wrong_history":
					var other Channel
					other, err = tx.AddChannel(ctx, ChannelInput{Name: "other-history", Kind: ChannelDwsPersonal, Identity: ChannelIdentity{ExpectedCorpID: f.channel.Tenant, ExpectedUserID: "other-owner"}})
					if err == nil {
						app.Identity.HistoryChannel = other.ID
						_, err = tx.Conn.ExecContext(ctx, "UPDATE channels SET identity=? WHERE id=?", JSON(app.Identity), app.ID)
					}
				case "wrong_robot":
					f.channel.Identity.DeliveryRobotCode = "different-robot"
					_, err = tx.Conn.ExecContext(ctx, "UPDATE channels SET identity=? WHERE id=?", JSON(f.channel.Identity), f.channel.ID)
				case "foreign_tenant":
					_, err = tx.Conn.ExecContext(ctx, "UPDATE channels SET tenant='foreign' WHERE id=?", app.ID)
				case "ambiguous_app":
					_, err = tx.AddChannel(ctx, ChannelInput{Name: "second-app", Kind: ChannelDingTalkApp, Identity: ChannelIdentity{ExpectedCorpID: f.channel.Tenant, ClientID: "client2", RobotCode: "robot2", HistoryChannel: f.channel.ID}})
				case "ambiguous_route":
					var other Route
					other, err = tx.AddRoute(ctx, app.ID, RouteInput{ConversationID: "another-inbox", ConversationType: "direct"})
					if err == nil {
						_, err = tx.UpdateRoute(ctx, other.ID, other.Version, RouteInput{SendPolicy: "dispatch_only"}, "second inbox")
					}
				case "ignored_route":
					_, err = tx.Conn.ExecContext(ctx, "UPDATE channel_routes SET mode='ignore' WHERE id=?", route.ID)
				case "revoked_alias":
					_, err = tx.Conn.ExecContext(ctx, "UPDATE identity_aliases SET verified=0 WHERE principal_id=?", f.config.OwnerPrincipalID)
				case "unattested_owner":
					_, err = tx.Conn.ExecContext(ctx, "UPDATE identity_aliases SET basis='platform_event' WHERE principal_id=?", f.config.OwnerPrincipalID)
				case "staff_owner":
					_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET owner_id_type='staff_id' WHERE id=?", f.config.ID)
				case "recalled":
					_, err = tx.Conn.ExecContext(ctx, "UPDATE messages SET availability='recalled' WHERE id=?", msg.MessageID)
				case "stale_task":
					_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET version=version+1 WHERE id=?", action.TaskID)
				case "changed_payload":
					_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET payload='different operation' WHERE id=?", action.ID)
				}
				return nil, err
			})
			_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "test.cross.denied"}, func(tx *Tx) (any, error) {
				return tx.ConfirmRuntimeActionFromMessage(ctx, action.ID, msg.MessageID)
			})
			if ErrorCode(err) != "denied" && ErrorCode(err) != "conflict" {
				t.Fatalf("unsafe approval %s: %v", scenario, err)
			}
			current, err := ReadRuntimeTask(ctx, f.s.DB, action.TaskID)
			if err != nil || current.Actions[0].Status != "pending" {
				t.Fatalf("rejected approval changed action: %+v %v", current, err)
			}
		})
	}
}

func TestRuntimeBackgroundConfirmationRemainsDisabledWithVerifiedAlias(t *testing.T) {
	f, app, inbox, action := crossConfirmationFixture(t)
	ctx := context.Background()
	staff := Sender{IDType: "staff_id", IDValue: "different-staff-identifier"}
	var direct RuntimeConfig
	runtimeMutate(t, f.s, "test.confirmation.alias", func(tx *Tx) (any, error) {
		app.Identity.HistoryChannel = f.channel.Name
		if _, err := tx.Conn.ExecContext(ctx, "UPDATE channels SET identity=? WHERE id=?", JSON(app.Identity), app.ID); err != nil {
			return nil, err
		}
		if _, err := tx.LinkIdentity(ctx, f.channel.Tenant, staff, f.owner, "platform_directory"); err != nil {
			return nil, err
		}
		inbox.ConversationID = f.owner.IDValue
		if _, err := tx.Conn.ExecContext(ctx, "UPDATE channel_routes SET conversation_id=? WHERE id=?", inbox.ConversationID, inbox.ID); err != nil {
			return nil, err
		}
		var err error
		direct, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "direct-bot", Channel: app.ID, RouteIDs: []string{inbox.ID}, DeliveryRouteID: inbox.ID, Owner: f.owner, ApplicationMode: "direct"})
		return direct, err
	})
	msg := intakeRuntimeMessage(t, f.s, app, inbox.ConversationID, "linked-owner", staff, ConfirmationToken(action), time.Now().Add(time.Hour))
	// The direct consumer wins scheduling, but must not turn a control token
	// into an AI request. Background tasks no longer consume these approvals.
	var synced RuntimeSyncResult
	runtimeMutate(t, f.s, "test.confirmation.direct.sync", func(tx *Tx) (any, error) {
		var err error
		synced, err = tx.SyncRuntimeMessages(ctx, direct.ID)
		return synced, err
	})
	if synced.Pending != 0 || synced.Context != 1 {
		t.Fatalf("direct runtime interpreted approval as request: %+v", synced)
	}
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "test.confirmation.alias.confirm"}, func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeActionFromMessage(ctx, action.ID, msg.MessageID)
	})
	if ErrorCode(err) != "denied" {
		t.Fatalf("background confirmation reenabled by alias: %v", err)
	}
}
