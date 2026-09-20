package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

func TestOwnerDirectTransportDoesNotClassifyText(t *testing.T) {
	cfg := core.RuntimeConfig{ApplicationMode: "direct", RouteIDs: []string{"owner"}, OwnerPrincipalID: "principal", BootstrapAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}
	for _, body := range []string{"hi", "今天讨论了一个方案", "继续", "取消刚才的任务", "补充一点", "记住我的偏好", "/clear", "/status", "/unknown", "你好\n原始内容"} {
		m := core.RuntimeMessage{ID: "message", Sender: "principal", Addressed: true, SentAt: time.Now().UTC().Format(time.RFC3339Nano), Body: body}
		b := core.RuntimeBatch{Mode: "direct", RouteID: "owner", Messages: []core.RuntimeMessage{m}}
		if !directRouteEligible(cfg, b) || len(directTurnReceipts(cfg, b).Decisions) != 1 {
			t.Fatalf("message was classified or dropped: %q", body)
		}
		b.Messages[0].Sender = "other"
		if len(directTurnReceipts(cfg, b).Decisions) != 0 {
			t.Fatal("foreign sender admitted")
		}
	}
}

type contextOnlyAnalyzer struct {
	calls int
	batch core.RuntimeBatch
}

func (a *contextOnlyAnalyzer) Analyze(_ context.Context, b core.RuntimeBatch) (core.RuntimeAnalysis, ModelUsage, error) {
	a.calls++
	a.batch = b
	time.Sleep(12 * time.Millisecond)
	return core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "context"}}}, ModelUsage{Model: "claude-haiku-test"}, nil
}

type modelReportingExecutor struct {
	calls     int
	actions   []core.RuntimeAction
	contexts  [][]core.RuntimeMessage
	inputs    []ExecutionInput
	onExecute func(ExecutionInput)
}

func (e *modelReportingExecutor) Execute(_ context.Context, input ExecutionInput) (core.RuntimeAttemptResult, error) {
	e.calls++
	e.inputs = append(e.inputs, input)
	e.contexts = append(e.contexts, append([]core.RuntimeMessage{}, input.ConversationContext...))
	if e.onExecute != nil {
		e.onExecute(input)
	}
	time.Sleep(12 * time.Millisecond)
	return core.RuntimeAttemptResult{Result: "新版 memgov 已接收并处理这条私聊。", Summary: "私聊测试已回复",
		Usage: map[string]any{"model": "claude-sonnet-test"}, Actions: e.actions}, nil
}

type directReactionAdapter struct {
	*fakeAdapter
	added, removed []string
}

func (a *directReactionAdapter) AddReaction(ctx context.Context, cfg channel.Config, req channel.ReactionRequest) error {
	a.added = append(a.added, req.Emoji)
	return a.fakeAdapter.AddReaction(ctx, cfg, req)
}

func (a *directReactionAdapter) RemoveReaction(_ context.Context, _ channel.Config, req channel.ReactionRequest) error {
	a.removed = append(a.removed, req.Emoji)
	return nil
}

func TestDirectFastReplySkipsHaikuAndDeliversMeasuredReply(t *testing.T) {
	// A long private request must still reach the executor; Search accepts at
	// most 30 terms and this request intentionally exceeds that limit.
	question := "请回复这条私聊测试消息，确认新版 memgov 已接管机器人回复，并写出实际模型信息与分析耗时" + strings.Repeat(" extra", 35)
	runDirectReplyScenario(t, question, false, nil, false)
}

func TestRepoBoundDirectGreetingCanReplyWithoutCommit(t *testing.T) {
	runDirectReplyScenario(t, "你好", true, nil, false)
}

func TestDirectExternalWriteStillRequiresConfirmation(t *testing.T) {
	runDirectReplyScenario(t, "请通知同事这条结果", false,
		[]core.RuntimeAction{{Kind: "send_message", Target: "another-person", Payload: "已核对的结果"}}, true)
}

func TestRepoBoundDirectCodeRequestIsDelegatedToAgent(t *testing.T) {
	runDirectReplyScenario(t, "请修复仓库的 README 文件并运行测试", true, nil, false)
}

func TestDirectFollowupUsesOnlyVerifiedOwnerPrivateDeliveredTurns(t *testing.T) {
	first := runDirectReplyScenario(t, "请告诉我第一条私聊测试的结果", false, nil, false)
	ctx := context.Background()
	if _, err := first.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.other.route"}, func(tx *core.Tx) (any, error) {
		return tx.AddRoute(ctx, first.cfg.ChannelID, core.RouteInput{ConversationID: "cid:other-private", ConversationType: "direct", Mode: "ignore"})
	}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.NormalizedEvent{
		{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/4", Origin: "stream", ProviderMessageID: "other-conversation", ConversationID: "cid:other-private", ConversationType: "direct", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "owner"}, Body: "跨会话隐私内容", SentAt: time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339Nano)},
		{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/4", Origin: "stream", ProviderMessageID: "other-sender", ConversationID: first.route.ConversationID, ConversationType: "direct", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "other"}, Body: "他人的私聊内容", SentAt: time.Now().Add(91 * time.Minute).UTC().Format(time.RFC3339Nano)},
		{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/4", Origin: "stream", ProviderMessageID: "followup", ConversationID: first.route.ConversationID, ConversationType: "direct", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "owner"}, Body: "那上一条呢？", SentAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)},
	} {
		if _, err := first.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.followup.intake"}, func(tx *core.Tx) (any, error) {
			return tx.Intake(ctx, first.cfg.ChannelID, event)
		}); err != nil {
			t.Fatal(err)
		}
	}
	first.service.tick(ctx, first.cfg, first.preset)
	// Each direct receipt consumes one message; an unverified sender is discarded
	// independently rather than being batched with the owner's next turn.
	first.service.tick(ctx, first.cfg, first.preset)
	if first.analyzer.calls != 0 || first.executor.calls != 2 || first.adapter.sends != 2 {
		t.Fatalf("followup waited for Haiku or failed delivery: analysis=%d execution=%d sends=%d", first.analyzer.calls, first.executor.calls, first.adapter.sends)
	}
	turns := first.executor.contexts[1]
	owner, bot := false, false
	for _, turn := range turns {
		if turn.SelfAuthored && turn.Body == core.RuntimeAcknowledgement {
			t.Fatal("receipt entered accepted answer history")
		}
		if strings.Contains(turn.Body, "跨会话隐私内容") || strings.Contains(turn.Body, "他人的私聊内容") {
			t.Fatalf("direct context crossed owner route: %+v", turns)
		}
		if !turn.SelfAuthored && strings.Contains(turn.Body, "第一条私聊测试") {
			owner = true
		}
		if turn.SelfAuthored && strings.Contains(turn.Body, "新版 memgov 已接收") {
			bot = true
		}
	}
	if !owner || !bot || len(turns) > 20 {
		t.Fatalf("followup lost prior accepted owner/bot turns: owner=%t bot=%t count=%d", owner, bot, len(turns))
	}
	// The same provider frame cannot create or deliver the followup twice.
	if _, err := first.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.followup.duplicate"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, first.cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/4", Origin: "stream", ProviderMessageID: "followup", ConversationID: first.route.ConversationID, ConversationType: "direct", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "owner"}, Body: "那上一条呢？", SentAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	first.service.tick(ctx, first.cfg, first.preset)
	if first.executor.calls != 2 || first.adapter.sends != 2 || first.analyzer.calls != 0 {
		t.Fatalf("duplicate direct frame created new work: analysis=%d execution=%d sends=%d", first.analyzer.calls, first.executor.calls, first.adapter.sends)
	}
}

func TestDirectSupplementGoesToAgentWithoutHaiku(t *testing.T) {
	first := runDirectReplyScenario(t, "请告诉我第一条结果", false, nil, false)
	ctx := context.Background()
	if _, err := first.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.supplement.intake"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, first.cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/4", Origin: "stream", ProviderMessageID: "supplement", ConversationID: first.route.ConversationID, ConversationType: "direct", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "owner"}, Body: "补充刚才的结果", SentAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	first.service.tick(ctx, first.cfg, first.preset)
	if first.analyzer.calls != 0 || first.executor.calls != 2 || first.adapter.sends != 2 {
		t.Fatalf("supplement was not passed to Agent: analysis=%d execution=%d sends=%d", first.analyzer.calls, first.executor.calls, first.adapter.sends)
	}
	var nativeSessionID string
	if err := first.service.Store.DB.QueryRowContext(ctx, "SELECT native_session_id FROM runtime_direct_sessions WHERE id=?", first.executor.inputs[0].SessionID).Scan(&nativeSessionID); err != nil || nativeSessionID == "" {
		t.Fatalf("first direct turn did not persist native session: id=%q error=%v", nativeSessionID, err)
	}
	if first.executor.inputs[1].ResumeSessionID != nativeSessionID || first.executor.inputs[1].NativeSessionID != nativeSessionID {
		t.Fatalf("follow-up did not resume persisted native session: input=%+v persisted=%q", first.executor.inputs[1], nativeSessionID)
	}
	tasks, err := core.RuntimeTaskList(ctx, first.service.Store.DB, first.cfg.ID, "", 10)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("supplement was not forwarded: count=%d error=%v", len(tasks), err)
	}
}

func TestDirectSplitRouteRejectsWrongOwnerDispatchAndRevokedIdentity(t *testing.T) {
	first := runDirectReplyScenario(t, "你好", false, nil, false)
	ctx := context.Background()
	if first.route.ID == first.cfg.DeliveryRouteID || first.route.SendPolicy != "draft_only" {
		t.Fatal("fixture stopped modeling distinct callback and owner dispatch routes")
	}
	if _, err := core.RuntimeOwnerDirectProcessingRoute(ctx, first.service.Store.DB, first.cfg, first.route.ID); err != nil {
		t.Fatalf("verified split-route pair was denied: %v", err)
	}
	wrong := first.cfg
	wrong.OwnerIDValue = "another-owner"
	if _, err := core.RuntimeOwnerDirectProcessingRoute(ctx, first.service.Store.DB, wrong, first.route.ID); core.ErrorCode(err) != "denied" {
		t.Fatalf("wrong owner dispatch address was admitted: %v", err)
	}
	wrong = first.cfg
	wrong.RouteIDs = []string{first.cfg.DeliveryRouteID}
	if _, err := core.RuntimeOwnerDirectProcessingRoute(ctx, first.service.Store.DB, wrong, first.route.ID); core.ErrorCode(err) != "denied" {
		t.Fatalf("callback route outside processing scope was admitted: %v", err)
	}
	tasks, err := core.RuntimeTaskList(ctx, first.service.Store.DB, first.cfg.ID, "completed", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("split-route fixture lost the completed task: count=%d error=%v", len(tasks), err)
	}
	forgedID := core.NewID()
	if _, err := first.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.outbox.forge"}, func(tx *core.Tx) (any, error) {
		_, e := tx.Conn.ExecContext(ctx, `INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,sender_identity,transport,content,format,input_digest,send_policy,state,created_at,updated_at)
SELECT ?,channel_id,?,route_version,job_id,?,audience_key,sender_identity,transport,content,format,input_digest,'draft_only','ready',?,?
FROM outbox WHERE job_id=? AND state='accepted' AND reason NOT IN ('runtime_receipt','runtime_processing_receipt','runtime_completion_receipt','runtime_failure_receipt')`, forgedID, first.route.ID, first.route.ConversationID, core.Now(), core.Now(), tasks[0].ID)
		return nil, e
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.outbox.begin"}, func(tx *core.Tx) (any, error) {
		_, e := tx.BeginDelivery(ctx, forgedID)
		return nil, e
	}); core.ErrorCode(err) != "denied" {
		t.Fatalf("ready outbox on callback CID advanced to sending: %v", err)
	}
	var forgedState string
	if err := first.service.Store.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE id=?", forgedID).Scan(&forgedState); err != nil || forgedState != "ready" {
		t.Fatalf("denied outbox changed state: state=%s error=%v", forgedState, err)
	}
	if _, err := first.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.owner.revoke"}, func(tx *core.Tx) (any, error) {
		_, e := tx.Conn.ExecContext(ctx, "UPDATE identity_aliases SET verified=0 WHERE tenant=? AND id_type=? AND id_value=?", "corp", first.cfg.OwnerIDType, first.cfg.OwnerIDValue)
		return nil, e
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := core.RuntimeOwnerDirectProcessingRoute(ctx, first.service.Store.DB, first.cfg, first.route.ID); core.ErrorCode(err) != "denied" {
		t.Fatalf("revoked owner identity still admitted private context: %v", err)
	}
}

func runDirectReplyScenario(t *testing.T, question string, repoBound bool, proposedActions []core.RuntimeAction, pending bool) directScenario {
	t.Helper()
	service, sourceConfig, preset, _, adapter := setupService(t)
	ctx := context.Background()
	var app core.Channel
	_, err := service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.app.add"}, func(tx *core.Tx) (any, error) {
		history, e := core.ReadChannel(ctx, tx.Conn, sourceConfig.ChannelID)
		if e != nil {
			return nil, e
		}
		if _, e = tx.AttestDWSOwner(ctx, history.ID, history.ConfigVersion); e != nil {
			return nil, e
		}
		app, e = tx.AddChannel(ctx, core.ChannelInput{Name: "direct-app", Kind: core.ChannelDingTalkApp,
			Identity: core.ChannelIdentity{ExpectedCorpID: "corp", ClientID: "app-client", RobotCode: "robot", HistoryChannel: history.ID}})
		return app, e
	})
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := ""
	if repoBound {
		repo := filepath.Join(t.TempDir(), "repo")
		if err = os.MkdirAll(repo, 0700); err != nil {
			t.Fatal(err)
		}
		gitRun(t, repo, "init", "-b", "main")
		if err = os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitRun(t, repo, "add", "README.md")
		gitRun(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "base")
		_, err = service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.repo.workspace"}, func(tx *core.Tx) (any, error) {
			workspace, e := tx.AddWorkspace(ctx, "direct-repo", repo)
			workspaceID = workspace.ID
			return workspace, e
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var route, delivery core.Route
	_, err = service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.app.route"}, func(tx *core.Tx) (any, error) {
		var e error
		route, e = tx.AddRoute(ctx, app.ID, core.RouteInput{ConversationID: "cid:owner-app", ConversationType: "direct",
			Mode: "assistant", SendPolicy: "draft_only", Workspace: workspaceID})
		return route, e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.app.delivery"}, func(tx *core.Tx) (any, error) {
		var e error
		delivery, e = tx.AddRoute(ctx, app.ID, core.RouteInput{ConversationID: "owner", ConversationType: "direct",
			Mode: "notify", SendPolicy: "dispatch_only"})
		return delivery, e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.app.capabilities"}, func(tx *core.Tx) (any, error) {
		return tx.SetChannelCapabilities(ctx, app.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "send": true}}, "fake")
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg core.RuntimeConfig
	_, err = service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.runtime.configure"}, func(tx *core.Tx) (any, error) {
		var e error
		var bash *bool
		externalActions := ""
		if pending {
			closed := false
			bash, externalActions = &closed, "owner_confirmation"
		}
		cfg, e = tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Name: "direct-bot", Channel: app.ID,
			RouteIDs: []string{route.ID}, DeliveryRouteID: delivery.ID, Owner: core.Sender{IDType: "user_id", IDValue: "owner"},
			ApplicationMode: "direct", AgentBash: bash, ExternalActions: externalActions, ItemThreshold: 1, MaxWaitSeconds: 300, ReconcileSeconds: 10})
		return cfg, e
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.runtime.start"}, func(tx *core.Tx) (any, error) {
		return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "")
	}); err != nil {
		t.Fatal(err)
	}
	logger, err := runlog.Open(service.Home, cfg.ID, io.Discard, runlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	service.Logger = logger
	reactions := &directReactionAdapter{fakeAdapter: adapter}
	service.Adapter = reactions
	_, err = service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "message.intake.direct"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/4",
			Origin: "stream", ProviderMessageID: "direct-request", ConversationID: route.ConversationID, ConversationType: "direct", Tenant: "corp",
			Sender: core.Sender{IDType: "user_id", IDValue: "owner"}, Body: question,
			SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &contextOnlyAnalyzer{}
	executor := &modelReportingExecutor{actions: proposedActions, onExecute: func(ExecutionInput) {
		if adapter.receipts != 2 || len(adapter.requests) != 0 || len(adapter.reactions) != 2 || adapter.reactions[0].Emoji != core.RuntimeAcknowledgement || adapter.reactions[1].Emoji != core.RuntimeProcessingAcknowledgement || adapter.reactions[1].MessageID != "direct-request" || adapter.reactions[1].ConversationID != route.ConversationID || len(reactions.removed) != 1 || reactions.removed[0] != core.RuntimeAcknowledgement {
			t.Fatalf("Agent did not enter processing after owner acknowledgement: reactions=%+v removed=%+v", adapter.reactions, reactions.removed)
		}
	}}
	service.Analyzer, service.Executor = analyzer, executor
	service.tick(ctx, cfg, preset)
	executor.onExecute = nil
	if analyzer.calls != 0 || adapter.sends != 1 {
		var state, status string
		_ = service.Store.DB.QueryRowContext(ctx, "SELECT state FROM runtime_message_states WHERE runtime_id=? ORDER BY first_seen_at DESC LIMIT 1", cfg.ID).Scan(&state)
		_ = service.Store.DB.QueryRowContext(ctx, "SELECT status FROM runtime_configs WHERE id=?", cfg.ID).Scan(&status)
		t.Logf("runtime status=%s message state=%s executor_calls=%d", status, state, executor.calls)
		if len(analyzer.batch.Messages) > 0 {
			m := analyzer.batch.Messages[0]
			t.Logf("batch mode=%s route_match=%t owner_match=%t addressed=%t self=%t fresh=%t requested=%t", analyzer.batch.Mode,
				analyzer.batch.RouteID == cfg.DeliveryRouteID, m.Sender == cfg.OwnerPrincipalID, m.Addressed, m.SelfAuthored,
				newerThanBootstrap(m.SentAt, cfg.BootstrapAt), true)
		}
		all, _ := core.RuntimeTaskList(ctx, service.Store.DB, cfg.ID, "", 10)
		for _, task := range all {
			t.Logf("task status=%s error=%s", task.Status, task.ErrorCode)
		}
		t.Fatalf("private reply was not executed and sent: analysis=%d sends=%d", analyzer.calls, adapter.sends)
	}
	wantStatus := "completed"
	if pending {
		wantStatus = "awaiting_confirmation"
	}
	tasks, err := core.RuntimeTaskList(ctx, service.Store.DB, cfg.ID, wantStatus, 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("private reply task had wrong status %s: count=%d error=%v", wantStatus, len(tasks), err)
	}
	task, err := core.ReadRuntimeTask(ctx, service.Store.DB, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if repoBound && (len(task.Attempts) != 1 || !strings.Contains(task.Attempts[0].WorkspaceDir, filepath.Join("runtime", "sessions")) || len(task.Attempts[0].Artifacts) != 0) {
		t.Fatalf("direct response did not stay in its conversation directory: attempts=%+v", task.Attempts)
	}
	if task.AnalysisDurationMS < 0 || task.AnalysisModel != "direct_agent" || executor.calls != 1 {
		t.Fatalf("private reply lost measured intake data or execution: analysis_ms=%d model=%s calls=%d",
			task.AnalysisDurationMS, task.AnalysisModel, executor.calls)
	}
	if executor.inputs[0].BashEnabled == pending {
		t.Fatalf("private Bash default/override lost: enabled=%t pending=%t", executor.inputs[0].BashEnabled, pending)
	}
	wantExternal := "owner_request"
	if pending {
		wantExternal = "owner_confirmation"
	}
	if executor.inputs[0].ExternalActions != wantExternal {
		t.Fatalf("private external action policy lost: %s", executor.inputs[0].ExternalActions)
	}
	analysisEvents, err := runlog.Show(service.Home, cfg.ID, runlog.Filter{Component: "intake"})
	queued := analysisEvents[:0]
	for _, event := range analysisEvents {
		if event.Event == "turn_queued" {
			queued = append(queued, event)
		}
	}
	analysisEvents = queued
	if err != nil || len(analysisEvents) != 1 || analysisEvents[0].Event != "turn_queued" || analysisEvents[0].Model != "" {
		t.Fatalf("direct local intake was mislabeled as a Haiku call: events=%+v error=%v", analysisEvents, err)
	}
	var content, state string
	if err = service.Store.DB.QueryRowContext(ctx, "SELECT content,state FROM outbox WHERE job_id=? AND reason NOT IN ('runtime_receipt','runtime_processing_receipt','runtime_completion_receipt','runtime_failure_receipt')", tasks[0].ID).Scan(&content, &state); err != nil || state != "accepted" {
		t.Fatalf("private reply was not accepted: state=%s error=%v", state, err)
	}
	if pending && (len(task.Actions) != 1 || task.Actions[0].Status != "pending" || !strings.Contains(content, "确认口令")) {
		t.Fatalf("separate external write lost its owner confirmation: actions=%+v", task.Actions)
	}
	for _, marker := range []string{"> **你问：**", "⚡ 接入", "⏱ 执行", "🤖 claude-sonnet-test", "新版 memgov 已接收"} {
		if !strings.Contains(content, marker) {
			t.Fatalf("private reply lost its quote or measured metadata: missing=%s", marker)
		}
	}
	wantAdded := []string{core.RuntimeAcknowledgement, core.RuntimeProcessingAcknowledgement}
	wantRemoved := []string{core.RuntimeAcknowledgement}
	if !pending {
		wantAdded = append(wantAdded, core.RuntimeCompletionAcknowledgement)
		wantRemoved = append(wantRemoved, core.RuntimeProcessingAcknowledgement)
	}
	if adapter.receipts != len(wantAdded) || !slices.Equal(reactions.added, wantAdded) || !slices.Equal(reactions.removed, wantRemoved) {
		t.Fatalf("private acknowledgement/reaction policy: receipts=%d added=%v removed=%v", adapter.receipts, reactions.added, reactions.removed)
	}
	return directScenario{service: service, cfg: cfg, route: route, preset: preset, analyzer: analyzer, executor: executor, adapter: adapter}
}

type directScenario struct {
	service  *Service
	cfg      core.RuntimeConfig
	route    core.Route
	preset   agent.Preset
	analyzer *contextOnlyAnalyzer
	executor *modelReportingExecutor
	adapter  *fakeAdapter
}
