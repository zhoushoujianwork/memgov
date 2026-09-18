package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

func ownerMessageFixture(t *testing.T) (runtimeFixture, RuntimeTask, RuntimeMessageInput, RuntimeMessageTargetVerification) {
	t.Helper()
	ctx := context.Background()
	f := newRuntimeFixture(t, 1, 300)
	f.channel.Identity.Profile = "corp1:user1"
	if _, err := f.s.DB.Exec("UPDATE channels SET identity=? WHERE id=?", JSON(f.channel.Identity), f.channel.ID); err != nil {
		t.Fatal(err)
	}
	runtimeMutate(t, f.s, "test.owner.attest", func(tx *Tx) (any, error) { return tx.AttestDWSOwner(ctx, f.channel.ID, f.channel.ConfigVersion) })
	if _, err := f.s.DB.Exec("UPDATE runtime_configs SET owner_principal_id=(SELECT principal_id FROM identity_aliases WHERE tenant='corp1' AND id_type='user_id' AND id_value='user1'),owner_id_type='user_id',owner_id_value='user1',external_actions='owner_delegated' WHERE id=?", f.config.ID); err != nil {
		t.Fatal(err)
	}
	createRuntimeTask(t, f, "owner-message")
	var task RuntimeTask
	runtimeMutate(t, f.s, "test.owner.claim", func(tx *Tx) (any, error) {
		var e error
		task, _, e = tx.ClaimRuntimeTask(ctx, f.config.ID, "owner-attempt", "model", "preset", "commit", "")
		return task, e
	})
	task, err := ReadRuntimeTask(ctx, f.s.DB, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := RuntimeMessageInput{AttemptID: "owner-attempt", IdempotencyKey: "outreach-1", TargetType: "group", TargetID: f.watch.ConversationID, Content: "I can investigate this issue.", Reason: "The observed issue needs coordination", EvidenceMessageIDs: []string{task.Messages[0].ID}}
	v := RuntimeMessageTargetVerification{ChannelID: f.channel.ID, ChannelVersion: f.channel.ConfigVersion, OwnerProfile: f.channel.Identity.Profile, OwnerUserID: "user1", TargetType: in.TargetType, TargetID: in.TargetID}
	return f, task, in, v
}

func authorizeOwnerMessage(t *testing.T, f runtimeFixture, task RuntimeTask, in RuntimeMessageInput, v RuntimeMessageTargetVerification) (RuntimeMessageAction, bool, error) {
	t.Helper()
	var a RuntimeMessageAction
	var created bool
	_, err := f.s.Mutate(context.Background(), Request{Scope: "global", Command: "test.owner.message"}, func(tx *Tx) (any, error) {
		var e error
		a, created, e = tx.AuthorizeRuntimeMessageAction(context.Background(), task.ID, in, v)
		return a, e
	})
	return a, created, err
}

func TestRuntimeOwnerMessageAuditsDecisionAndNeverResendsUnknown(t *testing.T) {
	f, task, in, v := ownerMessageFixture(t)
	a, created, err := authorizeOwnerMessage(t, f, task, in, v)
	if err != nil || !created || a.State != "sending" {
		t.Fatalf("authorize: %+v %v %v", a, created, err)
	}
	if again, newly, e := authorizeOwnerMessage(t, f, task, in, v); e != nil || newly || again.ID != a.ID {
		t.Fatalf("sending retry: %+v %t %v", again, newly, e)
	}
	runtimeMutate(t, f.s, "test.message.unknown", func(tx *Tx) (any, error) {
		return tx.RecordRuntimeMessageResult(context.Background(), a.ID, "unknown", "", "network timed out")
	})
	if again, newly, e := authorizeOwnerMessage(t, f, task, in, v); e != nil || newly || again.State != "unknown" {
		t.Fatalf("unknown retry: %+v %t %v", again, newly, e)
	}
	in.Content = "different message"
	if _, _, e := authorizeOwnerMessage(t, f, task, in, v); ErrorCode(e) != "conflict" {
		t.Fatalf("changed payload reused idempotency key: %v", e)
	}
	actions, e := RuntimeMessageActions(context.Background(), f.s.DB, task.ID)
	if e != nil || len(actions) != 1 || actions[0].OwnerProfile != "corp1:user1" || actions[0].Reason == "" {
		t.Fatalf("audit: %+v %v", actions, e)
	}
}

func TestRuntimeOwnerMessageRejectsStaleSourcesPolicyAndIdentity(t *testing.T) {
	for _, name := range []string{"attempt", "source", "policy", "identity", "owner", "owner_basis", "target", "stopped", "channel_status"} {
		t.Run(name, func(t *testing.T) {
			f, task, in, v := ownerMessageFixture(t)
			switch name {
			case "attempt":
				in.AttemptID = "missing"
			case "source":
				_, _ = f.s.DB.Exec("UPDATE messages SET availability='recalled' WHERE id=?", in.EvidenceMessageIDs[0])
			case "policy":
				_, _ = f.s.DB.Exec("UPDATE runtime_configs SET external_actions='owner_confirmation' WHERE id=?", f.config.ID)
			case "identity":
				v.OwnerProfile = "other"
			case "owner":
				if _, e := f.s.DB.Exec("UPDATE runtime_configs SET owner_principal_id=(SELECT sender_principal FROM messages WHERE id=?) WHERE id=?", in.EvidenceMessageIDs[0], f.config.ID); e != nil {
					t.Fatal(e)
				}
			case "owner_basis":
				if _, e := f.s.DB.Exec("UPDATE identity_aliases SET basis='platform_event' WHERE tenant='corp1' AND id_type='user_id' AND id_value='user1'"); e != nil {
					t.Fatal(e)
				}
			case "channel_status":
				if _, e := f.s.DB.Exec("UPDATE channels SET status='disabled' WHERE id=?", f.channel.ID); e != nil {
					t.Fatal(e)
				}
			case "target":
				v.TargetID = "cid:other"
			case "stopped":
				_, _ = f.s.DB.Exec("UPDATE runtime_configs SET status='paused' WHERE id=?", f.config.ID)
			}
			if _, _, err := authorizeOwnerMessage(t, f, task, in, v); err == nil {
				t.Fatal("unsafe communication accepted")
			}
		})
	}
}

func TestRuntimeOwnerMessageAcceptsActiveChannel(t *testing.T) {
	f, task, in, v := ownerMessageFixture(t)
	if _, err := f.s.DB.Exec("UPDATE channels SET status='active' WHERE id=?", f.channel.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := authorizeOwnerMessage(t, f, task, in, v); err != nil || !created {
		t.Fatalf("active channel rejected: %v", err)
	}
}

func TestRuntimeOwnerMessageChecksDisclosureWithoutRestrictingOrigin(t *testing.T) {
	f, task, in, v := ownerMessageFixture(t)
	in.TargetID = "cid:related-other-group"
	v.TargetID = in.TargetID
	if _, _, err := authorizeOwnerMessage(t, f, task, in, v); ErrorCode(err) != "denied" {
		t.Fatalf("cross-audience disclosure accepted: %v", err)
	}
	// A verified related audience can receive a non-disclosing coordination
	// request; it need not be the observed conversation.
	in.EvidenceMessageIDs = []string{}
	in.Content = "Could the project maintainer help investigate?"
	if _, created, err := authorizeOwnerMessage(t, f, task, in, v); err != nil || !created {
		t.Fatalf("related outreach rejected: %v", err)
	}
	in.IdempotencyKey = "owner-update"
	in.TargetType = "user"
	in.TargetID = "user1"
	in.EvidenceMessageIDs = []string{task.Messages[0].ID}
	v.TargetType = in.TargetType
	v.TargetID = in.TargetID
	if _, created, err := authorizeOwnerMessage(t, f, task, in, v); err != nil || !created {
		t.Fatalf("owner update rejected: %v", err)
	}
}

func TestRuntimeOwnerMessageDelayedReceiptEchoAndRetention(t *testing.T) {
	ctx := context.Background()
	f, task, in, v := ownerMessageFixture(t)
	a, _, err := authorizeOwnerMessage(t, f, task, in, v)
	if err != nil {
		t.Fatal(err)
	}
	runtimeMutate(t, f.s, "test.message.accept", func(tx *Tx) (any, error) {
		return tx.RecordRuntimeMessageResult(ctx, a.ID, "accepted", `{"openTaskId":"send-task"}`, "")
	})
	if echo, e := RuntimeAgentMessageEcho(ctx, f.s.DB, f.channel.ID, f.watch.ConversationID, "send-task"); e != nil || echo {
		t.Fatalf("send task mistaken for message: %t %v", echo, e)
	}
	if waiting, e := RuntimeAgentMessageAwaitingReceipt(ctx, f.s.DB, f.channel.ID, in.TargetID, "2999-01-01T00:00:00Z"); e != nil || !waiting {
		t.Fatalf("unresolved echo was not deferred: %t %v", waiting, e)
	}
	if waiting, e := RuntimeAgentMessageAwaitingReceipt(ctx, f.s.DB, f.channel.ID, "cid:unrelated", "2999-01-01T00:00:00Z"); e != nil || waiting {
		t.Fatalf("unrelated owner was deferred: %t %v", waiting, e)
	}
	unresolved, err := UnresolvedRuntimeMessageActions(ctx, f.s.DB, f.channel.ID)
	if err != nil || len(unresolved) != 1 {
		t.Fatalf("unresolved: %+v %v", unresolved, err)
	}
	runtimeMutate(t, f.s, "test.message.reconcile", func(tx *Tx) (any, error) {
		return tx.ReconcileRuntimeMessageResult(ctx, a.ID, "accepted", `{"send":{"openTaskId":"send-task"},"delivery":{"openMessageId":"agent-message"}}`, "")
	})
	if echo, e := RuntimeAgentMessageEcho(ctx, f.s.DB, f.channel.ID, f.watch.ConversationID, "agent-message"); e != nil || !echo {
		t.Fatalf("agent echo missed: %t %v", echo, e)
	}
	if echo, e := RuntimeAgentMessageEcho(ctx, f.s.DB, f.channel.ID, "cid:another-group", "agent-message"); e != nil || echo {
		t.Fatalf("provider ID collision in another conversation suppressed a human: %t %v", echo, e)
	}
	if waiting, e := RuntimeAgentMessageAwaitingReceipt(ctx, f.s.DB, f.channel.ID, in.TargetID, "2999-01-01T00:00:00Z"); e != nil || waiting {
		t.Fatalf("resolved owner message remains deferred: %t %v", waiting, e)
	}
	if echo, e := RuntimeAgentMessageEcho(ctx, f.s.DB, f.channel.ID, f.watch.ConversationID, "human-owner-message"); e != nil || echo {
		t.Fatalf("human owner suppressed: %t %v", echo, e)
	}
	if _, err = f.s.DB.Exec("UPDATE messages SET availability='expired' WHERE id=?", in.EvidenceMessageIDs[0]); err != nil {
		t.Fatal(err)
	}
	actions, err := RuntimeMessageActions(ctx, f.s.DB, task.ID)
	if err != nil || len(actions) != 1 || actions[0].Content != "" || actions[0].Reason != "" || actions[0].Receipt != "" {
		t.Fatalf("expired output leaked: %+v %v", actions, err)
	}
	prior, found, err := ExistingRuntimeMessageAction(ctx, f.s.DB, task.ID, in)
	if err != nil || !found || prior.Content != "" {
		t.Fatalf("idempotency bypassed retention: %+v %v", prior, err)
	}
}

func TestRuntimeOwnerMessageFlightResultSurvivesConfigInvalidation(t *testing.T) {
	ctx := context.Background()
	f, task, in, v := ownerMessageFixture(t)
	a, _, err := authorizeOwnerMessage(t, f, task, in, v)
	if err != nil {
		t.Fatal(err)
	}
	// Deterministically interleave a configuration change while the external
	// call is in flight. Its authorization is revoked, but its outcome is fact.
	runtimeMutate(t, f.s, "test.message.config_changed", func(tx *Tx) (any, error) { return nil, tx.InvalidateRuntimeConfigWork(ctx, f.config.ID) })
	var state string
	if err = f.s.DB.QueryRow("SELECT state FROM runtime_message_actions WHERE id=?", a.ID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("in-flight state=%s %v", state, err)
	}
	runtimeMutate(t, f.s, "test.message.late_result", func(tx *Tx) (any, error) {
		return tx.RecordRuntimeMessageResult(ctx, a.ID, "accepted", `{"openTaskId":"native-task","openMessageId":"sent-once"}`, "")
	})
	if echo, e := RuntimeAgentMessageEcho(ctx, f.s.DB, f.channel.ID, in.TargetID, "sent-once"); e != nil || !echo {
		t.Fatalf("late result evidence lost: %t %v", echo, e)
	}
	if _, created, e := authorizeOwnerMessage(t, f, task, in, v); e != nil || created {
		t.Fatalf("late result caused resend: %t %v", created, e)
	}
}

func TestRuntimeOwnerMessageReconcilesAfterSourceAndRawReceiptRemoval(t *testing.T) {
	ctx := context.Background()
	f, task, in, v := ownerMessageFixture(t)
	a, _, err := authorizeOwnerMessage(t, f, task, in, v)
	if err != nil {
		t.Fatal(err)
	}
	runtimeMutate(t, f.s, "test.message.submit", func(tx *Tx) (any, error) {
		return tx.RecordRuntimeMessageResult(ctx, a.ID, "accepted", `{"openTaskId":"retained-native-token","text":"private text"}`, "")
	})
	if _, err = f.s.DB.Exec("UPDATE messages SET availability='expired' WHERE id=?", in.EvidenceMessageIDs[0]); err != nil {
		t.Fatal(err)
	}
	// Retention destroys raw output but keeps native identifiers, which cannot
	// reconstruct its content and are required to identify the later echo.
	if _, err = f.s.DB.Exec("UPDATE runtime_message_actions SET content='',reason='',receipt='',detail='' WHERE id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := UnresolvedRuntimeMessageActions(ctx, f.s.DB, f.channel.ID)
	if err != nil || len(rows) != 1 || rows[0].ProviderSendTaskID != "retained-native-token" || rows[0].Content != "" || rows[0].Reason != "" || len(rows[0].EvidenceMessageIDs) != 0 || strings.Contains(rows[0].Receipt, "private text") {
		t.Fatalf("retention broke minimal status query: %+v %v", rows, err)
	}
	runtimeMutate(t, f.s, "test.message.status", func(tx *Tx) (any, error) {
		return tx.ReconcileRuntimeMessageResult(ctx, a.ID, "accepted", `{"send":{"openTaskId":"retained-native-token"},"delivery":{"openMessageId":"late-echo","text":"do not restore private output"}}`, "")
	})
	var receipt string
	if err = f.s.DB.QueryRow("SELECT receipt FROM runtime_message_actions WHERE id=?", a.ID).Scan(&receipt); err != nil || receipt != "" {
		t.Fatalf("late status restored raw text: %q %v", receipt, err)
	}
	if echo, e := RuntimeAgentMessageEcho(ctx, f.s.DB, f.channel.ID, in.TargetID, "late-echo"); e != nil || !echo {
		t.Fatalf("expired source prevented echo identification: %t %v", echo, e)
	}
	if waiting, e := RuntimeAgentMessageAwaitingReceipt(ctx, f.s.DB, f.channel.ID, in.TargetID, "2999-01-01T00:00:00Z"); e != nil || waiting {
		t.Fatalf("late receipt did not release human observations: %t %v", waiting, e)
	}
}

func TestRuntimeOwnerMessagePinsAdditionalEvidenceRevision(t *testing.T) {
	ctx := context.Background()
	f, task, in, v := ownerMessageFixture(t)
	sent := time.Now().Add(2 * time.Hour)
	extra := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "additional-evidence", Sender{IDType: "user_id", IDValue: "colleague"}, "Original supporting details", sent)
	in.EvidenceMessageIDs = append(in.EvidenceMessageIDs, extra.MessageID)
	a, _, err := authorizeOwnerMessage(t, f, task, in, v)
	if err != nil {
		t.Fatal(err)
	}
	if a.EvidenceMessageRevisions[extra.MessageID] != 1 {
		t.Fatalf("additional source was not versioned: %+v", a.EvidenceMessageRevisions)
	}
	runtimeMutate(t, f.s, "test.message.accept", func(tx *Tx) (any, error) {
		return tx.RecordRuntimeMessageResult(ctx, a.ID, "accepted", `{"openTaskId":"task-with-evidence"}`, "")
	})
	intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "additional-evidence", Sender{IDType: "user_id", IDValue: "colleague"}, "Corrected supporting details", sent)
	if current, e := RuntimeTaskOutputCurrent(ctx, f.s.DB, task.ID); e != nil || !current {
		t.Fatalf("fixture unexpectedly edited original task evidence: %t %v", current, e)
	}
	rows, err := RuntimeMessageActions(ctx, f.s.DB, task.ID)
	if err != nil || len(rows) != 1 || rows[0].Content != "" || rows[0].Receipt != "" {
		t.Fatalf("edited auxiliary evidence did not redact output: %+v %v", rows, err)
	}
	internal, err := UnresolvedRuntimeMessageActions(ctx, f.s.DB, f.channel.ID)
	if err != nil || len(internal) != 1 || internal[0].Content != "" || internal[0].ProviderSendTaskID == "" {
		t.Fatalf("read-only reconciliation lost after auxiliary edit: %+v %v", internal, err)
	}
}

func TestRuntimeOwnerMessageWaitIncludesSameSecondAndScopesDirectEcho(t *testing.T) {
	ctx := context.Background()
	f, task, in, v := ownerMessageFixture(t)
	a, _, err := authorizeOwnerMessage(t, f, task, in, v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("UPDATE runtime_message_actions SET created_at='2026-09-17T04:00:00.500Z' WHERE id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	if waiting, e := RuntimeAgentMessageAwaitingReceipt(ctx, f.s.DB, f.channel.ID, in.TargetID, "2026-09-17T04:00:00Z"); e != nil || !waiting {
		t.Fatalf("second-precision echo escaped waiting: %t %v", waiting, e)
	}
	if waiting, e := RuntimeAgentMessageAwaitingReceipt(ctx, f.s.DB, f.channel.ID, in.TargetID, "2026-09-17T03:59:59Z"); e != nil || waiting {
		t.Fatalf("earlier human owner message held: %t %v", waiting, e)
	}
	in.IdempotencyKey = "direct-native"
	in.TargetType = "user"
	in.TargetID = "user1"
	v.TargetType = "user"
	v.TargetID = "user1"
	direct, _, err := authorizeOwnerMessage(t, f, task, in, v)
	if err != nil {
		t.Fatal(err)
	}
	runtimeMutate(t, f.s, "test.message.direct", func(tx *Tx) (any, error) {
		return tx.RecordRuntimeMessageResult(ctx, direct.ID, "accepted", `{"send":{"openTaskId":"dm-task"},"delivery":{"openMessageId":"dm-echo","openConversationId":"cid:verified-native-dm"}}`, "")
	})
	if echo, e := RuntimeAgentMessageEcho(ctx, f.s.DB, f.channel.ID, "cid:verified-native-dm", "dm-echo"); e != nil || !echo {
		t.Fatalf("native DM receipt not linked: %t %v", echo, e)
	}
	if echo, e := RuntimeAgentMessageEcho(ctx, f.s.DB, f.channel.ID, "cid:someone-else", "dm-echo"); e != nil || echo {
		t.Fatalf("DM receipt crossed audience: %t %v", echo, e)
	}
}

func TestRuntimeOwnerMessageFailedPartialReceiptStillIdentifiesSentEcho(t *testing.T) {
	ctx := context.Background()
	f, task, in, v := ownerMessageFixture(t)
	a, _, err := authorizeOwnerMessage(t, f, task, in, v)
	if err != nil {
		t.Fatal(err)
	}
	runtimeMutate(t, f.s, "test.message.partial_failed", func(tx *Tx) (any, error) {
		return tx.RecordRuntimeMessageResult(ctx, a.ID, "failed", `{"send":{"openTaskId":"partial-send"},"delivery":{"failedCount":1,"openMessageId":"confirmed-part"}}`, "platform reported partial failure")
	})
	if echo, e := RuntimeAgentMessageEcho(ctx, f.s.DB, f.channel.ID, in.TargetID, "confirmed-part"); e != nil || !echo {
		t.Fatalf("confirmed part reentered observation after failure: %t %v", echo, e)
	}
}

func TestRuntimeOwnerMessageOldTaskVersionRemainsRedactedWithCurrentSources(t *testing.T) {
	ctx := context.Background()
	f, task, in, verified := ownerMessageFixture(t)
	// Generic coordination has no explicit citations, but still belongs to
	// the task version that authorized it.
	in.EvidenceMessageIDs = []string{}
	action, _, err := authorizeOwnerMessage(t, f, task, in, verified)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("UPDATE runtime_tasks SET version=version+1 WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	if current, e := RuntimeTaskOutputCurrent(ctx, f.s.DB, task.ID); e != nil || !current {
		t.Fatalf("task sources should remain current: %t %v", current, e)
	}
	// The old in-flight call still records its native identifiers, without
	// restoring raw output for the new task version.
	runtimeMutate(t, f.s, "test.message.old_version_receipt", func(tx *Tx) (any, error) {
		return tx.RecordRuntimeMessageResult(ctx, action.ID, "accepted", `{"send":{"openTaskId":"old-version-native"},"delivery":{"openMessageId":"old-version-echo","text":"old private result"}}`, "old result detail")
	})
	rows, err := RuntimeMessageActions(ctx, f.s.DB, task.ID)
	if err != nil || len(rows) != 1 || rows[0].Content != "" || rows[0].Reason != "" || rows[0].Receipt != "" || rows[0].ProviderSendTaskID != "old-version-native" {
		t.Fatalf("old version output leaked or metadata lost: %+v %v", rows, err)
	}
	prior, found, err := ExistingRuntimeMessageAction(ctx, f.s.DB, task.ID, in)
	if err != nil || !found || prior.Content != "" || prior.Reason != "" || prior.State != "accepted" {
		t.Fatalf("idempotency read restored old version output: %+v %v", prior, err)
	}
	var storedReceipt string
	if err = f.s.DB.QueryRow("SELECT receipt FROM runtime_message_actions WHERE id=?", action.ID).Scan(&storedReceipt); err != nil || storedReceipt != "" {
		t.Fatalf("old receipt text persisted: %q %v", storedReceipt, err)
	}
	if echo, e := RuntimeAgentMessageEcho(ctx, f.s.DB, f.channel.ID, in.TargetID, "old-version-echo"); e != nil || !echo {
		t.Fatalf("old version lost echo identity: %t %v", echo, e)
	}
}
