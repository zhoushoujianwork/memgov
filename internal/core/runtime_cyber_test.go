package core

import (
	"context"
	"testing"
	"time"
)

// Confirmation and reply regression cases belong to an actual owner-private
// application runtime; the old DWS observation/owner-DM bridge is gone.
func newOwnerInteractiveFixture(t *testing.T) runtimeFixture {
	t.Helper()
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	var app Channel
	var route Route
	var config RuntimeConfig
	owner := Sender{IDType: "user_id", IDValue: f.channel.Identity.ExpectedUserID}
	runtimeMutate(t, f.s, "interactive.fixture", func(tx *Tx) (any, error) {
		if _, err := tx.AttestDWSOwner(ctx, f.channel.ID, f.channel.ConfigVersion); err != nil {
			return nil, err
		}
		var err error
		app, err = tx.AddChannel(ctx, ChannelInput{Name: "interactive-owner", Kind: ChannelDingTalkApp, Identity: ChannelIdentity{ExpectedCorpID: f.channel.Tenant, ClientID: "client", RobotCode: "bot", HistoryChannel: f.channel.ID}})
		if err != nil {
			return nil, err
		}
		route, err = tx.AddRoute(ctx, app.ID, RouteInput{ConversationID: owner.IDValue, ConversationType: "direct", Mode: "assistant", Triggers: []string{"direct"}, SendPolicy: "dispatch_only"})
		if err != nil {
			return nil, err
		}
		off := false
		config, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "interactive-owner", Channel: app.ID, RouteIDs: []string{route.ID}, DeliveryRouteID: route.ID, Owner: owner, ApplicationMode: "direct", AgentBash: &off, ExternalActions: "owner_confirmation", ItemThreshold: 1})
		if err != nil {
			return nil, err
		}
		config, err = tx.SetRuntimeStatus(ctx, config.ID, "running", "")
		return config, err
	})
	f.channel, f.watch, f.direct, f.config, f.owner = app, route, route, config, owner
	return f
}

func TestCyberOwnerWithoutDeliveryRouteInvestigatesAndRecordsBlockedWork(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	runtimeMutate(t, f.s, "cyber.configure", func(tx *Tx) (any, error) {
		var err error
		f.config, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "cyber", Channel: f.channel.ID, RouteIDs: []string{f.watch.ID}, Owner: f.owner, ItemThreshold: 1})
		if err != nil {
			return nil, err
		}
		f.config, err = tx.SetRuntimeStatus(ctx, f.config.ID, "running", "")
		return f.config, err
	})
	if f.config.DeliveryRouteID != "" || f.config.CompletionPolicy != "record_only" || !f.config.AgentBash || f.config.ExternalActions != "owner_delegated" {
		t.Fatalf("incorrect Cyber owner defaults: %+v", f.config)
	}
	m := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "investigate", f.owner, "需要定位故障，先查环境", time.Now().Add(time.Hour))
	runtimeMutate(t, f.s, "cyber.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var task RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "cyber.investigate", func(tx *Tx) (any, error) {
		batch, err := tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now().Add(2*time.Hour))
		if err != nil {
			return nil, err
		}
		tasks, err := tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "investigation", Title: "排查", Instructions: "查询背景和环境", NeedsClarification: true, MessageIDs: []string{m.MessageID}}}})
		if err != nil {
			return nil, err
		}
		if len(tasks) != 1 || tasks[0].Status != "pending" {
			t.Fatalf("missing input prevented investigation: %+v", tasks)
		}
		task, attempt, err = tx.ClaimRuntimeTask(ctx, f.config.ID, NewID(), "fake", "preset", "commit", "")
		return task, err
	})
	runtimeMutate(t, f.s, "cyber.blocked", func(tx *Tx) (any, error) {
		var err error
		task, err = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "已定位，生产修改超出当前委托", Actions: []RuntimeAction{{Kind: "production_change", Target: "production", Payload: "requires a separate owner delegation"}}})
		return task, err
	})
	if task.Status != "blocked" {
		t.Fatalf("background blocker: %+v", task)
	}
	for _, purpose := range []string{"result", RuntimeReceiptPurpose, RuntimeProcessingReceiptPurpose, RuntimeCompletionReceiptPurpose, RuntimeFailureReceiptPurpose, "confirmation"} {
		_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "cyber.no-notice"}, func(tx *Tx) (any, error) { return tx.prepareTaskDelivery(ctx, task.ID, purpose, false) })
		if err == nil {
			t.Fatalf("background %s generated a notification", purpose)
		}
	}
	var count int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE job_id=?", task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("background created outbox: %d %v", count, err)
	}
}

func TestGroupConversationContextWithoutDWSStaysInApplicationConversation(t *testing.T) {
	f := newOwnerInteractiveFixture(t)
	ctx := context.Background()
	var first, second Route
	var config RuntimeConfig
	runtimeMutate(t, f.s, "bot.groups", func(tx *Tx) (any, error) {
		var err error
		first, err = tx.AddRoute(ctx, f.channel.ID, RouteInput{ConversationID: "cid:first", ConversationType: "group", Mode: "assistant", Triggers: []string{"mention"}, SendPolicy: "reply_to_trigger"})
		if err != nil {
			return nil, err
		}
		second, err = tx.AddRoute(ctx, f.channel.ID, RouteInput{ConversationID: "cid:second", ConversationType: "group", Mode: "assistant", Triggers: []string{"mention"}, SendPolicy: "reply_to_trigger"})
		if err != nil {
			return nil, err
		}
		config, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "bot-groups", Channel: f.channel.ID, RouteIDs: []string{first.ID, second.ID}, DeliveryRouteID: first.ID, Owner: f.owner, ApplicationMode: "group_mention"})
		return config, err
	})
	if config.ContextChannelID != "" {
		t.Fatal("optional history was implicitly enabled")
	}
	message := intakeRuntimeMessage(t, f.s, f.channel, first.ConversationID, "first-history", f.owner, "当前群上下文", time.Now().Add(time.Hour))
	intakeRuntimeMessage(t, f.s, f.channel, second.ConversationID, "other-group", f.owner, "其他群不可见", time.Now().Add(time.Hour))
	intakeRuntimeMessage(t, f.s, f.channel, f.direct.ConversationID, "owner-private", f.owner, "owner私聊不可见", time.Now().Add(time.Hour))
	contextMessages, err := RuntimeTaskConversationContext(ctx, f.s.DB, config, RuntimeTask{RouteID: first.ID}, 30)
	if err != nil || len(contextMessages) != 1 || contextMessages[0].ID != message.MessageID {
		t.Fatalf("application context isolation: %+v %v", contextMessages, err)
	}
}

func TestSourceDiscoveryWithoutRobotKeepsFrozenScope(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	var source DataSource
	runtimeMutate(t, f.s, "source.no-bot", func(tx *Tx) (any, error) {
		_, err := tx.SetChannelCapabilities(ctx, f.channel.ID, Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake")
		if err != nil {
			return nil, err
		}
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "no-bot", Channel: f.channel.ID, Workspace: "global"})
		if err != nil {
			return nil, err
		}
		c, err := ReadChannel(ctx, tx.Conn, f.channel.ID)
		if err != nil {
			return nil, err
		}
		groups := []DataSourceGroup{{ID: f.watch.ConversationID}}
		if _, err = tx.SyncDataSourceGroupDetails(ctx, source.ID, groups); err != nil {
			return nil, err
		}
		return nil, tx.RecordSourceGroupDiscovery(ctx, source.ID, source.Version, c.ConfigVersion, groups)
	})
	source, _ = ReadDataSource(ctx, f.s.DB, source.ID)
	routes, err := ReadSourcePositiveProcessingRoutes(ctx, f.s.DB, source, time.Now())
	if err != nil || len(routes) != 1 || routes[0] != f.watch.ID {
		t.Fatalf("source requires bot: routes=%v err=%v", routes, err)
	}
	if _, err = f.s.DB.Exec("UPDATE data_sources SET version=version+1 WHERE id=?", source.ID); err != nil {
		t.Fatal(err)
	}
	source, _ = ReadDataSource(ctx, f.s.DB, source.ID)
	routes, err = ReadSourcePositiveProcessingRoutes(ctx, f.s.DB, source, time.Now())
	if err != nil || len(routes) != 0 {
		t.Fatalf("old discovery widened new scope: %v %v", routes, err)
	}
}
