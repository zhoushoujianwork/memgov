package runtime

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

type fakeAdapter struct {
	sends     int
	receipts  int
	requests  []channel.SendRequest
	reactions []channel.ReactionRequest
	removed   []channel.ReactionRequest
	removeErr error
}

func (*fakeAdapter) Name() string { return "fake" }
func (*fakeAdapter) ProbeCapabilities(context.Context, channel.Config) (core.Capabilities, error) {
	return core.Capabilities{}, nil
}
func (*fakeAdapter) ReadWindow(context.Context, channel.Config, channel.Window) (channel.WindowResult, error) {
	return channel.WindowResult{Complete: true}, nil
}
func (*fakeAdapter) RunReceiver(ctx context.Context, _ channel.Config, _ channel.ReceiverOptions) error {
	<-ctx.Done()
	return nil
}
func (a *fakeAdapter) Send(_ context.Context, _ channel.Config, req channel.SendRequest) (channel.SendResult, error) {
	a.requests = append(a.requests, req)
	a.sends++
	return channel.SendResult{State: "accepted", Receipt: "local-receipt"}, nil
}

func (a *fakeAdapter) AddReaction(_ context.Context, _ channel.Config, req channel.ReactionRequest) error {
	a.receipts++
	a.reactions = append(a.reactions, req)
	return nil
}
func (a *fakeAdapter) RemoveReaction(_ context.Context, _ channel.Config, req channel.ReactionRequest) error {
	a.removed = append(a.removed, req)
	return a.removeErr
}

type persistentReceiverAdapter struct {
	fakeAdapter
	deadline chan bool
}

func (a *persistentReceiverAdapter) RunReceiver(ctx context.Context, _ channel.Config, _ channel.ReceiverOptions) error {
	_, hasDeadline := ctx.Deadline()
	a.deadline <- hasDeadline
	<-ctx.Done()
	return nil
}

type fakeModels struct {
	analyses int
	executes int
}

type fakeMemoryModels struct {
	analyses int
	executes int
	reviews  int
}

func (m *fakeModels) Analyze(_ context.Context, b core.RuntimeBatch) (core.RuntimeAnalysis, ModelUsage, error) {
	m.analyses++
	return core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "one", Title: "answer", Instructions: "answer and verify", MessageIDs: []string{b.Messages[0].ID}}}}, ModelUsage{InputTokens: 2, OutputTokens: 1}, nil
}
func (m *fakeModels) Execute(context.Context, ExecutionInput) (core.RuntimeAttemptResult, error) {
	m.executes++
	return core.RuntimeAttemptResult{Result: "完成", Summary: "已完成", ToolKinds: []string{"query"}, Usage: map[string]any{"input_tokens": 3, "output_tokens": 2}}, nil
}
func (*fakeModels) ExecuteConfirmedAction(context.Context, ActionExecutionInput) (core.RuntimeAttemptResult, error) {
	return core.RuntimeAttemptResult{Result: "executed"}, nil
}
func (*fakeModels) Review(context.Context, core.RuntimeTask, core.CandidateInput) (ReviewResult, error) {
	return ReviewResult{Decision: "accept", Issues: []string{}}, nil
}

func (m *fakeMemoryModels) Analyze(_ context.Context, b core.RuntimeBatch) (core.RuntimeAnalysis, ModelUsage, error) {
	m.analyses++
	return core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{
		Kind:         "memory",
		CanonicalKey: "memory:s3-proxy-signed-url-limit",
		Title:        "S3 proxy 签名链接限制",
		Instructions: "提炼已确认的 S3 proxy 能力边界，并保留原消息证据。",
		MessageIDs:   []string{b.Messages[0].ID},
	}}}, ModelUsage{InputTokens: 3, OutputTokens: 2}, nil
}

func (m *fakeMemoryModels) Execute(_ context.Context, in ExecutionInput) (core.RuntimeAttemptResult, error) {
	m.executes++
	message := in.Task.Messages[0]
	return core.RuntimeAttemptResult{
		Result:    "已生成受治理的记忆候选。",
		Summary:   "S3 proxy 仅支持普通文件操作，不能生成可供外部使用的签名链接。",
		ToolKinds: []string{"memory.propose"},
		Usage:     map[string]any{"input_tokens": 4, "output_tokens": 3},
		Candidate: &core.CandidateInput{
			Action: "create",
			Reason: "保留对后续接口选型有影响的稳定能力边界。",
			Memory: core.Memory{
				Category: "fact",
				Title:    "S3 proxy 签名链接限制",
				Summary:  "S3 proxy 不能为 COS 文件生成可供外部使用的签名链接。",
				Content:  "S3 proxy 只支持普通文件操作，不支持为 COS 文件生成可供外部使用的签名链接；即使产生签名结果，域名仍是 S3 proxy 域名，不能用于外部访问。",
				Evidence: []core.Evidence{{SourceID: message.SourceID, FragmentID: message.FragmentID, SHA256: message.SHA256}},
			},
		},
	}, nil
}

func (*fakeMemoryModels) ExecuteConfirmedAction(context.Context, ActionExecutionInput) (core.RuntimeAttemptResult, error) {
	return core.RuntimeAttemptResult{Result: "executed"}, nil
}

func (m *fakeMemoryModels) Review(context.Context, core.RuntimeTask, core.CandidateInput) (ReviewResult, error) {
	m.reviews++
	return ReviewResult{Decision: "accept", Issues: []string{}}, nil
}

func setupService(t *testing.T) (*Service, core.RuntimeConfig, agent.Preset, *fakeModels, *fakeAdapter) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	s, err := core.Open(ctx, filepath.Join(home, "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var c core.Channel
	var watch core.Route
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "channel.add"}, func(tx *core.Tx) (any, error) {
		var addErr error
		c, addErr = tx.AddChannel(ctx, core.ChannelInput{Name: "dws-main", Kind: core.ChannelDwsPersonal, Identity: core.ChannelIdentity{ExpectedCorpID: "corp", ExpectedUserID: "owner", DeliveryRobotCode: "robot"}, Route: &core.RouteInput{ConversationID: "watch", ConversationType: "group"}})
		if addErr == nil {
			watch = c.Routes[0]
		}
		return c, addErr
	})
	if err != nil {
		t.Fatal(err)
	}
	var direct core.Route
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "route.add"}, func(tx *core.Tx) (any, error) {
		var addErr error
		direct, addErr = tx.AddRoute(ctx, c.ID, core.RouteInput{ConversationID: "owner", ConversationType: "direct"})
		return direct, addErr
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "route.enable"}, func(tx *core.Tx) (any, error) {
		return tx.UpdateRoute(ctx, direct.ID, direct.Version, core.RouteInput{SendPolicy: "dispatch_only"}, "enable owner delivery")
	})
	if err != nil {
		t.Fatal(err)
	}
	direct, _ = core.ReadRoute(ctx, s.DB, direct.ID)
	owner := core.Sender{IDType: "user_id", IDValue: "owner"}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "owner.intake"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, c.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "history", ProviderMessageID: "owner-bootstrap", ConversationID: direct.ConversationID, Tenant: c.Tenant, Sender: owner, Body: "context", SentAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "channel.capabilities"}, func(tx *core.Tx) (any, error) {
		return tx.SetChannelCapabilities(ctx, c.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true, "send": true}}, "fake")
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg core.RuntimeConfig
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "runtime.configure"}, func(tx *core.Tx) (any, error) {
		var configErr error
		cfg, configErr = tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Name: "watcher", Channel: c.ID, RouteIDs: []string{watch.ID}, DeliveryRouteID: direct.ID, Owner: owner, ItemThreshold: 1, MaxWaitSeconds: 300, ReconcileSeconds: 10})
		return cfg, configErr
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "runtime.start"}, func(tx *core.Tx) (any, error) { return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "") })
	if err != nil {
		t.Fatal(err)
	}
	preset, err := agent.Enable(ctx, home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	logger, err := runlog.Open(home, cfg.ID, io.Discard, runlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	models, adapter := &fakeModels{}, &fakeAdapter{}
	service := &Service{Home: home, Store: s, Adapter: adapter, Analyzer: models, Executor: models, Actioner: models, Reviewer: models, Logger: logger}
	return service, cfg, preset, models, adapter
}

func TestTickDoesNotCallModelsWhenIdleAndRecordsCompletedWork(t *testing.T) {
	service, cfg, preset, models, adapter := setupService(t)
	ctx := context.Background()
	service.tick(ctx, cfg, preset)
	if models.analyses != 0 || models.executes != 0 {
		t.Fatalf("idle tick called models: analysis=%d execution=%d", models.analyses, models.executes)
	}
	_, err := service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "message.intake"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderEventID: "event-1", ProviderMessageID: "message-1", ConversationID: "watch", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "alice"}, Body: "please answer", SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.mutate(ctx, "global", "test.sync", func(tx *core.Tx) (any, error) {
		return tx.SyncRuntimeMessages(ctx, cfg.ID)
	}); err != nil {
		t.Fatal(err)
	}
	service.tick(ctx, cfg, preset)
	if models.analyses != 1 || models.executes != 1 || adapter.sends != 0 || adapter.receipts != 0 {
		t.Fatalf("pipeline counts analysis=%d execution=%d sends=%d", models.analyses, models.executes, adapter.sends)
	}
	tasks, err := core.RuntimeTaskList(ctx, service.Store.DB, cfg.ID, "completed", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("completed tasks=%+v err=%v", tasks, err)
	}
	var notifications int
	if err = service.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM outbox WHERE job_id=?", tasks[0].ID).Scan(&notifications); err != nil || notifications != 0 {
		t.Fatalf("record-only task generated %d notifications: %v", notifications, err)
	}
}

func TestMemoryObservationClosesSourceCandidateReviewMemoryLoop(t *testing.T) {
	service, cfg, preset, _, _ := setupService(t)
	models := &fakeMemoryModels{}
	service.Analyzer, service.Executor, service.Actioner, service.Reviewer = models, models, models, models
	ctx := context.Background()

	_, err := service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "message.intake.memory"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{
			Kind:              core.EventMessage,
			Adapter:           "fake",
			ParseVersion:      "1",
			Origin:            "stream",
			ProviderEventID:   "event-memory-1",
			ProviderMessageID: "message-memory-1",
			ConversationID:    "watch",
			Tenant:            "corp",
			Sender:            core.Sender{IDType: "user_id", IDValue: "cai-yuzhou"},
			Body:              "不可以，S3 proxy 只支持普通文件操作，签名后的也是 S3 proxy 域名，不能外部使用。",
			SentAt:            time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	service.tick(ctx, cfg, preset)
	if models.analyses != 1 || models.executes != 1 || models.reviews != 1 {
		t.Fatalf("memory pipeline calls analysis=%d execution=%d review=%d", models.analyses, models.executes, models.reviews)
	}
	tasks, err := core.RuntimeTaskList(ctx, service.Store.DB, cfg.ID, "completed", 10)
	if err != nil || len(tasks) != 1 || tasks[0].Kind != "memory" || tasks[0].CandidateID == "" {
		t.Fatalf("completed memory tasks=%+v err=%v", tasks, err)
	}
	candidate, err := core.ReadCandidate(ctx, service.Store.DB, tasks[0].CandidateID, "global")
	if err != nil || candidate.Status != "applied" || candidate.AppliedMemoryID == "" || candidate.Generator != "runtime-executor" {
		t.Fatalf("governed candidate=%+v err=%v", candidate, err)
	}
	var acceptedReviews int
	if err = service.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM reviews WHERE candidate_id=? AND candidate_digest=? AND decision='accept'", candidate.ID, candidate.Digest).Scan(&acceptedReviews); err != nil || acceptedReviews != 1 {
		t.Fatalf("accepted review count=%d err=%v", acceptedReviews, err)
	}
	memory, err := core.ReadMemory(ctx, service.Store.DB, candidate.AppliedMemoryID, "global", 0)
	if err != nil || memory.Status != "active" || memory.Title != "S3 proxy 签名链接限制" || len(memory.Evidence) != 1 {
		t.Fatalf("applied memory=%+v err=%v", memory, err)
	}
}

func TestLiveMemoryObservationClosesFullLoop(t *testing.T) {
	if os.Getenv("MEMGOV_LIVE_MODEL_TEST") != "1" {
		t.Skip("set MEMGOV_LIVE_MODEL_TEST=1 to run the Claude-backed acceptance test")
	}
	service, cfg, preset, _, _ := setupService(t)
	// Match the installed service's authenticated Claude profile instead of the
	// developer shell's default OAuth session.
	cfg.ClaudeProfile, cfg.ExecutionModel = "cc", "profile"
	installedBinary, err := exec.LookPath("memgov")
	if err != nil {
		t.Fatalf("live acceptance test needs an installed memgov binary: %v", err)
	}
	if err = os.MkdirAll(filepath.Join(service.Home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(installedBinary, filepath.Join(service.Home, "bin", "memgov")); err != nil {
		t.Fatal(err)
	}
	claude := NewClaude(cfg.AnalysisModel, cfg.ExecutionModel)
	claude.Profile = cfg.ClaudeProfile
	service.Analyzer, service.Executor, service.Actioner, service.Reviewer = claude, claude, claude, claude
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	messages := []struct {
		id     string
		sender string
		body   string
	}{
		{id: "live-memory-question", sender: "owner", body: "守键 我们s3 proxy 的key可以给cos上的文件签名吗"},
		{id: "live-memory-answer", sender: "cai-yuzhou", body: "不可以 s3-proxy 不支持签名，只支持普通的文件操作，而且签名后的也是s3proxy的域名，也不能外部使用"},
		{id: "live-memory-ack", sender: "owner", body: "好的"},
	}
	for i, message := range messages {
		_, err := service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "message.intake.live-memory"}, func(tx *core.Tx) (any, error) {
			return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{
				Kind:              core.EventMessage,
				Adapter:           "fake",
				ParseVersion:      "1",
				Origin:            "stream",
				ProviderEventID:   "event-" + message.id,
				ProviderMessageID: message.id,
				ConversationID:    "watch",
				Tenant:            "corp",
				Sender:            core.Sender{IDType: "user_id", IDValue: message.sender},
				Body:              message.body,
				SentAt:            time.Now().Add(time.Hour + time.Duration(i)*time.Second).UTC().Format(time.RFC3339Nano),
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 5; i++ {
		service.tick(ctx, cfg, preset)
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		candidates, err := core.CandidateList(ctx, service.Store.DB, "global", "applied")
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) > 0 {
			memory, err := core.ReadMemory(ctx, service.Store.DB, candidates[0].AppliedMemoryID, "global", 0)
			if err != nil || memory.Status != "active" || len(memory.Evidence) == 0 {
				t.Fatalf("live applied memory=%+v err=%v", memory, err)
			}
			if !strings.Contains(strings.ToLower(memory.Content+" "+memory.Summary), "proxy") {
				t.Fatalf("live memory lost the named system: %+v", memory)
			}
			return
		}
	}

	tasks, err := core.RuntimeTaskList(ctx, service.Store.DB, cfg.ID, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	var batchStatus, batchOutput, batchError string
	_ = service.Store.DB.QueryRowContext(ctx, "SELECT status,output,error_code FROM runtime_batches WHERE runtime_id=? ORDER BY created_at DESC LIMIT 1", cfg.ID).Scan(&batchStatus, &batchOutput, &batchError)
	t.Fatalf("live model did not produce and apply a memory candidate; tasks=%+v batch_status=%s batch_error=%s batch_output=%s", tasks, batchStatus, batchError, batchOutput)
}

func TestRuntimeCapabilitiesAllowStreamOnlyApplicationBot(t *testing.T) {
	verifiedStream := core.Capabilities{Verified: map[string]bool{"receive": true, "send": true}}
	if !runtimeCapabilitiesReady(core.Channel{Kind: core.ChannelDingTalkApp, Capabilities: verifiedStream}, core.RuntimeConfig{ApplicationMode: "direct"}) {
		t.Fatal("application bot should start from its Stream bootstrap without history")
	}
	if runtimeCapabilitiesReady(core.Channel{Kind: core.ChannelDwsPersonal, Capabilities: verifiedStream}, core.RuntimeConfig{ApplicationMode: "proactive"}) {
		t.Fatal("personal DWS watcher needs history for reconciliation")
	}
	verifiedDWS := core.Capabilities{Verified: map[string]bool{"receive": true, "history": true}}
	if !runtimeCapabilitiesReady(core.Channel{Kind: core.ChannelDwsPersonal, Capabilities: verifiedDWS}, core.RuntimeConfig{ApplicationMode: "proactive"}) {
		t.Fatal("personal DWS watcher must start without bot-send capability")
	}
	if runtimeCapabilitiesReady(core.Channel{Kind: core.ChannelDingTalkApp, Capabilities: verifiedDWS}, core.RuntimeConfig{ApplicationMode: "group_mention"}) {
		t.Fatal("bot interaction still requires verified send")
	}
}

func TestReconcileSkipsOutboundDeliveryRoute(t *testing.T) {
	service, cfg, _, _, _ := setupService(t)
	adapter := &historyConversationAdapter{fakeAdapter: fakeAdapter{}}
	service.Adapter = adapter
	channelValue, err := core.ReadChannel(context.Background(), service.Store.DB, cfg.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	service.reconcile(context.Background(), cfg, channelValue)
	if len(adapter.conversations) != 1 || adapter.conversations[0] != "watch" {
		t.Fatalf("reconciled conversations = %v; want only monitored route", adapter.conversations)
	}
}

type historyConversationAdapter struct {
	fakeAdapter
	conversations []string
}

type groupListingAdapter struct {
	fakeAdapter
	groups []channel.GroupConversation
	err    error
}

func (a *groupListingAdapter) ListGroupConversations(context.Context, channel.Config) ([]channel.GroupConversation, error) {
	return a.groups, a.err
}

func TestSyncGroupsRefreshesProcessingSetAndPreservesItOnFailure(t *testing.T) {
	service, cfg, _, _, _ := setupService(t)
	ctx := context.Background()
	channelValue, err := core.ReadChannel(ctx, service.Store.DB, cfg.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &groupListingAdapter{groups: []channel.GroupConversation{{ID: "new-group", Name: "New"}}}
	service.Adapter = adapter
	next := service.syncGroups(ctx, cfg, channelValue)
	if len(next.RouteIDs) != 1 || next.RouteIDs[0] == cfg.RouteIDs[0] {
		t.Fatalf("processing routes were not replaced: old=%v new=%v", cfg.RouteIDs, next.RouteIDs)
	}
	route, err := core.ReadRoute(ctx, service.Store.DB, next.RouteIDs[0])
	if err != nil || route.ConversationID != "new-group" || route.Mode != "collect" {
		t.Fatalf("new managed route=%+v err=%v", route, err)
	}

	adapter.err = core.Fail("unavailable", "temporary lookup failure")
	kept := service.syncGroups(ctx, next, channelValue)
	if len(kept.RouteIDs) != 1 || kept.RouteIDs[0] != next.RouteIDs[0] {
		t.Fatalf("failed lookup changed processing scope: before=%v after=%v", next.RouteIDs, kept.RouteIDs)
	}
}

func (a *historyConversationAdapter) ReadWindow(_ context.Context, _ channel.Config, window channel.Window) (channel.WindowResult, error) {
	a.conversations = append(a.conversations, window.ConversationID)
	return channel.WindowResult{Complete: true}, nil
}

func TestReceiveLoopKeepsStreamOpenUntilRuntimeStops(t *testing.T) {
	service, cfg, _, _, _ := setupService(t)
	c, err := core.ReadChannel(context.Background(), service.Store.DB, cfg.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &persistentReceiverAdapter{deadline: make(chan bool, 1)}
	service.Adapter = adapter
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.receiveLoop(ctx, cfg, c)
	}()
	if hasDeadline := <-adapter.deadline; hasDeadline {
		t.Fatal("live receiver inherited the reconciliation interval as a deadline")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receive loop did not stop after runtime cancellation")
	}
}

// dwsGroupSourceAdapter is a read-only DWS discovery source used by
// group_mention runtimes. It must never be asked to send or receive.
type dwsGroupSourceAdapter struct {
	fakeAdapter
	groups []channel.GroupConversation
	err    error
	calls  int
}

func (a *dwsGroupSourceAdapter) ListGroupConversations(context.Context, channel.Config) ([]channel.GroupConversation, error) {
	a.calls++
	return a.groups, a.err
}

// countingReceiverAdapter counts how many times the application Stream
// receiver is actually started, so a multi-group runtime can be shown to
// share a single lease rather than opening one receiver per group.
type countingReceiverAdapter struct {
	fakeAdapter
	starts int32
}

func (a *countingReceiverAdapter) RunReceiver(ctx context.Context, _ channel.Config, _ channel.ReceiverOptions) error {
	atomic.AddInt32(&a.starts, 1)
	<-ctx.Done()
	return nil
}

// setupGroupMentionService builds a group_mention runtime on an application
// channel bound to a DWS context channel with two active groups, matching
// the shape the CLI wires at `runtime start`: the Stream adapter is the only
// receive/send path, and GroupSource is a separate read-only DWS lister.
func setupGroupMentionService(t *testing.T) (*Service, core.RuntimeConfig, core.Channel, core.Route, core.Route) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	s, err := core.Open(ctx, filepath.Join(home, "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	var dwsChannel core.Channel
	var dwsA core.Route
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "channel.add.dws"}, func(tx *core.Tx) (any, error) {
		var addErr error
		dwsChannel, addErr = tx.AddChannel(ctx, core.ChannelInput{Name: "dws-context", Kind: core.ChannelDwsPersonal,
			Identity: core.ChannelIdentity{ExpectedCorpID: "corp", ExpectedUserID: "owner"},
			Route:    &core.RouteInput{ConversationID: "cid:group-a", ConversationType: "group"}})
		if addErr == nil {
			dwsA = dwsChannel.Routes[0]
		}
		return dwsChannel, addErr
	})
	if err != nil {
		t.Fatal(err)
	}
	var dwsB core.Route
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "channel.add.dws.b"}, func(tx *core.Tx) (any, error) {
		var addErr error
		dwsB, addErr = tx.AddRoute(ctx, dwsChannel.ID, core.RouteInput{ConversationID: "cid:group-b", ConversationType: "group"})
		return dwsB, addErr
	})
	if err != nil {
		t.Fatal(err)
	}

	var appChannel core.Channel
	var appA, ownerRoute core.Route
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "channel.add.app"}, func(tx *core.Tx) (any, error) {
		var addErr error
		appChannel, addErr = tx.AddChannel(ctx, core.ChannelInput{Name: "group-app", Kind: core.ChannelDingTalkApp,
			Identity: core.ChannelIdentity{ExpectedCorpID: dwsChannel.Tenant, ClientID: "group-app", RobotCode: "group-bot", HistoryChannel: dwsChannel.ID}})
		return appChannel, addErr
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "route.add.app.a"}, func(tx *core.Tx) (any, error) {
		var addErr error
		appA, addErr = tx.AddRoute(ctx, appChannel.ID, core.RouteInput{ConversationID: dwsA.ConversationID, ConversationType: "group",
			Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		return appA, addErr
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "route.add.app.owner"}, func(tx *core.Tx) (any, error) {
		var addErr error
		ownerRoute, addErr = tx.AddRoute(ctx, appChannel.ID, core.RouteInput{ConversationID: "cid:owner-app", ConversationType: "direct", Mode: "notify", SendPolicy: "dispatch_only"})
		return ownerRoute, addErr
	})
	if err != nil {
		t.Fatal(err)
	}
	owner := core.Sender{IDType: "staff_id", IDValue: "owner-1"}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "owner.intake"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, appChannel.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "history", ProviderMessageID: "owner-bootstrap", ConversationID: ownerRoute.ConversationID, Tenant: appChannel.Tenant, Sender: owner, Body: "bootstrap", SentAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "channel.capabilities.app"}, func(tx *core.Tx) (any, error) {
		return tx.SetChannelCapabilities(ctx, appChannel.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "send": true}}, "fake")
	})
	if err != nil {
		t.Fatal(err)
	}

	var cfg core.RuntimeConfig
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "runtime.configure"}, func(tx *core.Tx) (any, error) {
		var configErr error
		cfg, configErr = tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Name: "group-helper", Channel: appChannel.ID, RouteIDs: []string{appA.ID},
			DeliveryRouteID: appA.ID, Owner: owner, ApplicationMode: "group_mention", ContextChannel: dwsChannel.ID, ReconcileSeconds: 10})
		return cfg, configErr
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "runtime.start"}, func(tx *core.Tx) (any, error) { return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "") })
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = core.ReadRuntime(ctx, s.DB, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	logger, err := runlog.Open(home, cfg.ID, io.Discard, runlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	service := &Service{Home: home, Store: s, Adapter: &fakeAdapter{}, Logger: logger}
	return service, cfg, appChannel, dwsA, dwsB
}

// A group_mention runtime discovers new groups through its DWS GroupSource,
// never through the application Stream adapter, and folds them into
// route_ids as assistant routes rather than starting a second process.
func TestSyncGroupsUsesDWSSourceForGroupMentionRuntime(t *testing.T) {
	service, cfg, appChannel, dwsA, dwsB := setupGroupMentionService(t)
	ctx := context.Background()
	source := &dwsGroupSourceAdapter{groups: []channel.GroupConversation{{ID: dwsA.ConversationID}, {ID: dwsB.ConversationID}}}
	service.GroupSource = source
	appChannelValue, err := core.ReadChannel(ctx, service.Store.DB, appChannel.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := service.syncGroups(ctx, cfg, appChannelValue)
	if source.calls != 1 {
		t.Fatalf("group source was not consulted exactly once: %d", source.calls)
	}
	if len(next.RouteIDs) != 2 {
		t.Fatalf("group_mention sync did not adopt both active groups: %+v", next.RouteIDs)
	}
	newRoute, err := core.RouteFor(ctx, service.Store.DB, appChannel.ID, dwsB.ConversationID)
	if err != nil || newRoute.Mode != "assistant" || newRoute.SendPolicy != "reply_to_trigger" {
		t.Fatalf("new group route did not get the fixed assistant policy: %+v err=%v", newRoute, err)
	}

	// A failed DWS lookup must preserve the previously adopted scope rather
	// than shrinking it, and must not touch the Stream adapter at all.
	source.err = core.Fail("unavailable", "temporary dws lookup failure")
	fakeStream, ok := service.Adapter.(*fakeAdapter)
	if !ok {
		t.Fatal("expected the Stream adapter to remain a plain fake")
	}
	kept := service.syncGroups(ctx, next, appChannelValue)
	if len(kept.RouteIDs) != len(next.RouteIDs) {
		t.Fatalf("failed DWS lookup changed the processing scope: before=%v after=%v", next.RouteIDs, kept.RouteIDs)
	}
	if fakeStream.sends != 0 {
		t.Fatalf("group discovery must never call the application Stream adapter's send path: sends=%d", fakeStream.sends)
	}
}

// Two DWS-active groups mounted on the same group_mention runtime must share
// the single application channel receive lease: exactly one Stream receiver
// starts, never one per group.
func TestGroupMentionRuntimeSharesOneReceiverAcrossTwoGroups(t *testing.T) {
	service, cfg, appChannel, dwsA, dwsB := setupGroupMentionService(t)
	ctx := context.Background()
	source := &dwsGroupSourceAdapter{groups: []channel.GroupConversation{{ID: dwsA.ConversationID}, {ID: dwsB.ConversationID}}}
	service.GroupSource = source
	appChannelValue, err := core.ReadChannel(ctx, service.Store.DB, appChannel.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg = service.syncGroups(ctx, cfg, appChannelValue)
	if len(cfg.RouteIDs) != 2 {
		t.Fatalf("expected both groups adopted before starting the receiver: %+v", cfg.RouteIDs)
	}
	receiver := &countingReceiverAdapter{}
	service.Adapter = receiver
	receiveCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.receiveLoop(receiveCtx, cfg, appChannelValue)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("receive loop did not stop after cancellation")
	}
	if atomic.LoadInt32(&receiver.starts) != 1 {
		t.Fatalf("expected exactly one receiver for a two-group runtime, got %d", receiver.starts)
	}
}
