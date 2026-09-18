package core

import (
	"context"
	"testing"
	"time"
)

type fixtureMessageVerifier struct {
	byKind map[string]PlatformMessageProof
}

func (v fixtureMessageVerifier) LookupMessage(_ context.Context, c Channel, _ MessageView) (PlatformMessageProof, error) {
	return v.byKind[c.Kind], nil
}

func TestCrossTransportAssociationRequiresIndependentPlatformProof(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	dws, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	app, _ := fixtureChannel(t, s, ChannelDingTalkApp, "cid:group1")
	sender := Sender{IDType: "staff_id", IDValue: "peer"}
	first := intakeRuntimeMessage(t, s, dws, "cid:group1", "dws-message", sender, "same words", time.Now())
	second := intakeRuntimeMessage(t, s, app, "cid:group1", "app-message", sender, "same words", time.Now())
	if first.MessageID == second.MessageID {
		t.Fatal("two provider namespaces merged before proof")
	}
	lookup := fixtureMessageVerifier{byKind: map[string]PlatformMessageProof{
		ChannelDwsPersonal: {Issuer: "platform_lookup", GroupKey: "group-verified", EventKey: "event-a"},
		ChannelDingTalkApp: {Issuer: "platform_lookup", GroupKey: "group-verified", EventKey: "event-b"},
	}}
	var unresolved MessageAssociation
	runtimeMutate(t, s, "message.association.unresolved", func(tx *Tx) (any, error) {
		var err error
		unresolved, err = tx.AssociateVerifiedMessages(ctx, first.MessageID, second.MessageID, lookup)
		return unresolved, err
	})
	if unresolved.Status != "unresolved" {
		t.Fatalf("different platform events were merged: %+v", unresolved)
	}
	status, err := MessageAssociationStatus(ctx, s.DB, first.MessageID)
	if err != nil || status.Status != "unresolved" {
		t.Fatalf("unresolved inspection: %+v, %v", status, err)
	}
	if _, err := ReadMessageAssociation(ctx, s.DB, first.MessageID); ErrorCode(err) != "not_found" {
		t.Fatalf("an unresolved guess was persisted as verified: %v", err)
	}
	lookup.byKind[ChannelDingTalkApp] = lookup.byKind[ChannelDwsPersonal]
	var verified MessageAssociation
	runtimeMutate(t, s, "message.association.verified", func(tx *Tx) (any, error) {
		var err error
		verified, err = tx.AssociateVerifiedMessages(ctx, first.MessageID, second.MessageID, lookup)
		return verified, err
	})
	if verified.Status != "verified" || verified.ProofDigest == "" || verified.ID == "" {
		t.Fatalf("platform proof was not recorded: %+v", verified)
	}
	again, err := ReadMessageAssociation(ctx, s.DB, second.MessageID)
	if err != nil || again.ID != verified.ID {
		t.Fatalf("association lookup: %+v %v", again, err)
	}
	var total int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM verified_message_associations").Scan(&total); err != nil || total != 1 {
		t.Fatalf("association count %d, %v", total, err)
	}
}

func TestLateVerifiedAssociationClaimsExistingBatch(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 1, 300)
	app, _ := fixtureChannel(t, f.s, ChannelDingTalkApp, f.watch.ConversationID)
	sender := Sender{IDType: "staff_id", IDValue: "peer"}
	dws := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "dws-late", sender, "question", time.Now())
	bot := intakeRuntimeMessage(t, f.s, app, f.watch.ConversationID, "bot-late", sender, "question", time.Now())
	runtimeMutate(t, f.s, "runtime.sync.before-proof", func(tx *Tx) (any, error) {
		return tx.SyncRuntimeMessages(ctx, f.config.ID)
	})
	runtimeMutate(t, f.s, "runtime.claim.before-proof", func(tx *Tx) (any, error) {
		batch, err := tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now())
		if err == nil && (len(batch.Messages) != 1 || batch.Messages[0].ID != dws.MessageID) {
			t.Fatalf("first batch: %+v", batch)
		}
		return batch, err
	})
	proof := PlatformMessageProof{Issuer: "platform_lookup", GroupKey: "group", EventKey: "logical-event"}
	lookup := fixtureMessageVerifier{byKind: map[string]PlatformMessageProof{ChannelDwsPersonal: proof, ChannelDingTalkApp: proof}}
	var association MessageAssociation
	runtimeMutate(t, f.s, "message.association.after-claim", func(tx *Tx) (any, error) {
		var err error
		association, err = tx.AssociateVerifiedMessages(ctx, dws.MessageID, bot.MessageID, lookup)
		return association, err
	})
	if association.ClaimedMessageID != dws.MessageID || association.ClaimedRuntimeID != f.config.ID {
		t.Fatalf("late association lost the prior claim: %+v", association)
	}
}

func TestVerifiedAssociationPrioritizesGroupAgentEvenWhenProactiveTicksFirst(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 1, 300)
	var app Channel
	var appRoute Route
	runtimeMutate(t, f.s, "association.app.channel", func(tx *Tx) (any, error) {
		var err error
		app, err = tx.AddChannel(ctx, ChannelInput{Name: "verified-app", Kind: ChannelDingTalkApp,
			Identity: ChannelIdentity{ExpectedCorpID: f.channel.Tenant, ClientID: "verified-app", RobotCode: "robot", HistoryChannel: f.channel.ID}})
		return app, err
	})
	runtimeMutate(t, f.s, "association.app.route", func(tx *Tx) (any, error) {
		var err error
		appRoute, err = tx.AddRoute(ctx, app.ID, RouteInput{ConversationID: f.watch.ConversationID, ConversationType: "group",
			Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		return appRoute, err
	})
	var group RuntimeConfig
	runtimeMutate(t, f.s, "association.app.runtime", func(tx *Tx) (any, error) {
		var err error
		group, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "verified-group", Channel: app.ID, RouteIDs: []string{appRoute.ID},
			DeliveryRouteID: appRoute.ID, Owner: f.owner, ApplicationMode: "group_mention", ContextChannel: f.channel.ID, ReconcileSeconds: 10})
		return group, err
	})
	runtimeMutate(t, f.s, "association.app.start", func(tx *Tx) (any, error) { return tx.SetRuntimeStatus(ctx, group.ID, "running", "") })
	sender := Sender{IDType: "staff_id", IDValue: "peer"}
	dws := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "same-logical-dws", sender, "Can you check?", time.Now().Add(time.Second))
	var bot IntakeResult
	runtimeMutate(t, f.s, "association.app.message", func(tx *Tx) (any, error) {
		var err error
		bot, err = tx.Intake(ctx, app.ID, NormalizedEvent{Kind: EventMessage, Adapter: "fake_app", ParseVersion: "1", Origin: "stream",
			ProviderMessageID: "same-logical-app", ConversationID: appRoute.ConversationID, Tenant: app.Tenant,
			Sender: sender, Body: "Can you check?", Mentioned: true, SentAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)})
		return bot, err
	})
	for _, runtimeID := range []string{f.config.ID, group.ID} {
		runtimeMutate(t, f.s, "association.sync."+runtimeID, func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, runtimeID) })
	}
	proof := PlatformMessageProof{Issuer: "platform_lookup", GroupKey: "verified-group", EventKey: "verified-logical-event"}
	lookup := fixtureMessageVerifier{byKind: map[string]PlatformMessageProof{ChannelDwsPersonal: proof, ChannelDingTalkApp: proof}}
	runtimeMutate(t, f.s, "association.link", func(tx *Tx) (any, error) {
		return tx.AssociateVerifiedMessages(ctx, dws.MessageID, bot.MessageID, lookup)
	})
	var first RuntimeBatch
	runtimeMutate(t, f.s, "association.first.batch", func(tx *Tx) (any, error) {
		var err error
		first, err = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now().Add(time.Second))
		return first, err
	})
	if first.ID != "" {
		t.Fatalf("proactive mode claimed a verified group request: %+v", first)
	}
	var second RuntimeBatch
	runtimeMutate(t, f.s, "association.second.batch", func(tx *Tx) (any, error) {
		var err error
		second, err = tx.ClaimRuntimeBatch(ctx, group.ID, time.Now().Add(time.Second))
		return second, err
	})
	if len(second.Messages) != 1 || second.Messages[0].ID != bot.MessageID {
		t.Fatalf("group Agent did not claim verified request: %+v", second)
	}
	var state string
	if err := f.s.DB.QueryRowContext(ctx, "SELECT state FROM runtime_message_states WHERE runtime_id=? AND message_id=?", f.config.ID, dws.MessageID).Scan(&state); err != nil || state != "linked_duplicate" {
		t.Fatalf("proactive mode state %q, %v", state, err)
	}
	var tasks []RuntimeTask
	runtimeMutate(t, f.s, "association.group.task", func(tx *Tx) (any, error) {
		var err error
		tasks, err = tx.CompleteRuntimeBatch(ctx, second, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "mention:" + bot.MessageID,
			Title: "answer", Instructions: "answer", MessageIDs: []string{bot.MessageID}}}})
		return tasks, err
	})
	if len(tasks) != 1 {
		t.Fatalf("group task: %+v", tasks)
	}
	shown, err := ReadRuntimeTask(ctx, f.s.DB, tasks[0].ID)
	if err != nil || len(shown.Associations) != 1 || shown.Associations[0].ClaimedMessageID != bot.MessageID {
		t.Fatalf("task result lost verified observation link: %+v, %v", shown.Associations, err)
	}
}
