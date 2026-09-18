package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

type runtimeFixture struct {
	s       *Store
	channel Channel
	watch   Route
	direct  Route
	config  RuntimeConfig
	owner   Sender
}

func newRuntimeFixture(t *testing.T, threshold, wait int) runtimeFixture {
	t.Helper()
	ctx := context.Background()
	s := testStore(t)
	c, watch := fixtureChannel(t, s, ChannelDwsPersonal, "cid:watch")
	var direct Route
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "test.route"}, func(tx *Tx) (any, error) {
		var addErr error
		direct, addErr = tx.AddRoute(ctx, c.ID, RouteInput{ConversationID: "cid:owner", ConversationType: "direct", Mode: "notify"})
		return direct, addErr
	})
	if err != nil {
		t.Fatal(err)
	}
	var updated any
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "test.route.enable"}, func(tx *Tx) (any, error) {
		var updateErr error
		updated, updateErr = tx.UpdateRoute(ctx, direct.ID, direct.Version, RouteInput{SendPolicy: "dispatch_only"}, "enable runtime owner delivery")
		return updated, updateErr
	})
	if err != nil {
		t.Fatal(err)
	}
	direct, err = ReadRoute(ctx, s.DB, direct.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner := Sender{IDType: "staff_id", IDValue: "owner-1"}
	// A platform event creates the verified exact-identifier mapping used for
	// authorization. The message predates runtime configuration and is context.
	intakeRuntimeMessage(t, s, c, direct.ConversationID, "owner-bootstrap", owner, "bootstrap", time.Now().Add(-time.Hour))
	var cfg RuntimeConfig
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "runtime.configure"}, func(tx *Tx) (any, error) {
		var configErr error
		cfg, configErr = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "watcher", Channel: c.ID, RouteIDs: []string{watch.ID}, DeliveryRouteID: direct.ID, Owner: owner, ItemThreshold: threshold, MaxWaitSeconds: wait, ReconcileSeconds: 10})
		return cfg, configErr
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "runtime.start"}, func(tx *Tx) (any, error) {
		return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ = ReadRuntime(ctx, s.DB, cfg.ID)
	return runtimeFixture{s: s, channel: c, watch: watch, direct: direct, config: cfg, owner: owner}
}

func intakeRuntimeMessage(t *testing.T, s *Store, c Channel, conversation, providerID string, sender Sender, body string, sent time.Time) IntakeResult {
	t.Helper()
	ctx := context.Background()
	var out IntakeResult
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "test.intake"}, func(tx *Tx) (any, error) {
		var intakeErr error
		out, intakeErr = tx.Intake(ctx, c.ID, NormalizedEvent{Kind: EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "history", ProviderMessageID: providerID, ConversationID: conversation, Tenant: c.Tenant, Sender: sender, Body: body, Format: "text", SentAt: sent.UTC().Format(time.RFC3339Nano), EventAt: sent.UTC().Format(time.RFC3339Nano)})
		return out, intakeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func runtimeMutate(t *testing.T, s *Store, command string, fn func(*Tx) (any, error)) {
	t.Helper()
	if _, err := s.Mutate(context.Background(), Request{Scope: "global", Command: command}, fn); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeStatusListAggregatesAllCounters(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 1, 30)
	first := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "status-1", f.owner, "one", time.Now())
	second := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "status-2", f.owner, "two", time.Now())
	runtimeMutate(t, f.s, "test.runtime.status-list", func(tx *Tx) (any, error) {
		now := Now()
		for _, row := range []struct{ messageID, state string }{{first.MessageID, "pending"}, {second.MessageID, "waiting_receipt"}} {
			if _, err := tx.Conn.ExecContext(ctx, `INSERT INTO runtime_message_states(runtime_id,route_id,message_id,revision,state,first_seen_at) VALUES(?,?,?,?,?,?)`, f.config.ID, f.watch.ID, row.messageID, 1, row.state, now); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Conn.ExecContext(ctx, `INSERT INTO runtime_batches(id,runtime_id,route_id,status,input_digest,message_count,model,created_at,started_at) VALUES('batch-status',?,?, 'analyzing','digest',2,'test',?,?)`, f.config.ID, f.watch.ID, now, now); err != nil {
			return nil, err
		}
		for _, row := range []struct{ id, key, status string }{{"task-status-1", "status-1", "failed"}, {"task-status-2", "status-2", "completed"}} {
			if _, err := tx.Conn.ExecContext(ctx, `INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,title,instructions,status,created_at,updated_at) VALUES(?,?,?,?, 'title','instructions',?,?,?)`, row.id, f.config.ID, f.watch.ID, row.key, row.status, now, now); err != nil {
				return nil, err
			}
		}
		_, err := tx.Conn.ExecContext(ctx, `INSERT INTO runtime_pending_actions(id,task_id,task_version,kind,target,payload,payload_digest,status,created_at,updated_at) VALUES('action-status','task-status-1',1,'send','target','payload','digest','pending',?,?)`, now, now)
		return nil, err
	})
	statuses, err := RuntimeStatusList(ctx, f.s.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses: %+v", statuses)
	}
	got := statuses[0]
	if got.Runtime.ID != f.config.ID || got.PendingMessages != 1 || got.WaitingReceiptMessages != 1 || got.AnalyzingBatches != 1 || got.Tasks["failed"] != 1 || got.Tasks["completed"] != 1 || got.PendingActions != 1 {
		t.Fatalf("aggregated status: %+v", got)
	}
}

func TestGroupMentionRuntimeUsesBoundDWSContextAndRepliesToTrigger(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	dwsChannel, dwsRoute := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group-agent")
	owner := Sender{IDType: "staff_id", IDValue: "owner-1"}
	intakeRuntimeMessage(t, s, dwsChannel, dwsRoute.ConversationID, "history-1", Sender{IDType: "staff_id", IDValue: "peer"}, "项目代号是 Aurora", time.Now().Add(-time.Hour))

	var appChannel Channel
	var appRoute, ownerRoute Route
	runtimeMutate(t, s, "group.app.add", func(tx *Tx) (any, error) {
		var err error
		appChannel, err = tx.AddChannel(ctx, ChannelInput{Name: "group-app", Kind: ChannelDingTalkApp,
			Identity: ChannelIdentity{ExpectedCorpID: dwsChannel.Tenant, ClientID: "group-app", RobotCode: "group-bot", HistoryChannel: dwsChannel.ID}})
		return appChannel, err
	})
	runtimeMutate(t, s, "group.route.add", func(tx *Tx) (any, error) {
		var err error
		appRoute, err = tx.AddRoute(ctx, appChannel.ID, RouteInput{ConversationID: dwsRoute.ConversationID, ConversationType: "group",
			Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		return appRoute, err
	})
	runtimeMutate(t, s, "group.owner.route", func(tx *Tx) (any, error) {
		var err error
		ownerRoute, err = tx.AddRoute(ctx, appChannel.ID, RouteInput{ConversationID: "cid:owner-app", ConversationType: "direct", Mode: "notify", SendPolicy: "dispatch_only"})
		return ownerRoute, err
	})
	intakeRuntimeMessage(t, s, appChannel, ownerRoute.ConversationID, "owner-bootstrap", owner, "bootstrap", time.Now().Add(-time.Hour))

	var cfg RuntimeConfig
	runtimeMutate(t, s, "group.runtime.configure", func(tx *Tx) (any, error) {
		var err error
		cfg, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "group-helper", Channel: appChannel.ID, RouteIDs: []string{appRoute.ID},
			DeliveryRouteID: appRoute.ID, Owner: owner, ApplicationMode: "group_mention", ContextChannel: dwsChannel.ID, ReconcileSeconds: 10})
		return cfg, err
	})
	if cfg.ItemThreshold != 1 || cfg.MemoryScope != "conversation_published" || cfg.ContextChannelID != dwsChannel.ID {
		t.Fatalf("group runtime was not constrained: %+v", cfg)
	}
	runtimeMutate(t, s, "group.runtime.start", func(tx *Tx) (any, error) { return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "") })
	cfg, _ = ReadRuntime(ctx, s.DB, cfg.ID)
	intakeApp := func(id, body string, mentioned bool) IntakeResult {
		var out IntakeResult
		runtimeMutate(t, s, "group.intake."+id, func(tx *Tx) (any, error) {
			var err error
			out, err = tx.Intake(ctx, appChannel.ID, NormalizedEvent{Kind: EventMessage, Adapter: "dingtalk_app", ParseVersion: "1", Origin: "stream",
				ProviderMessageID: id, ConversationID: appRoute.ConversationID, ConversationType: "group", Tenant: appChannel.Tenant,
				Sender: Sender{IDType: "staff_id", IDValue: "peer"}, Body: body, Mentioned: mentioned, SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
			return out, err
		})
		return out
	}
	unmentioned := intakeApp("ordinary", "普通讨论", false)
	mentioned := intakeApp("question", "@机器人 项目代号是什么？", true)
	runtimeMutate(t, s, "group.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, cfg.ID) })
	var ordinaryState, mentionState string
	if err := s.DB.QueryRow("SELECT state FROM runtime_message_states WHERE runtime_id=? AND message_id=?", cfg.ID, unmentioned.MessageID).Scan(&ordinaryState); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow("SELECT state FROM runtime_message_states WHERE runtime_id=? AND message_id=?", cfg.ID, mentioned.MessageID).Scan(&mentionState); err != nil {
		t.Fatal(err)
	}
	if ordinaryState != "ignored" || mentionState != "pending" {
		t.Fatalf("mention gate ordinary=%s mentioned=%s", ordinaryState, mentionState)
	}
	var batch RuntimeBatch
	runtimeMutate(t, s, "group.batch", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now())
		return batch, err
	})
	if batch.Mode != "group_mention" || len(batch.Messages) != 1 || batch.Messages[0].ID != mentioned.MessageID || len(batch.Context) != 1 || !strings.Contains(batch.Context[0].Body, "Aurora") {
		t.Fatalf("group batch did not use DWS context: %+v", batch)
	}
	var tasks []RuntimeTask
	runtimeMutate(t, s, "group.batch.complete", func(tx *Tx) (any, error) {
		var err error
		tasks, err = tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "mention:" + mentioned.MessageID,
			Title: "回答项目代号", Instructions: "回答提问", MessageIDs: []string{mentioned.MessageID}}}})
		return tasks, err
	})
	var task RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, s, "group.task.claim", func(tx *Tx) (any, error) {
		var err error
		task, attempt, err = tx.ClaimRuntimeTask(ctx, cfg.ID, "group-attempt", "model", "preset", "commit", "")
		return task, err
	})
	runtimeMutate(t, s, "group.task.complete", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "项目代号是 Aurora。", Summary: "已回答"}, "")
	})
	var delivery OutboxView
	runtimeMutate(t, s, "group.delivery", func(tx *Tx) (any, error) {
		var err error
		delivery, err = tx.PrepareTaskDelivery(ctx, tasks[0].ID)
		return delivery, err
	})
	if delivery.Transport != "bot_group" || delivery.ConversationID != appRoute.ConversationID || delivery.ReplyTo != "question" {
		t.Fatalf("group delivery is not bound to trigger: %+v", delivery)
	}

	// A second monitored group must receive its own task's reply, not the
	// runtime's default group route.
	runtimeMutate(t, s, "group.runtime.pause", func(tx *Tx) (any, error) { return tx.SetRuntimeStatus(ctx, cfg.ID, "paused", "") })
	var secondRoute Route
	runtimeMutate(t, s, "group.route.add.second", func(tx *Tx) (any, error) {
		var err error
		secondRoute, err = tx.AddRoute(ctx, appChannel.ID, RouteInput{ConversationID: "cid:group-agent-2", ConversationType: "group",
			Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		return secondRoute, err
	})
	runtimeMutate(t, s, "group.route.second.history", func(tx *Tx) (any, error) {
		return tx.AddRoute(ctx, dwsChannel.ID, RouteInput{ConversationID: secondRoute.ConversationID, ConversationType: "group"})
	})
	runtimeMutate(t, s, "group.runtime.configure.second", func(tx *Tx) (any, error) {
		var err error
		cfg, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "group-helper", Channel: appChannel.ID, RouteIDs: []string{appRoute.ID, secondRoute.ID},
			DeliveryRouteID: appRoute.ID, Owner: owner, ApplicationMode: "group_mention", ContextChannel: dwsChannel.ID, ReconcileSeconds: 10, ExpectedVersion: cfg.Version})
		return cfg, err
	})
	runtimeMutate(t, s, "group.runtime.resume", func(tx *Tx) (any, error) { return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "") })
	cfg, _ = ReadRuntime(ctx, s.DB, cfg.ID)
	secondMentioned := IntakeResult{}
	runtimeMutate(t, s, "group.intake.second", func(tx *Tx) (any, error) {
		var err error
		secondMentioned, err = tx.Intake(ctx, appChannel.ID, NormalizedEvent{Kind: EventMessage, Adapter: "dingtalk_app", ParseVersion: "1", Origin: "stream",
			ProviderMessageID: "question-2", ConversationID: secondRoute.ConversationID, ConversationType: "group", Tenant: appChannel.Tenant,
			Sender: Sender{IDType: "staff_id", IDValue: "peer"}, Body: "@机器人 第二个群提问", Mentioned: true, SentAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)})
		return secondMentioned, err
	})
	runtimeMutate(t, s, "group.sync.second", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, cfg.ID) })
	var secondBatch RuntimeBatch
	runtimeMutate(t, s, "group.batch.second", func(tx *Tx) (any, error) {
		var err error
		secondBatch, err = tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now())
		return secondBatch, err
	})
	if secondBatch.RouteID != secondRoute.ID {
		t.Fatalf("second batch did not bind to the second group route: %+v", secondBatch)
	}
	var secondTasks []RuntimeTask
	runtimeMutate(t, s, "group.batch.complete.second", func(tx *Tx) (any, error) {
		var err error
		secondTasks, err = tx.CompleteRuntimeBatch(ctx, secondBatch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "mention:" + secondMentioned.MessageID,
			Title: "回答第二个群", Instructions: "回答提问", MessageIDs: []string{secondMentioned.MessageID}}}})
		return secondTasks, err
	})
	var secondTask RuntimeTask
	var secondAttempt RuntimeAttempt
	runtimeMutate(t, s, "group.task.claim.second", func(tx *Tx) (any, error) {
		var err error
		secondTask, secondAttempt, err = tx.ClaimRuntimeTask(ctx, cfg.ID, "group-attempt-2", "model", "preset", "commit", "")
		return secondTask, err
	})
	runtimeMutate(t, s, "group.task.complete.second", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(ctx, secondTask.ID, secondTask.Version, secondAttempt.ID, RuntimeAttemptResult{Result: "第二个群的回答。", Summary: "已回答"}, "")
	})
	var secondDelivery OutboxView
	runtimeMutate(t, s, "group.delivery.second", func(tx *Tx) (any, error) {
		var err error
		secondDelivery, err = tx.PrepareTaskDelivery(ctx, secondTasks[0].ID)
		return secondDelivery, err
	})
	if secondDelivery.Transport != "bot_group" || secondDelivery.ConversationID != secondRoute.ConversationID || secondDelivery.RouteID != secondRoute.ID || secondDelivery.ReplyTo != "question-2" {
		t.Fatalf("second group task did not deliver to its own trigger group: %+v", secondDelivery)
	}
	if secondDelivery.ConversationID == appRoute.ConversationID {
		t.Fatalf("second group task leaked delivery into the default group: %+v", secondDelivery)
	}
	// Activity discovery can withdraw the second group while its assistant
	// route remains active. Its old result must not be sent to that group (or
	// diverted into the owner-private confirmation route).
	runtimeMutate(t, s, "group.discovery.withdraw.second", func(tx *Tx) (any, error) {
		_, _, err := tx.SyncGroupMentionRoutes(ctx, cfg.ID, []string{appRoute.ConversationID})
		return nil, err
	})
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "group.delivery.withdrawn"}, func(tx *Tx) (any, error) {
		return tx.PrepareTaskDelivery(ctx, secondTasks[0].ID)
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("group Agent delivered a task after its active processing scope shrank: %v", err)
	}

	// Deactivating (or ignoring) a route must block delivery of any task still
	// bound to it, even though the task already completed.
	runtimeMutate(t, s, "group.route.second.disable", func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, secondRoute.ID, secondRoute.Version, RouteInput{Mode: "ignore"}, "group access revoked")
	})
	secondMentioned2 := IntakeResult{}
	runtimeMutate(t, s, "group.intake.second.stale", func(tx *Tx) (any, error) {
		var err error
		secondMentioned2, err = tx.Intake(ctx, appChannel.ID, NormalizedEvent{Kind: EventMessage, Adapter: "dingtalk_app", ParseVersion: "1", Origin: "stream",
			ProviderMessageID: "question-3", ConversationID: secondRoute.ConversationID, ConversationType: "group", Tenant: appChannel.Tenant,
			Sender: Sender{IDType: "staff_id", IDValue: "peer"}, Body: "普通消息", Mentioned: false, SentAt: time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339Nano)})
		return secondMentioned2, err
	})
	// Force a stale task record pointed at the now-ignored route to simulate a
	// task that completed before the route lost its permission.
	var staleTask RuntimeTask
	if _, err := s.DB.Exec("INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,kind,title,instructions,status,needs_clarification,version,result,result_summary,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"stale-task-1", cfg.ID, secondRoute.ID, "stale-key", "task", "旧任务", "旧指令", "completed", 0, 1, "旧结果", "旧摘要", Now(), Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO runtime_task_messages(task_id,message_id,revision) SELECT ?,id,current_revision FROM messages WHERE id=?", "stale-task-1", secondMentioned2.MessageID); err != nil {
		t.Fatal(err)
	}
	staleTask, err = ReadRuntimeTask(ctx, s.DB, "stale-task-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "group.delivery.stale-route"}, func(tx *Tx) (any, error) {
		return tx.PrepareTaskDelivery(ctx, staleTask.ID)
	})
	if ErrorCode(err) != "denied" && ErrorCode(err) != "conflict" {
		t.Fatalf("delivery to a withdrawn or ignored route was not blocked: %v", err)
	}
}

// A group_mention runtime that watches two groups must reply each task to the
// group that actually triggered it, not to the runtime's default delivery
// route. This guards against the multi-group routing regression fixed by #2.
func TestGroupMentionRuntimeWithTwoGroupsDeliversEachTaskToItsOwnTrigger(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	dwsChannel, dwsRouteA := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group-a")
	owner := Sender{IDType: "staff_id", IDValue: "owner-1"}
	var dwsRouteB Route
	runtimeMutate(t, s, "dws.route.b", func(tx *Tx) (any, error) {
		var err error
		dwsRouteB, err = tx.AddRoute(ctx, dwsChannel.ID, RouteInput{ConversationID: "cid:group-b", ConversationType: "group"})
		return dwsRouteB, err
	})

	var appChannel Channel
	var appRouteA, appRouteB, ownerRoute Route
	runtimeMutate(t, s, "group.app.add", func(tx *Tx) (any, error) {
		var err error
		appChannel, err = tx.AddChannel(ctx, ChannelInput{Name: "group-app", Kind: ChannelDingTalkApp,
			Identity: ChannelIdentity{ExpectedCorpID: dwsChannel.Tenant, ClientID: "group-app", RobotCode: "group-bot", HistoryChannel: dwsChannel.ID}})
		return appChannel, err
	})
	runtimeMutate(t, s, "group.route.add.a", func(tx *Tx) (any, error) {
		var err error
		appRouteA, err = tx.AddRoute(ctx, appChannel.ID, RouteInput{ConversationID: dwsRouteA.ConversationID, ConversationType: "group",
			Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		return appRouteA, err
	})
	runtimeMutate(t, s, "group.route.add.b", func(tx *Tx) (any, error) {
		var err error
		appRouteB, err = tx.AddRoute(ctx, appChannel.ID, RouteInput{ConversationID: dwsRouteB.ConversationID, ConversationType: "group",
			Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		return appRouteB, err
	})
	runtimeMutate(t, s, "group.owner.route", func(tx *Tx) (any, error) {
		var err error
		ownerRoute, err = tx.AddRoute(ctx, appChannel.ID, RouteInput{ConversationID: "cid:owner-app", ConversationType: "direct", Mode: "notify", SendPolicy: "dispatch_only"})
		return ownerRoute, err
	})
	intakeRuntimeMessage(t, s, appChannel, ownerRoute.ConversationID, "owner-bootstrap", owner, "bootstrap", time.Now().Add(-time.Hour))

	var cfg RuntimeConfig
	runtimeMutate(t, s, "group.runtime.configure", func(tx *Tx) (any, error) {
		var err error
		cfg, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "group-helper-multi", Channel: appChannel.ID, RouteIDs: []string{appRouteA.ID, appRouteB.ID},
			DeliveryRouteID: appRouteA.ID, Owner: owner, ApplicationMode: "group_mention", ContextChannel: dwsChannel.ID, ReconcileSeconds: 10})
		return cfg, err
	})
	runtimeMutate(t, s, "group.runtime.start", func(tx *Tx) (any, error) { return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "") })
	cfg, _ = ReadRuntime(ctx, s.DB, cfg.ID)

	intakeApp := func(id string, route Route, body string) IntakeResult {
		var out IntakeResult
		runtimeMutate(t, s, "group.intake."+id, func(tx *Tx) (any, error) {
			var err error
			out, err = tx.Intake(ctx, appChannel.ID, NormalizedEvent{Kind: EventMessage, Adapter: "dingtalk_app", ParseVersion: "1", Origin: "stream",
				ProviderMessageID: id, ConversationID: route.ConversationID, ConversationType: "group", Tenant: appChannel.Tenant,
				Sender: Sender{IDType: "staff_id", IDValue: "peer"}, Body: body, Mentioned: true, SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
			return out, err
		})
		return out
	}
	messageA := intakeApp("question-a", appRouteA, "@机器人 A 群的问题")
	messageB := intakeApp("question-b", appRouteB, "@机器人 B 群的问题")
	runtimeMutate(t, s, "group.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, cfg.ID) })

	claimAndCompleteWithActions := func(key string, msg IntakeResult, result string, actions []RuntimeAction) RuntimeTask {
		var batch RuntimeBatch
		runtimeMutate(t, s, "group.batch."+key, func(tx *Tx) (any, error) {
			var err error
			batch, err = tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now())
			return batch, err
		})
		if len(batch.Messages) != 1 || batch.Messages[0].ID != msg.MessageID {
			t.Fatalf("batch %s did not claim its own trigger message: %+v", key, batch)
		}
		var tasks []RuntimeTask
		runtimeMutate(t, s, "group.batch.complete."+key, func(tx *Tx) (any, error) {
			var err error
			tasks, err = tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: key,
				Title: "回答", Instructions: "回答提问", MessageIDs: []string{msg.MessageID}}}})
			return tasks, err
		})
		var task RuntimeTask
		var attempt RuntimeAttempt
		runtimeMutate(t, s, "group.task.claim."+key, func(tx *Tx) (any, error) {
			var err error
			task, attempt, err = tx.ClaimRuntimeTask(ctx, cfg.ID, key+"-attempt", "model", "preset", "commit", "")
			return task, err
		})
		var completed RuntimeTask
		runtimeMutate(t, s, "group.task.complete."+key, func(tx *Tx) (any, error) {
			var err error
			completed, err = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: result, Summary: "已回答", Actions: actions}, "")
			return completed, err
		})
		return completed
	}
	claimAndComplete := func(key string, msg IntakeResult, result string) RuntimeTask {
		return claimAndCompleteWithActions(key, msg, result, nil)
	}
	taskA := claimAndComplete("a", messageA, "A 群的答案")
	taskB := claimAndComplete("b", messageB, "B 群的答案")

	var deliveryA, deliveryB OutboxView
	runtimeMutate(t, s, "group.delivery.a", func(tx *Tx) (any, error) {
		var err error
		deliveryA, err = tx.PrepareTaskDelivery(ctx, taskA.ID)
		return deliveryA, err
	})
	runtimeMutate(t, s, "group.delivery.b", func(tx *Tx) (any, error) {
		var err error
		deliveryB, err = tx.PrepareTaskDelivery(ctx, taskB.ID)
		return deliveryB, err
	})
	if deliveryA.ConversationID != appRouteA.ConversationID || deliveryA.ReplyTo != "question-a" {
		t.Fatalf("task A leaked to the wrong group: %+v", deliveryA)
	}
	if deliveryB.ConversationID != appRouteB.ConversationID || deliveryB.ReplyTo != "question-b" {
		t.Fatalf("task B leaked to the wrong group (likely the default delivery route): %+v", deliveryB)
	}
	if deliveryA.ConversationID == deliveryB.ConversationID {
		t.Fatalf("both tasks were delivered to the same group: %+v %+v", deliveryA, deliveryB)
	}

	// A repeated delivery call for the same task must return the same outbox
	// row (idempotency), not create a second row or flip conversations.
	var repeatA OutboxView
	runtimeMutate(t, s, "group.delivery.a.repeat", func(tx *Tx) (any, error) {
		var err error
		repeatA, err = tx.PrepareTaskDelivery(ctx, taskA.ID)
		return repeatA, err
	})
	if repeatA.ID != deliveryA.ID || repeatA.ConversationID != deliveryA.ConversationID {
		t.Fatalf("repeated delivery was not idempotent: first=%+v repeat=%+v", deliveryA, repeatA)
	}
	var outboxCount int
	if err := s.DB.QueryRow("SELECT count(*) FROM outbox WHERE job_id=?", taskA.ID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("repeated delivery created %d outbox rows", outboxCount)
	}

	// Once the trigger route's reply permission is narrowed (e.g. taken out of
	// reply_to_trigger), any further delivery attempt for tasks still bound to
	// it must be denied, not silently redirected to the default group.
	runtimeMutate(t, s, "group.route.narrow.b", func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, appRouteB.ID, appRouteB.Version, RouteInput{SendPolicy: "draft_only"}, "narrow group B reply permission")
	})
	messageC := intakeApp("question-c", appRouteB, "@机器人 B 群的另一个问题")
	runtimeMutate(t, s, "group.sync.c", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, cfg.ID) })
	taskC := claimAndComplete("c", messageC, "另一个答案")
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "group.delivery.denied"}, func(tx *Tx) (any, error) {
		return tx.PrepareTaskDelivery(ctx, taskC.ID)
	})
	if ErrorCode(err) != "denied" {
		t.Fatalf("delivery to a route with narrowed permission was not denied: %v", err)
	}

	// The reply and action preview stay in the origin group. Approval still
	// requires the verified owner in that exact group.
	messageD := intakeApp("question-d", appRouteA, "@机器人 需要确认的操作")
	runtimeMutate(t, s, "group.sync.d", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, cfg.ID) })
	taskD := claimAndCompleteWithActions("d", messageD, "准备完成", []RuntimeAction{{Kind: "git_push", Target: "origin/main", Payload: "push commit abc"}})
	if taskD.Status != "awaiting_confirmation" {
		t.Fatalf("task did not require confirmation: %+v", taskD)
	}
	var groupAnswer OutboxView
	runtimeMutate(t, s, "group.delivery.pending-answer", func(tx *Tx) (any, error) {
		var err error
		groupAnswer, err = tx.PrepareTaskDelivery(ctx, taskD.ID)
		return groupAnswer, err
	})
	if groupAnswer.Transport != "bot_group" || groupAnswer.ConversationID != appRouteA.ConversationID || !strings.Contains(groupAnswer.Content, "准备完成") || !strings.Contains(groupAnswer.Content, ConfirmationToken(taskD.Actions[0])) || !strings.Contains(groupAnswer.Content, taskD.Actions[0].Payload) {
		t.Fatalf("pending group answer missing or leaked confirmation details: %+v", groupAnswer)
	}
	action := taskD.Actions[0]
	token := ConfirmationToken(action)
	wrong := intakeRuntimeMessage(t, s, appChannel, appRouteB.ConversationID, "wrong-group-confirmation", owner, token, time.Now().Add(time.Hour))
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "group.confirm.wrong-route"}, func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeActionFromMessage(ctx, action.ID, wrong.MessageID)
	})
	if ErrorCode(err) != "denied" {
		t.Fatalf("confirmation from another group was accepted: %v", err)
	}
	private := intakeRuntimeMessage(t, s, appChannel, ownerRoute.ConversationID, "owner-action-confirmation", owner, token, time.Now().Add(time.Hour))
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "group.confirm.reject-private"}, func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeActionFromMessage(ctx, action.ID, private.MessageID)
	})
	if ErrorCode(err) != "denied" {
		t.Fatalf("group action accepted private confirmation: %v", err)
	}
	foreign := intakeRuntimeMessage(t, s, appChannel, appRouteA.ConversationID, "foreign-action-confirmation", Sender{IDType: "staff_id", IDValue: "not-owner"}, token, time.Now().Add(time.Hour))
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "group.confirm.reject-foreign"}, func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeActionFromMessage(ctx, action.ID, foreign.MessageID)
	})
	if ErrorCode(err) != "denied" {
		t.Fatalf("foreign group member confirmed action: %v", err)
	}
	groupConfirmation := intakeRuntimeMessage(t, s, appChannel, appRouteA.ConversationID, "group-owner-confirmation", owner, token, time.Now().Add(time.Hour))
	var confirmed RuntimePendingAction
	runtimeMutate(t, s, "group.confirm.owner", func(tx *Tx) (any, error) {
		var e error
		confirmed, e = tx.ConfirmRuntimeActionFromMessage(ctx, action.ID, groupConfirmation.MessageID)
		return confirmed, e
	})
	if confirmed.Status != "confirmed" || confirmed.ConfirmationOrigin != "dingtalk_message:"+groupConfirmation.MessageID {
		t.Fatalf("owner origin-group confirmation failed: %+v", confirmed)
	}
	messageE := intakeApp("question-e", appRouteA, "@机器人 第二个确认操作")
	runtimeMutate(t, s, "group.sync.e", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, cfg.ID) })
	taskE := claimAndCompleteWithActions("e", messageE, "准备第二个操作", []RuntimeAction{{Kind: "git_push", Target: "origin/feature", Payload: "push second commit"}})
	intakeRuntimeMessage(t, s, appChannel, appRouteB.ConversationID, "owner-wrong-group-auto", owner, ConfirmationToken(taskE.Actions[0]), time.Now().Add(time.Hour))
	var groupControl IntakeResult
	runtimeMutate(t, s, "group.confirm.at-message", func(tx *Tx) (any, error) {
		var e error
		groupControl, e = tx.Intake(ctx, appChannel.ID, NormalizedEvent{Kind: EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "owner-auto-confirmation", ConversationID: appRouteA.ConversationID, ConversationType: "group", Tenant: appChannel.Tenant, Sender: owner, Mentioned: true, Body: "@机器人 " + ConfirmationToken(taskE.Actions[0]), SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
		return groupControl, e
	})
	var scanned []RuntimePendingAction
	runtimeMutate(t, s, "group.confirm.scan", func(tx *Tx) (any, error) {
		var e error
		scanned, e = tx.ProcessRuntimeConfirmations(ctx, cfg.ID)
		return scanned, e
	})
	if len(scanned) != 1 || scanned[0].ID != taskE.Actions[0].ID || scanned[0].Status != "confirmed" {
		t.Fatalf("owner confirmation scan missed group task: %+v", scanned)
	}
	runtimeMutate(t, s, "group.confirm.sync-controls", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, cfg.ID) })
	var controlState string
	if err = s.DB.QueryRow("SELECT state FROM runtime_message_states WHERE runtime_id=? AND message_id=?", cfg.ID, groupControl.MessageID).Scan(&controlState); err != nil || controlState != "processed" {
		t.Fatalf("approval became another Agent request: state=%s err=%v", controlState, err)
	}
	messageF := intakeApp("question-f", appRouteA, "@机器人 确认边界检查")
	runtimeMutate(t, s, "group.sync.f", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, cfg.ID) })
	taskF := claimAndCompleteWithActions("f", messageF, "准备检查", []RuntimeAction{{Kind: "git_push", Target: "origin/check", Payload: "push boundary check"}})
	action = taskF.Actions[0]
	token = ConfirmationToken(action)
	for _, invalid := range []struct {
		name      string
		body      string
		mentioned bool
	}{
		{"untrusted-at", "@机器人 " + token, false},
		{"negated", "@机器人 不要确认 " + token, true},
		{"extra-text", "@机器人 " + token + " 备注", true},
		{"quoted", "@机器人 \"" + token + "\"", true},
	} {
		var control IntakeResult
		runtimeMutate(t, s, "group.confirm.invalid-intake."+invalid.name, func(tx *Tx) (any, error) {
			var e error
			control, e = tx.Intake(ctx, appChannel.ID, NormalizedEvent{Kind: EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "invalid-confirmation-" + invalid.name, ConversationID: appRouteA.ConversationID, ConversationType: "group", Tenant: appChannel.Tenant, Sender: owner, Mentioned: invalid.mentioned, Body: invalid.body, SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
			return control, e
		})
		_, e := s.Mutate(ctx, Request{Scope: "global", Command: "group.confirm.invalid." + invalid.name}, func(tx *Tx) (any, error) {
			return tx.ConfirmRuntimeActionFromMessage(ctx, action.ID, control.MessageID)
		})
		if ErrorCode(e) != "denied" {
			t.Fatalf("%s confirmation was accepted: %v", invalid.name, e)
		}
	}
}

func TestRuntimeBatchesByCountOrOldestWaitAndKeepsBootstrapAsContext(t *testing.T) {
	f := newRuntimeFixture(t, 2, 300)
	old := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "old", Sender{IDType: "union_id", IDValue: "alice"}, "old context", time.Now().Add(-time.Hour))
	runtimeMutate(t, f.s, "runtime.sync.old", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(context.Background(), f.config.ID) })
	var state string
	if err := f.s.DB.QueryRow("SELECT state FROM runtime_message_states WHERE runtime_id=? AND message_id=?", f.config.ID, old.MessageID).Scan(&state); err != nil || state != "context" {
		t.Fatalf("bootstrap message state = %q, %v", state, err)
	}
	first := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "new-1", Sender{IDType: "union_id", IDValue: "alice"}, "please inspect it", time.Now().Add(time.Hour))
	runtimeMutate(t, f.s, "runtime.sync.one", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(context.Background(), f.config.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "runtime.claim.early", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(context.Background(), f.config.ID, time.Now())
		return batch, err
	})
	if batch.ID != "" {
		t.Fatal("one fresh message triggered before the wait elapsed")
	}
	runtimeMutate(t, f.s, "runtime.claim.wait", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(context.Background(), f.config.ID, time.Now().Add(6*time.Minute))
		return batch, err
	})
	if len(batch.Messages) != 1 || batch.Messages[0].ID != first.MessageID || len(batch.Context) == 0 {
		t.Fatalf("time-triggered batch: %+v", batch)
	}
	runtimeMutate(t, f.s, "runtime.complete.context", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeBatch(context.Background(), batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "context"}}})
	})
	intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "new-2", Sender{IDType: "union_id", IDValue: "alice"}, "first", time.Now().Add(2*time.Hour))
	intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "new-3", Sender{IDType: "union_id", IDValue: "alice"}, "second", time.Now().Add(2*time.Hour+time.Second))
	runtimeMutate(t, f.s, "runtime.sync.two", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(context.Background(), f.config.ID) })
	runtimeMutate(t, f.s, "runtime.claim.count", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(context.Background(), f.config.ID, time.Now())
		return batch, err
	})
	if len(batch.Messages) != 2 {
		t.Fatalf("count trigger returned %d messages", len(batch.Messages))
	}
}

func TestOwnerDirectMessageTriggersImmediately(t *testing.T) {
	f := newOwnerInteractiveFixture(t)
	message := intakeRuntimeMessage(t, f.s, f.channel, f.direct.ConversationID, "direct-question", f.owner, "请帮我检查测试", time.Now().Add(time.Hour))
	runtimeMutate(t, f.s, "runtime.sync.direct", func(tx *Tx) (any, error) {
		return tx.SyncRuntimeMessages(context.Background(), f.config.ID)
	})
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "runtime.claim.direct", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(context.Background(), f.config.ID, time.Now())
		return batch, err
	})
	if batch.RouteID != f.direct.ID || len(batch.Messages) != 1 || batch.Messages[0].ID != message.MessageID {
		t.Fatalf("owner direct message was not claimed immediately: %+v", batch)
	}
	if batch.Messages[0].ProviderMessageID != "direct-question" || batch.Messages[0].ConversationID != f.direct.ConversationID {
		t.Fatalf("provider reply address missing: %+v", batch.Messages[0])
	}
}

func TestRuntimeStopsPendingWorkWhenRouteBecomesIgnored(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	message := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "ignore-pending", Sender{IDType: "union_id", IDValue: "alice"}, "please process", time.Now().Add(time.Hour))
	runtimeMutate(t, f.s, "runtime.sync.before-ignore", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	runtimeMutate(t, f.s, "runtime.route.ignore", func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, f.watch.ID, f.watch.Version, RouteInput{Mode: "ignore"}, "ignore this group")
	})
	runtimeMutate(t, f.s, "runtime.sync.after-ignore", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var state string
	if err := f.s.DB.QueryRowContext(ctx, "SELECT state FROM runtime_message_states WHERE runtime_id=? AND message_id=?", f.config.ID, message.MessageID).Scan(&state); err != nil || state != "ignored" {
		t.Fatalf("pending work was not ignored: state=%q err=%v", state, err)
	}
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "runtime.claim.after-ignore", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now().Add(time.Hour))
		return batch, err
	})
	if batch.ID != "" {
		t.Fatalf("ignored route still produced a batch: %+v", batch)
	}
}

func TestRuntimeGroupSyncAddsNewGroupsAndPreservesIgnore(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	var ignored Route
	runtimeMutate(t, f.s, "runtime.route.add-ignore", func(tx *Tx) (any, error) {
		var err error
		ignored, err = tx.AddRoute(ctx, f.channel.ID, RouteInput{ConversationID: "cid:ignored", ConversationType: "group", Mode: "ignore"})
		return ignored, err
	})
	var updated RuntimeConfig
	var added int
	runtimeMutate(t, f.s, "runtime.groups.sync", func(tx *Tx) (any, error) {
		var err error
		updated, added, err = tx.SyncRuntimeGroupRoutes(ctx, f.config.ID, []string{f.watch.ConversationID, ignored.ConversationID, "cid:new"})
		return updated, err
	})
	if added != 1 || len(updated.RouteIDs) != 2 {
		t.Fatalf("unexpected group sync: added=%d config=%+v", added, updated)
	}
	for _, routeID := range updated.RouteIDs {
		if routeID == ignored.ID {
			t.Fatal("ignored route was restored by automatic group sync")
		}
	}
	newRoute, err := RouteFor(ctx, f.s.DB, f.channel.ID, "cid:new")
	if err != nil || newRoute.Mode != "collect" {
		t.Fatalf("new group route: %+v err=%v", newRoute, err)
	}
	runtimeMutate(t, f.s, "runtime.groups.sync.active-subset", func(tx *Tx) (any, error) {
		var syncErr error
		updated, _, syncErr = tx.SyncRuntimeGroupRoutes(ctx, f.config.ID, []string{"cid:new"})
		return updated, syncErr
	})
	if len(updated.RouteIDs) != 1 || updated.RouteIDs[0] != newRoute.ID {
		t.Fatalf("inactive groups remained in processing set: %+v", updated.RouteIDs)
	}
}

func TestRuntimeFirstStartMakesPostConfigureHistoryContext(t *testing.T) {
	f := newRuntimeFixture(t, 2, 300)
	ctx := context.Background()
	var cfg RuntimeConfig
	runtimeMutate(t, f.s, "runtime.configure.before-start", func(tx *Tx) (any, error) {
		var err error
		cfg, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "watcher-before-start", Channel: f.channel.ID, RouteIDs: []string{f.watch.ID}, DeliveryRouteID: f.direct.ID, Owner: f.owner})
		return cfg, err
	})
	message := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "arrived-before-start", Sender{IDType: "union_id", IDValue: "alice"}, "historical context", time.Now())
	runtimeMutate(t, f.s, "runtime.first-start", func(tx *Tx) (any, error) {
		return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "")
	})
	runtimeMutate(t, f.s, "runtime.sync.first-start", func(tx *Tx) (any, error) {
		return tx.SyncRuntimeMessages(ctx, cfg.ID)
	})
	var state string
	if err := f.s.DB.QueryRowContext(ctx, "SELECT state FROM runtime_message_states WHERE runtime_id=? AND message_id=?", cfg.ID, message.MessageID).Scan(&state); err != nil || state != "context" {
		t.Fatalf("message present before first start became %q, %v", state, err)
	}
}

func TestRuntimeRejectsAnalysisWhenABatchMessageChanges(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "batch-edit", Sender{IDType: "union_id", IDValue: "alice"}, "original request", time.Now().Add(time.Hour))
	runtimeMutate(t, f.s, "runtime.sync.batch-edit", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "runtime.claim.batch-edit", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now())
		return batch, err
	})
	runtimeMutate(t, f.s, "runtime.intake.batch-edit", func(tx *Tx) (any, error) {
		return tx.Intake(ctx, f.channel.ID, NormalizedEvent{Kind: EventEdit, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "batch-edit", ConversationID: f.watch.ConversationID, Tenant: f.channel.Tenant, Sender: Sender{IDType: "union_id", IDValue: "alice"}, Body: "changed request", EditedAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano), EventAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "runtime.complete.changed-batch"}, func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "changed", Title: "Old", Instructions: "Use old request"}}})
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("changed batch message accepted old analysis: %v", err)
	}
}

func createRuntimeTask(t *testing.T, f runtimeFixture, key string) RuntimeTask {
	t.Helper()
	sender := Sender{IDType: "union_id", IDValue: "alice"}
	if f.config.ApplicationMode == "direct" {
		sender = f.owner
	}
	msg := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "task-"+key, sender, "please do "+key, time.Now().Add(time.Hour))
	runtimeMutate(t, f.s, "runtime.sync.task", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(context.Background(), f.config.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "runtime.claim.task", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(context.Background(), f.config.ID, time.Now().Add(10*time.Minute))
		return batch, err
	})
	var tasks []RuntimeTask
	runtimeMutate(t, f.s, "runtime.complete.task", func(tx *Tx) (any, error) {
		var err error
		tasks, err = tx.CompleteRuntimeBatch(context.Background(), batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: key, Title: "Do " + key, Instructions: "Complete and verify " + key, MessageIDs: []string{msg.MessageID}}}})
		return tasks, err
	})
	return tasks[0]
}

func TestRuntimeTaskVersionStopsOldResultsAndRecallHidesBody(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	task := createRuntimeTask(t, f, "versioned")
	var claimed RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "runtime.claim.execution", func(tx *Tx) (any, error) {
		var err error
		claimed, attempt, err = tx.ClaimRuntimeTask(context.Background(), f.config.ID, "attempt-old", "model", "preset", "commit", "")
		return claimed, err
	})
	if claimed.ID != task.ID {
		t.Fatalf("claimed task %q", claimed.ID)
	}
	// An edit creates a new revision and stales the running attempt.
	runtimeMutate(t, f.s, "runtime.edit", func(tx *Tx) (any, error) {
		return tx.Intake(context.Background(), f.channel.ID, NormalizedEvent{Kind: EventEdit, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "task-versioned", ConversationID: f.watch.ConversationID, Tenant: f.channel.Tenant, Sender: Sender{IDType: "union_id", IDValue: "alice"}, Body: "please do the revised version", EditedAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano), EventAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	_, completionErr := f.s.Mutate(context.Background(), Request{Scope: "global", Command: "runtime.complete.before-sync"}, func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(context.Background(), task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "obsolete"}, "")
	})
	if ErrorCode(completionErr) != "conflict" {
		t.Fatalf("old result was accepted before message-state sync: %v", completionErr)
	}
	runtimeMutate(t, f.s, "runtime.sync.edit", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(context.Background(), f.config.ID) })
	_, completionErr = f.s.Mutate(context.Background(), Request{Scope: "global", Command: "runtime.complete.old"}, func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(context.Background(), task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "obsolete"}, "")
	})
	if ErrorCode(completionErr) != "conflict" {
		t.Fatalf("old result accepted: %v", completionErr)
	}
	// Recalling the trigger removes its body from task reads while keeping the
	// audit link readable.
	runtimeMutate(t, f.s, "runtime.recall", func(tx *Tx) (any, error) {
		return tx.Intake(context.Background(), f.channel.ID, NormalizedEvent{Kind: EventRecall, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "task-versioned", ConversationID: f.watch.ConversationID, Tenant: f.channel.Tenant, RecalledAt: time.Now().UTC().Format(time.RFC3339Nano)})
	})
	runtimeMutate(t, f.s, "runtime.sync.recall", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(context.Background(), f.config.ID) })
	after, err := ReadRuntimeTask(context.Background(), f.s.DB, task.ID)
	if err != nil || len(after.Messages) != 1 || after.Messages[0].Body != "" || after.Status != "cancelled" {
		t.Fatalf("recalled task: %+v, %v", after, err)
	}
}

func TestRuntimeBlocksDeliveryWhenSourceChangedBeforeSync(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	task := createRuntimeTask(t, f, "delivery-stale")
	var claimed RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "runtime.claim.delivery-stale", func(tx *Tx) (any, error) {
		var err error
		claimed, attempt, err = tx.ClaimRuntimeTask(context.Background(), f.config.ID, "delivery-attempt", "model", "preset", "commit", "")
		return claimed, err
	})
	runtimeMutate(t, f.s, "runtime.complete.delivery-stale", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(context.Background(), claimed.ID, claimed.Version, attempt.ID, RuntimeAttemptResult{Result: "old result", Summary: "old"}, "")
	})
	runtimeMutate(t, f.s, "runtime.edit.before-delivery", func(tx *Tx) (any, error) {
		return tx.Intake(context.Background(), f.channel.ID, NormalizedEvent{Kind: EventEdit, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "task-delivery-stale", ConversationID: f.watch.ConversationID, Tenant: f.channel.Tenant, Sender: Sender{IDType: "union_id", IDValue: "alice"}, Body: "changed before delivery", EditedAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano), EventAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	_, err := f.s.Mutate(context.Background(), Request{Scope: "global", Command: "runtime.delivery.stale"}, func(tx *Tx) (any, error) {
		return tx.PrepareTaskDelivery(context.Background(), task.ID)
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("stale result was prepared for delivery: %v", err)
	}
}

func TestRuntimeDeliveryQuotesQuestionAndShowsModelAndThinkingTime(t *testing.T) {
	f := newOwnerInteractiveFixture(t)
	task := createRuntimeTask(t, f, "styled-delivery")
	var claimed RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "runtime.claim.styled", func(tx *Tx) (any, error) {
		var err error
		claimed, attempt, err = tx.ClaimRuntimeTask(context.Background(), f.config.ID, "styled-attempt", "profile", "preset", "commit", "")
		return claimed, err
	})
	runtimeMutate(t, f.s, "runtime.complete.styled", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(context.Background(), claimed.ID, claimed.Version, attempt.ID,
			RuntimeAttemptResult{Result: "测试已经通过。", Summary: "通过", Usage: map[string]any{"model": "claude-sonnet-test"}}, "")
	})
	if _, err := f.s.DB.Exec("UPDATE runtime_attempts SET started_at=?,finished_at=? WHERE id=?", "2026-09-15T01:00:00Z", "2026-09-15T01:01:06Z", attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec(`UPDATE runtime_batches SET started_at=?,finished_at=? WHERE id IN (
SELECT rbm.batch_id FROM runtime_batch_messages rbm JOIN runtime_task_messages rtm ON rtm.message_id=rbm.message_id WHERE rtm.task_id=?)`, "2026-09-15T00:59:50Z", "2026-09-15T01:00:00Z", task.ID); err != nil {
		t.Fatal(err)
	}
	var delivery OutboxView
	runtimeMutate(t, f.s, "runtime.delivery.styled", func(tx *Tx) (any, error) {
		var err error
		delivery, err = tx.PrepareTaskDelivery(context.Background(), task.ID)
		return delivery, err
	})
	for _, want := range []string{"> **你问：** please do styled-delivery", "测试已经通过。", "⏱ 执行 1m6s", "🤖 claude-sonnet-test"} {
		if !strings.Contains(delivery.Content, want) {
			t.Fatalf("styled delivery missing %q: %s", want, delivery.Content)
		}
	}
}

func TestRuntimeConfirmationRequiresExactOwnerDirectMessageAndExecutesOnce(t *testing.T) {
	f := newOwnerInteractiveFixture(t)
	task := createRuntimeTask(t, f, "external")
	var claimed RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "runtime.claim.external", func(tx *Tx) (any, error) {
		var err error
		claimed, attempt, err = tx.ClaimRuntimeTask(context.Background(), f.config.ID, "attempt-external", "model", "preset", "commit", "/tmp/work")
		return claimed, err
	})
	var completed RuntimeTask
	runtimeMutate(t, f.s, "runtime.complete.external", func(tx *Tx) (any, error) {
		var err error
		completed, err = tx.CompleteRuntimeTask(context.Background(), claimed.ID, claimed.Version, attempt.ID, RuntimeAttemptResult{Result: "准备完成", Summary: "等待确认", Actions: []RuntimeAction{{Kind: "git_push", Target: "origin/main", Payload: "push commit abc"}}}, "")
		return completed, err
	})
	if completed.Status != "awaiting_confirmation" || len(completed.Actions) != 1 {
		t.Fatalf("pending action missing: %+v", completed)
	}
	action := completed.Actions[0]
	var delivery OutboxView
	runtimeMutate(t, f.s, "runtime.delivery.preview", func(tx *Tx) (any, error) {
		var err error
		delivery, err = tx.PrepareTaskDelivery(context.Background(), task.ID)
		return delivery, err
	})
	if !strings.Contains(delivery.Content, ConfirmationToken(action)) || !strings.Contains(delivery.Content, action.Target) {
		t.Fatalf("confirmation was not concrete: %s", delivery.Content)
	}
	wrongSender := intakeRuntimeMessage(t, f.s, f.channel, f.direct.ConversationID, "confirm-wrong", Sender{IDType: "union_id", IDValue: "mallory"}, ConfirmationToken(action), time.Now().Add(2*time.Hour))
	_, err := f.s.Mutate(context.Background(), Request{Scope: "global", Command: "runtime.confirm.wrong"}, func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeActionFromMessage(context.Background(), action.ID, wrongSender.MessageID)
	})
	if ErrorCode(err) != "denied" {
		t.Fatalf("another member confirmed the action: %v", err)
	}
	runtimeMutate(t, f.s, "confirmation.other-route", func(tx *Tx) (any, error) {
		return tx.AddRoute(context.Background(), f.channel.ID, RouteInput{ConversationID: "another-conversation", ConversationType: "group", Mode: "collect"})
	})
	wrongRoute := intakeRuntimeMessage(t, f.s, f.channel, "another-conversation", "confirm-group", f.owner, ConfirmationToken(action), time.Now().Add(2*time.Hour))
	_, err = f.s.Mutate(context.Background(), Request{Scope: "global", Command: "runtime.confirm.group"}, func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeActionFromMessage(context.Background(), action.ID, wrongRoute.MessageID)
	})
	if ErrorCode(err) != "denied" {
		t.Fatalf("a group message confirmed the action: %v", err)
	}
	ownerMessage := intakeRuntimeMessage(t, f.s, f.channel, f.direct.ConversationID, "confirm-owner", f.owner, ConfirmationToken(action), time.Now().Add(2*time.Hour))
	runtimeMutate(t, f.s, "runtime.confirm.owner", func(tx *Tx) (any, error) {
		return tx.ConfirmRuntimeActionFromMessage(context.Background(), action.ID, ownerMessage.MessageID)
	})
	var claimedAction RuntimePendingAction
	var actionAttempt RuntimeActionAttempt
	runtimeMutate(t, f.s, "runtime.action.claim", func(tx *Tx) (any, error) {
		var err error
		claimedAction, actionAttempt, err = tx.ClaimRuntimeAction(context.Background(), f.config.ID, "action-attempt", "model")
		return claimedAction, err
	})
	if claimedAction.ID != action.ID {
		t.Fatalf("confirmed action was not claimed: %+v", claimedAction)
	}
	runtimeMutate(t, f.s, "runtime.action.complete", func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeAction(context.Background(), action.ID, actionAttempt.ID, action.TaskVersion, RuntimeAttemptResult{Result: "push accepted", Summary: "已推送", ToolKinds: []string{"git"}})
	})
	after, err := ReadRuntimeTask(context.Background(), f.s.DB, task.ID)
	if err != nil || after.Status != "completed" || after.Actions[0].Status != "executed" || len(after.Actions[0].Attempts) != 1 {
		t.Fatalf("completed action: %+v, %v", after, err)
	}
	var second RuntimePendingAction
	runtimeMutate(t, f.s, "runtime.action.claim.again", func(tx *Tx) (any, error) {
		var claimErr error
		second, _, claimErr = tx.ClaimRuntimeAction(context.Background(), f.config.ID, "again", "model")
		return second, claimErr
	})
	if second.ID != "" {
		t.Fatalf("executed action was claimed twice: %+v", second)
	}
}

func TestRuntimeRecoveryRetriesAnalysisButLeavesExternalOutcomesUnknown(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	createRuntimeTask(t, f, "recover")
	var task RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "runtime.claim.recover", func(tx *Tx) (any, error) {
		var err error
		task, attempt, err = tx.ClaimRuntimeTask(context.Background(), f.config.ID, "recover-attempt", "model", "preset", "commit", "")
		return task, err
	})
	if attempt.ID == "" {
		t.Fatal("task was not running before recovery")
	}
	var recovery RuntimeRecovery
	runtimeMutate(t, f.s, "runtime.recover", func(tx *Tx) (any, error) {
		var err error
		recovery, err = tx.RecoverRuntime(context.Background(), f.config.ID)
		return recovery, err
	})
	if recovery.Tasks != 1 || len(recovery.FailedTaskIDs) != 1 || recovery.FailedTaskIDs[0] != task.ID {
		t.Fatalf("recovery: %+v", recovery)
	}
	after, err := ReadRuntimeTask(context.Background(), f.s.DB, task.ID)
	if err != nil || after.Status != "failed" || after.Attempts[0].Status != "failed" {
		t.Fatalf("recovered task: %+v, %v", after, err)
	}
}

func TestRuntimeEnforcesOneExecutionAcrossCompetingClaimers(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	createRuntimeTask(t, f, "first")
	createRuntimeTask(t, f, "second")
	var first RuntimeTask
	runtimeMutate(t, f.s, "runtime.worker.one", func(tx *Tx) (any, error) {
		var err error
		first, _, err = tx.ClaimRuntimeTask(context.Background(), f.config.ID, "worker-one", "model", "preset", "commit", "")
		return first, err
	})
	if first.ID == "" {
		t.Fatal("first worker did not claim a task")
	}
	var second RuntimeTask
	runtimeMutate(t, f.s, "runtime.worker.two", func(tx *Tx) (any, error) {
		var err error
		second, _, err = tx.ClaimRuntimeTask(context.Background(), f.config.ID, "worker-two", "model", "preset", "commit", "")
		return second, err
	})
	if second.ID != "" {
		t.Fatalf("second worker exceeded concurrency one: %+v", second)
	}
}

// groupMentionSyncFixture prepares a group_mention runtime backed by a DWS
// context channel with two bound groups (one already an app assistant route,
// one still DWS-only) plus a DWS route explicitly marked ignore. It mirrors
// the shape SyncGroupMentionRoutes expects: the DWS channel is read-only
// discovery input, and the application channel carries the actual routes
// that end up in route_ids.
type groupMentionSyncFixture struct {
	s          *Store
	dwsChannel Channel
	appChannel Channel
	dwsA       Route
	dwsB       Route
	dwsIgnored Route
	appA       Route
	ownerRoute Route
	config     RuntimeConfig
	owner      Sender
}

func newGroupMentionSyncFixture(t *testing.T) groupMentionSyncFixture {
	t.Helper()
	ctx := context.Background()
	s := testStore(t)
	dwsChannel, dwsA := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group-a")
	var dwsB, dwsIgnored Route
	runtimeMutate(t, s, "dws.route.b", func(tx *Tx) (any, error) {
		var err error
		dwsB, err = tx.AddRoute(ctx, dwsChannel.ID, RouteInput{ConversationID: "cid:group-b", ConversationType: "group"})
		return dwsB, err
	})
	runtimeMutate(t, s, "dws.route.ignored", func(tx *Tx) (any, error) {
		var err error
		dwsIgnored, err = tx.AddRoute(ctx, dwsChannel.ID, RouteInput{ConversationID: "cid:group-ignored", ConversationType: "group", Mode: "ignore"})
		return dwsIgnored, err
	})

	owner := Sender{IDType: "staff_id", IDValue: "owner-1"}
	var appChannel Channel
	var appA, ownerRoute Route
	runtimeMutate(t, s, "group.app.add", func(tx *Tx) (any, error) {
		var err error
		appChannel, err = tx.AddChannel(ctx, ChannelInput{Name: "group-app", Kind: ChannelDingTalkApp,
			Identity: ChannelIdentity{ExpectedCorpID: dwsChannel.Tenant, ClientID: "group-app", RobotCode: "group-bot", HistoryChannel: dwsChannel.ID}})
		return appChannel, err
	})
	runtimeMutate(t, s, "group.route.add.a", func(tx *Tx) (any, error) {
		var err error
		appA, err = tx.AddRoute(ctx, appChannel.ID, RouteInput{ConversationID: dwsA.ConversationID, ConversationType: "group",
			Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		return appA, err
	})
	runtimeMutate(t, s, "group.owner.route", func(tx *Tx) (any, error) {
		var err error
		ownerRoute, err = tx.AddRoute(ctx, appChannel.ID, RouteInput{ConversationID: "cid:owner-app", ConversationType: "direct", Mode: "notify", SendPolicy: "dispatch_only"})
		return ownerRoute, err
	})
	intakeRuntimeMessage(t, s, appChannel, ownerRoute.ConversationID, "owner-bootstrap", owner, "bootstrap", time.Now().Add(-time.Hour))

	var cfg RuntimeConfig
	runtimeMutate(t, s, "group.runtime.configure", func(tx *Tx) (any, error) {
		var err error
		cfg, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "group-helper-sync", Channel: appChannel.ID, RouteIDs: []string{appA.ID},
			DeliveryRouteID: appA.ID, Owner: owner, ApplicationMode: "group_mention", ContextChannel: dwsChannel.ID, ReconcileSeconds: 10})
		return cfg, err
	})
	runtimeMutate(t, s, "group.runtime.start", func(tx *Tx) (any, error) { return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "") })
	cfg, err := ReadRuntime(ctx, s.DB, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	return groupMentionSyncFixture{s: s, dwsChannel: dwsChannel, appChannel: appChannel, dwsA: dwsA, dwsB: dwsB,
		dwsIgnored: dwsIgnored, appA: appA, ownerRoute: ownerRoute, config: cfg, owner: owner}
}

// A group_mention runtime must pick up a newly active DWS group on its very
// first sync (bootstrap), create a matching assistant route with the fixed
// policy, and fold it into route_ids without disturbing the existing group.
func TestGroupMentionRuntimeFirstSyncCreatesAssistantRouteForNewGroup(t *testing.T) {
	f := newGroupMentionSyncFixture(t)
	ctx := context.Background()
	var updated RuntimeConfig
	var added int
	runtimeMutate(t, f.s, "group.mention.sync.first", func(tx *Tx) (any, error) {
		var err error
		updated, added, err = tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID, f.dwsB.ConversationID})
		return updated, err
	})
	if added != 1 || len(updated.RouteIDs) != 2 {
		t.Fatalf("first sync did not adopt the new active group: added=%d config=%+v", added, updated)
	}
	newAppRoute, err := RouteFor(ctx, f.s.DB, f.appChannel.ID, f.dwsB.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if newAppRoute.Mode != "assistant" || newAppRoute.SendPolicy != "reply_to_trigger" || newAppRoute.AudiencePolicy != "conversation" ||
		newAppRoute.MemoryPolicy != "explicit_only" || !contains(newAppRoute.Triggers, "mention") {
		t.Fatalf("new group route did not use the fixed assistant policy: %+v", newAppRoute)
	}
	if newAppRoute.WorkspaceID != f.dwsB.WorkspaceID {
		t.Fatalf("new group route used the wrong workspace: %+v want %s", newAppRoute, f.dwsB.WorkspaceID)
	}
	if !contains(updated.RouteIDs, f.appA.ID) || !contains(updated.RouteIDs, newAppRoute.ID) {
		t.Fatalf("route_ids did not contain both the original and newly adopted route: %+v", updated.RouteIDs)
	}
}

// A group that is active on a later reconcile cycle (not the first sync) must
// also be picked up, without re-creating the route for a group already
// adopted on an earlier cycle.
func TestGroupMentionRuntimePeriodicSyncAddsNewlyActiveGroup(t *testing.T) {
	f := newGroupMentionSyncFixture(t)
	ctx := context.Background()
	runtimeMutate(t, f.s, "group.mention.sync.bootstrap", func(tx *Tx) (any, error) {
		cfg, _, err := tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID})
		return cfg, err
	})
	var afterC RuntimeConfig
	var addedC int
	var dwsC Route
	runtimeMutate(t, f.s, "dws.route.c", func(tx *Tx) (any, error) {
		var err error
		dwsC, err = tx.AddRoute(ctx, f.dwsChannel.ID, RouteInput{ConversationID: "cid:group-c", ConversationType: "group"})
		return dwsC, err
	})
	runtimeMutate(t, f.s, "group.mention.sync.periodic", func(tx *Tx) (any, error) {
		var err error
		afterC, addedC, err = tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID, dwsC.ConversationID})
		return afterC, err
	})
	if addedC != 1 || len(afterC.RouteIDs) != 2 {
		t.Fatalf("periodic sync did not add the newly active group: added=%d config=%+v", addedC, afterC)
	}
	newAppRoute, err := RouteFor(ctx, f.s.DB, f.appChannel.ID, dwsC.ConversationID)
	if err != nil || newAppRoute.Mode != "assistant" {
		t.Fatalf("periodic group route: %+v err=%v", newAppRoute, err)
	}

	// Calling sync again with the same active set must be a no-op: no new
	// route, no route_ids churn, no version bump.
	var repeat RuntimeConfig
	var addedAgain int
	runtimeMutate(t, f.s, "group.mention.sync.repeat", func(tx *Tx) (any, error) {
		var err error
		repeat, addedAgain, err = tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID, dwsC.ConversationID})
		return repeat, err
	})
	if addedAgain != 0 || repeat.Version != afterC.Version {
		t.Fatalf("idempotent sync changed state: added=%d before=%+v after=%+v", addedAgain, afterC, repeat)
	}
}

// A group that stops being DWS-active (or whose robot left) must drop out of
// route_ids on the next sync while its route and audit trail remain intact.
func TestGroupMentionRuntimeRemovesInactiveGroupButKeepsRoute(t *testing.T) {
	f := newGroupMentionSyncFixture(t)
	ctx := context.Background()
	var afterBoth RuntimeConfig
	runtimeMutate(t, f.s, "group.mention.sync.both", func(tx *Tx) (any, error) {
		var err error
		afterBoth, _, err = tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID, f.dwsB.ConversationID})
		return afterBoth, err
	})
	groupBRoute, err := RouteFor(ctx, f.s.DB, f.appChannel.ID, f.dwsB.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(afterBoth.RouteIDs, groupBRoute.ID) {
		t.Fatalf("group B was not adopted before going silent: %+v", afterBoth.RouteIDs)
	}
	var afterSilent RuntimeConfig
	runtimeMutate(t, f.s, "group.mention.sync.silent", func(tx *Tx) (any, error) {
		var err error
		afterSilent, _, err = tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID})
		return afterSilent, err
	})
	if contains(afterSilent.RouteIDs, groupBRoute.ID) {
		t.Fatalf("silent group route remained in the processing set: %+v", afterSilent.RouteIDs)
	}
	if !contains(afterSilent.RouteIDs, f.appA.ID) {
		t.Fatalf("the still-active group was dropped: %+v", afterSilent.RouteIDs)
	}
	stillThere, err := ReadRoute(ctx, f.s.DB, groupBRoute.ID)
	if err != nil || stillThere.Status != "active" {
		t.Fatalf("route and audit for the silent group must remain: %+v err=%v", stillThere, err)
	}
}

// A DWS route explicitly marked ignore must never get an app assistant
// route, and an app route independently marked ignore must never be restored
// into route_ids even though its DWS conversation is still active.
func TestGroupMentionRuntimeIgnorePriorityOnBothSides(t *testing.T) {
	f := newGroupMentionSyncFixture(t)
	ctx := context.Background()
	var afterIgnored RuntimeConfig
	var added int
	runtimeMutate(t, f.s, "group.mention.sync.ignored-dws", func(tx *Tx) (any, error) {
		var err error
		afterIgnored, added, err = tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID, f.dwsIgnored.ConversationID})
		return afterIgnored, err
	})
	if added != 0 {
		t.Fatalf("an app route was created for a DWS-ignored group: added=%d", added)
	}
	if _, err := RouteFor(ctx, f.s.DB, f.appChannel.ID, f.dwsIgnored.ConversationID); ErrorCode(err) != "denied" {
		t.Fatalf("an app route was created for a DWS-ignored group: err=%v", err)
	}

	// Now adopt group B, then mark its app route ignore directly (e.g. the
	// robot was removed and the operator locked it out) and confirm it is
	// never restored even while still DWS-active.
	runtimeMutate(t, f.s, "group.mention.sync.adopt-b", func(tx *Tx) (any, error) {
		cfg, _, err := tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID, f.dwsB.ConversationID})
		return cfg, err
	})
	groupBRoute, err := RouteFor(ctx, f.s.DB, f.appChannel.ID, f.dwsB.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeMutate(t, f.s, "group.mention.route.ignore-b", func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, groupBRoute.ID, groupBRoute.Version, RouteInput{Mode: "ignore"}, "lock out group B")
	})
	var afterAppIgnore RuntimeConfig
	runtimeMutate(t, f.s, "group.mention.sync.after-app-ignore", func(tx *Tx) (any, error) {
		var err error
		afterAppIgnore, _, err = tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID, f.dwsB.ConversationID})
		return afterAppIgnore, err
	})
	if contains(afterAppIgnore.RouteIDs, groupBRoute.ID) {
		t.Fatalf("an app-side ignored route was restored by sync: %+v", afterAppIgnore.RouteIDs)
	}
}

// A failed or malformed discovery input must leave route_ids exactly as it
// was, never silently shrinking the processing scope.
func TestGroupMentionRuntimeSyncPreservesOldRangeOnInvalidInput(t *testing.T) {
	f := newGroupMentionSyncFixture(t)
	ctx := context.Background()
	var before RuntimeConfig
	runtimeMutate(t, f.s, "group.mention.sync.before", func(tx *Tx) (any, error) {
		var err error
		before, _, err = tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID, f.dwsB.ConversationID})
		return before, err
	})
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "group.mention.sync.invalid"}, func(tx *Tx) (any, error) {
		cfg, _, syncErr := tx.SyncGroupMentionRoutes(ctx, f.config.ID, []string{f.dwsA.ConversationID, "not a valid id with spaces"})
		return cfg, syncErr
	})
	if ErrorCode(err) != "invalid_input" {
		t.Fatalf("invalid discovered ID was not rejected: %v", err)
	}
	after, err := ReadRuntime(ctx, f.s.DB, f.config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if JSON(after.RouteIDs) != JSON(before.RouteIDs) || after.Version != before.Version {
		t.Fatalf("route_ids changed despite a failed sync: before=%+v after=%+v", before, after)
	}
}
