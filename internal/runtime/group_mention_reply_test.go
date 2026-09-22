package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/channel/dingtalkapp"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

type botConversationCase struct {
	Name   string          `json:"name"`
	Owner  bool            `json:"owner"`
	Frame  json.RawMessage `json:"frame"`
	Expect struct {
		ConversationType string `json:"conversation_type"`
		Mentioned        bool   `json:"mentioned"`
		Triggered        bool   `json:"triggered"`
		RuntimeReply     bool   `json:"runtime_reply"`
	} `json:"expect"`
}

func loadBotConversationCases(t *testing.T) map[string]botConversationCase {
	t.Helper()
	raw, err := os.ReadFile("../channel/dingtalkapp/testdata/bot_conversation_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []botConversationCase
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	result := make(map[string]botConversationCase, len(cases))
	for _, item := range cases {
		result[item.Name] = item
	}
	return result
}

func parseBotConversationCase(t *testing.T, item botConversationCase) core.NormalizedEvent {
	t.Helper()
	event, err := dingtalkapp.ParseFrame(channel.Config{Tenant: "corp", Identity: core.ChannelIdentity{ExpectedCorpID: "corp", RobotCode: "fixture-bot"}}, item.Frame)
	if err != nil {
		t.Fatal(err)
	}
	if event.ConversationType != item.Expect.ConversationType || event.Mentioned != item.Expect.Mentioned || dingtalkapp.Triggered(event) != item.Expect.Triggered {
		t.Fatalf("fixture expectation differs from parser: event=%+v expect=%+v", event, item.Expect)
	}
	event.SentAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	event.EventAt = event.SentAt
	return event
}

func TestDingTalkBotConversationFixturesCoverGroupAndPrivateReplies(t *testing.T) {
	cases := loadBotConversationCases(t)

	t.Run("group mention replies with visible and native requester mention", func(t *testing.T) {
		item := cases["group_mention"]
		event := parseBotConversationCase(t, item)
		s, cfg, app, group, _ := setupGroupMentionService(t)
		if event.ConversationID != group.ConversationID || !item.Expect.RuntimeReply {
			t.Fatalf("group fixture no longer matches runtime route: event=%s route=%s", event.ConversationID, group.ConversationID)
		}
		models := &fakeModels{}
		s.Analyzer, s.Executor = models, models
		preset, err := agent.Enable(context.Background(), s.Home, "claude", "claude-default")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.mutate(context.Background(), "global", "fixture.group.intake", func(tx *core.Tx) (any, error) {
			return tx.Intake(context.Background(), app.ID, event)
		}); err != nil {
			t.Fatal(err)
		}
		s.tick(context.Background(), cfg, preset)
		adapter := s.Adapter.(*fakeAdapter)
		if models.analyses != 0 || models.executes != 1 || len(adapter.requests) != 1 {
			t.Fatalf("group fixture routing: analysis=%d execution=%d requests=%+v", models.analyses, models.executes, adapter.requests)
		}
		var card core.RuntimeCard
		if err = json.Unmarshal([]byte(adapter.requests[0].Content), &card); err != nil {
			t.Fatal(err)
		}
		if adapter.requests[0].Format != "group_markdown" || len(card.Mentions) != 1 || card.Mentions[0].IDType != "user_id" || card.Mentions[0].IDValue != event.Sender.IDValue || !strings.HasSuffix(card.Text, "\n\n@测试提问人") {
			t.Fatalf("group reply did not @ its requester: request=%+v card=%+v", adapter.requests[0], card)
		}
	})

	t.Run("verified owner private chat replies", func(t *testing.T) {
		item := cases["owner_private"]
		event := parseBotConversationCase(t, item)
		if !item.Owner || !item.Expect.RuntimeReply {
			t.Fatal("owner private fixture lost its authorization expectation")
		}
		runDirectReplyScenario(t, event.Body, false, nil, false)
	})

	t.Run("ordinary user private chat stays silent", func(t *testing.T) {
		owner := parseBotConversationCase(t, cases["owner_private"])
		scenario := runDirectReplyScenario(t, owner.Body, false, nil, false)
		foreignCase := cases["ordinary_user_private"]
		foreign := parseBotConversationCase(t, foreignCase)
		if foreignCase.Owner || foreignCase.Expect.RuntimeReply {
			t.Fatal("ordinary private fixture unexpectedly authorizes a reply")
		}
		beforeExec, beforeSend, beforeReceipts := scenario.executor.calls, scenario.adapter.sends, scenario.adapter.receipts
		if err := scenario.service.mutate(context.Background(), "global", "fixture.foreign-private.intake", func(tx *core.Tx) (any, error) {
			return tx.Intake(context.Background(), scenario.cfg.ChannelID, foreign)
		}); err != nil {
			t.Fatal(err)
		}
		scenario.service.tick(context.Background(), scenario.cfg, scenario.preset)
		if scenario.executor.calls != beforeExec || scenario.adapter.sends != beforeSend || scenario.adapter.receipts != beforeReceipts {
			t.Fatalf("ordinary private message ran or replied: executions=%d sends=%d receipts=%d", scenario.executor.calls-beforeExec, scenario.adapter.sends-beforeSend, scenario.adapter.receipts-beforeReceipts)
		}
	})
}

func TestGroupMentionsReplyToGreetingsWithoutClassifier(t *testing.T) {
	s, cfg, app, _, groupB := setupGroupMentionService(t)
	ctx := context.Background()
	// Exercise a group other than the default delivery group.
	var route core.Route
	err := s.mutate(ctx, "global", "groups.sync", func(tx *core.Tx) (any, error) {
		var err error
		cfg, _, err = tx.SyncGroupMentionRoutes(ctx, cfg.ID, []string{"cid:group-a", groupB.ConversationID})
		return cfg, err
	})
	if err != nil {
		t.Fatal(err)
	}
	route, err = core.RouteFor(ctx, s.Store.DB, app.ID, groupB.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	models := &fakeModels{}
	adapter := s.Adapter.(*fakeAdapter)
	executor := &mentionCheckingExecutor{models: models, before: func(input ExecutionInput) {
		if len(adapter.reactions) == 0 {
			t.Fatalf("Agent started without acknowledgement: %+v", adapter.requests)
		}
		last := adapter.reactions[len(adapter.reactions)-1]
		if last.Emoji != core.RuntimeProcessingAcknowledgement || last.ConversationID != route.ConversationID || last.MessageID != input.Task.Messages[0].ProviderMessageID {
			t.Fatalf("processing acknowledgement used wrong group or trigger: %+v", last)
		}
	}}
	s.Analyzer, s.Executor, s.Reviewer = models, executor, models
	preset, err := agent.Enable(ctx, s.Home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		id        string
		mentioned bool
		body      string
	}{{"ordinary", false, "在吗"}, {"greeting-1", true, "在吗"}, {"greeting-2", true, "在吗"}, {"long", true, strings.Repeat("long request ", 100)}} {
		err = s.mutate(ctx, "global", "message.intake", func(tx *core.Tx) (any, error) {
			return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: event.id,
				ConversationID: route.ConversationID, ConversationType: "group", Tenant: app.Tenant,
				Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: event.mentioned, Body: event.body, SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		s.tick(ctx, cfg, preset)
	}
	tasks, err := core.RuntimeTaskList(ctx, s.Store.DB, cfg.ID, "", 10)
	if err != nil || len(tasks) != 3 {
		t.Fatalf("mention tasks=%+v err=%v", tasks, err)
	}
	if models.analyses != 0 || models.executes != 3 || s.Adapter.(*fakeAdapter).sends != 3 {
		t.Fatalf("analysis=%d executions=%d sends=%d tasks=%+v", models.analyses, models.executes, s.Adapter.(*fakeAdapter).sends, tasks)
	}
	for _, task := range tasks {
		if task.Status != "completed" || task.RouteID != route.ID {
			t.Fatalf("reply task=%+v", task)
		}
		full, err := core.ReadRuntimeTask(ctx, s.Store.DB, task.ID)
		if err != nil || len(full.Messages) != 1 || !full.Messages[0].Addressed || len([]rune(full.Title)) > 120 || full.AnalysisModel != "local-mention-routing" {
			t.Fatalf("original mention=%+v err=%v", full, err)
		}
		if strings.HasPrefix(full.Messages[0].Body, "long request") && full.Messages[0].Body != strings.Repeat("long request ", 100) {
			t.Fatal("long original message was truncated")
		}
		var state string
		if err := s.Store.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE job_id=? AND reason='result'", task.ID).Scan(&state); err != nil || state != "accepted" {
			t.Fatalf("delivery=%q err=%v", state, err)
		}
		for _, purpose := range []string{core.RuntimeReceiptPurpose, core.RuntimeProcessingReceiptPurpose, core.RuntimeCompletionReceiptPurpose} {
			if err := s.Store.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE job_id=? AND reason=?", task.ID, purpose).Scan(&state); err != nil || state != "accepted" {
				t.Fatalf("stage %s=%q err=%v", purpose, state, err)
			}
		}
	}
	if adapter.receipts != 9 {
		t.Fatalf("ordinary message received an acknowledgement: %d", adapter.receipts)
	}
	if len(adapter.removed) != 6 {
		t.Fatalf("stage reactions did not replace their predecessors: %+v", adapter.removed)
	}
	s.tick(ctx, cfg, preset)
	if adapter.receipts != 9 || adapter.sends != 3 {
		t.Fatal("idle tick resent acknowledgement or answer")
	}
	if tasks[0].CanonicalKey == tasks[1].CanonicalKey {
		t.Fatal("repeated greetings merged")
	}
}

// The callback observes transport before the model is invoked.
type mentionCheckingExecutor struct {
	models *fakeModels
	before func(ExecutionInput)
}

func (e *mentionCheckingExecutor) Execute(ctx context.Context, input ExecutionInput) (core.RuntimeAttemptResult, error) {
	e.before(input)
	return e.models.Execute(ctx, input)
}

type failingMentionExecutor struct{}

func (*failingMentionExecutor) Execute(context.Context, ExecutionInput) (core.RuntimeAttemptResult, error) {
	return core.RuntimeAttemptResult{}, core.Fail("internal", "execution failed")
}

func TestFailedGroupMentionReplacesAcknowledgementAndRecordsOutcome(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	models := &fakeModels{}
	s.Analyzer, s.Executor, s.Reviewer = models, &failingMentionExecutor{}, models
	preset, err := agent.Enable(ctx, s.Home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.mutate(ctx, "global", "failure-receipt.intake", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "failed-question", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "执行后失败", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx, cfg, preset)
	tasks, err := core.RuntimeTaskList(ctx, s.Store.DB, cfg.ID, "failed", 10)
	if err != nil || len(tasks) != 1 || tasks[0].ErrorCode != "internal" {
		t.Fatalf("failed task=%+v err=%v", tasks, err)
	}
	adapter := s.Adapter.(*fakeAdapter)
	if len(adapter.reactions) != 3 || adapter.reactions[0].Emoji != core.RuntimeAcknowledgement || adapter.reactions[1].Emoji != core.RuntimeProcessingAcknowledgement || adapter.reactions[2].Emoji != core.RuntimeFailureAcknowledgement {
		t.Fatalf("failure reactions=%+v", adapter.reactions)
	}
	if len(adapter.removed) != 2 || adapter.removed[0].Emoji != core.RuntimeAcknowledgement || adapter.removed[1].Emoji != core.RuntimeProcessingAcknowledgement || adapter.removed[0].ConversationID != group.ConversationID || adapter.removed[0].MessageID != "failed-question" {
		t.Fatalf("removed acknowledgement=%+v", adapter.removed)
	}
	rows, err := s.Store.DB.QueryContext(ctx, "SELECT reason,state FROM outbox WHERE job_id=? ORDER BY created_at", tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var reason, state string
		if err = rows.Scan(&reason, &state); err != nil {
			t.Fatal(err)
		}
		states[reason] = state
	}
	if states[core.RuntimeReceiptPurpose] != "accepted" || states[core.RuntimeProcessingReceiptPurpose] != "accepted" || states[core.RuntimeFailureReceiptPurpose] != "accepted" {
		t.Fatalf("receipt states=%+v", states)
	}
}

func TestFailedGroupMentionStillAddsFailureMarkWhenReceiptRemovalFails(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	models := &fakeModels{}
	s.Analyzer, s.Executor, s.Reviewer = models, &failingMentionExecutor{}, models
	adapter := s.Adapter.(*fakeAdapter)
	adapter.removeErr = core.Fail("unavailable", "receipt removal failed")
	preset, err := agent.Enable(ctx, s.Home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.mutate(ctx, "global", "failure-receipt.partial.intake", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "failed-question-partial", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "执行后失败且接收标记无法移除", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx, cfg, preset)
	if len(adapter.reactions) != 3 || adapter.reactions[2].Emoji != core.RuntimeFailureAcknowledgement {
		t.Fatalf("failure mark was not attempted after cleanup failure: %+v", adapter.reactions)
	}
	var state string
	if err = s.Store.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE reason=?", core.RuntimeFailureReceiptPurpose).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("partial replacement was not visible as unknown: state=%q err=%v", state, err)
	}
}

func TestGroupActionUnknownReportsExistingResultInsteadOfOnlyFailureMark(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	var task core.RuntimeTask
	var attempt core.RuntimeAttempt
	if err := s.mutate(ctx, "global", "failure-result.intake", func(tx *core.Tx) (any, error) {
		if _, err := tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "failure-result-question", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester", DisplayName: "发起人"}, Mentioned: true, Body: "查询后继续处理", SentAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
			return nil, err
		}
		if _, err := tx.SyncRuntimeMessages(ctx, cfg.ID); err != nil {
			return nil, err
		}
		batch, err := tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now().Add(time.Minute))
		if err != nil {
			return nil, err
		}
		tasks, err := tx.CompleteRuntimeBatch(ctx, batch, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "failure-result", Title: "查询后继续处理", Instructions: "先查询再处理", MessageIDs: []string{batch.Messages[0].ID}}}})
		if err != nil {
			return nil, err
		}
		task, attempt, err = tx.ClaimRuntimeTask(ctx, cfg.ID, core.NewID(), "fake", "preset", "commit", "")
		return tasks, err
	}); err != nil {
		t.Fatal(err)
	}
	s.acknowledge(ctx, cfg, task.ID)
	if err := s.mutate(ctx, "global", "failure-result.complete", func(tx *core.Tx) (any, error) {
		var err error
		task, err = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, core.RuntimeAttemptResult{Result: "已经查到可反馈的结论。", Summary: "查询已有结论", Actions: []core.RuntimeAction{{Kind: "external_change", Target: "target", Payload: "continue"}}}, "")
		return task, err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.mutate(ctx, "global", "failure-result.confirm", func(tx *core.Tx) (any, error) {
		if _, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='confirmed',confirmed_by='owner',confirmation_origin='test' WHERE task_id=?", task.ID); err != nil {
			return nil, err
		}
		action, attempt, err := tx.ClaimRuntimeAction(ctx, cfg.ID, core.NewID(), "fake")
		if err != nil {
			return nil, err
		}
		return tx.UnknownRuntimeAction(ctx, action.ID, attempt.ID, action.TaskVersion, "runtime_restarted")
	}); err != nil {
		t.Fatal(err)
	}
	s.failureStageAcknowledgement(ctx, cfg, task.ID)
	s.reconcileFailureNotices(ctx, cfg)
	adapter := s.Adapter.(*fakeAdapter)
	if len(adapter.requests) != 1 || adapter.requests[0].ConversationID != group.ConversationID || adapter.requests[0].ReplyTo != "failure-result-question" || adapter.requests[0].Format != "group_markdown" {
		t.Fatalf("failure result was not replied to the triggering group: %+v", adapter.requests)
	}
	var card core.RuntimeCard
	if err := json.Unmarshal([]byte(adapter.requests[0].Content), &card); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card.Text, "已经查到可反馈的结论") || !strings.Contains(card.Text, "最终状态无法确认") || len(card.Mentions) != 1 || card.Mentions[0].IDValue != "requester" {
		t.Fatalf("failure result did not preserve conclusion and uncertainty: %+v", card)
	}
	if len(adapter.reactions) != 2 || adapter.reactions[0].Emoji != core.RuntimeAcknowledgement || adapter.reactions[1].Emoji != core.RuntimeFailureAcknowledgement {
		t.Fatalf("unknown action lost its truthful failure marker: %+v", adapter.reactions)
	}
	s.reconcileFailureNotices(ctx, cfg)
	if len(adapter.requests) != 1 {
		t.Fatal("failure result was sent more than once")
	}
}

func TestRuntimeStartReconcilesFailureMarkAfterRecoveryCrash(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	models := &fakeModels{}
	s.Analyzer, s.Executor, s.Reviewer = models, models, models
	s.Tick = time.Hour
	if _, err := agent.Enable(ctx, s.Home, "claude", "claude-default"); err != nil {
		t.Fatal(err)
	}
	if err := s.mutate(ctx, "global", "failure-receipt.recovery.intake", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "interrupted-question", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "执行期间服务重启", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	var task core.RuntimeTask
	if err := s.mutate(ctx, "global", "failure-receipt.recovery.prepare", func(tx *core.Tx) (any, error) {
		if _, err := tx.SyncRuntimeMessages(ctx, cfg.ID); err != nil {
			return nil, err
		}
		batch, err := tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now().Add(time.Minute))
		if err != nil {
			return nil, err
		}
		tasks, err := tx.CompleteRuntimeBatch(ctx, batch, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "interrupted-group-task", Title: "执行期间服务重启", Instructions: "处理群请求", MessageIDs: []string{batch.Messages[0].ID}}}})
		if err != nil {
			return nil, err
		}
		task, _, err = tx.ClaimRuntimeTask(ctx, cfg.ID, core.NewID(), "fake", "preset", "commit", "")
		return tasks, err
	}); err != nil {
		t.Fatal(err)
	}
	s.acknowledge(ctx, cfg, task.ID)
	if err := s.mutate(ctx, "global", "failure-receipt.recovery.crash-window", func(tx *core.Tx) (any, error) {
		return tx.RecoverRuntime(ctx, cfg.ID)
	}); err != nil {
		t.Fatal(err)
	}
	// The worker that owned the running attempt disappeared with the simulated
	// service crash. Do not let the current test process look like a competing
	// healthy service during startup recovery.
	if _, err := s.Store.DB.ExecContext(ctx, "UPDATE runtime_work_leases SET owner_pid=0,owner_started='',process_pid=0,process_started='' WHERE runtime_id=? AND released=0", cfg.ID); err != nil {
		t.Fatal(err)
	}
	before, err := core.ReadRuntimeTask(ctx, s.Store.DB, task.ID)
	if err != nil || before.Status != "failed" || before.ErrorCode != "runtime_restarted" {
		t.Fatalf("pre-restart recovery task=%+v err=%v", before, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx, cfg.ID) }()
	receiverEventually(t, func() bool {
		var receiptState, noticeState string
		receiptErr := s.Store.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE job_id=? AND reason=?", task.ID, core.RuntimeFailureReceiptPurpose).Scan(&receiptState)
		noticeErr := s.Store.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE job_id=? AND reason=?", task.ID, core.RuntimeFailureNoticePurpose).Scan(&noticeState)
		return receiptErr == nil && receiptState == "accepted" && noticeErr == nil && noticeState == "accepted"
	})
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not stop after recovery verification")
	}
	after, err := core.ReadRuntimeTask(ctx, s.Store.DB, task.ID)
	if err != nil || after.Status != "failed" || after.ErrorCode != "runtime_restarted" {
		t.Fatalf("recovered task=%+v err=%v", after, err)
	}
	adapter := s.Adapter.(*fakeAdapter)
	if len(adapter.reactions) != 2 || adapter.reactions[1].Emoji != core.RuntimeFailureAcknowledgement || len(adapter.removed) != 1 {
		t.Fatalf("recovery reactions=%+v removed=%+v", adapter.reactions, adapter.removed)
	}
	if len(adapter.requests) != 1 || adapter.requests[0].ConversationID != group.ConversationID || adapter.requests[0].ReplyTo != "interrupted-question" || adapter.requests[0].Format != "group_markdown" {
		t.Fatalf("recovery failure was not explained in the triggering group: %+v", adapter.requests)
	}
	var card core.RuntimeCard
	if err = json.Unmarshal([]byte(adapter.requests[0].Content), &card); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card.Text, "错误代码：`runtime_restarted`") || !strings.Contains(card.Text, "服务现已恢复") || len(card.Mentions) != 1 || card.Mentions[0].IDValue != "requester" {
		t.Fatalf("recovery failure notice was not actionable: %+v", card)
	}
}

func TestRuntimeStartReconcilesCompletionMarkAfterStateCommitCrash(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	models := &fakeModels{}
	s.Analyzer, s.Executor, s.Reviewer = models, models, models
	s.Tick = time.Hour
	if _, err := agent.Enable(ctx, s.Home, "claude", "claude-default"); err != nil {
		t.Fatal(err)
	}
	if err := s.mutate(ctx, "global", "completion-receipt.recovery.intake", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "completed-question", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "完成后服务崩溃", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	var task core.RuntimeTask
	var attempt core.RuntimeAttempt
	if err := s.mutate(ctx, "global", "completion-receipt.recovery.prepare", func(tx *core.Tx) (any, error) {
		if _, err := tx.SyncRuntimeMessages(ctx, cfg.ID); err != nil {
			return nil, err
		}
		batch, err := tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now().Add(time.Minute))
		if err != nil {
			return nil, err
		}
		if _, err = tx.CompleteRuntimeBatch(ctx, batch, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "completed-group-task", Title: "完成后服务崩溃", Instructions: "处理群请求", MessageIDs: []string{batch.Messages[0].ID}}}}); err != nil {
			return nil, err
		}
		task, attempt, err = tx.ClaimRuntimeTask(ctx, cfg.ID, core.NewID(), "fake", "preset", "commit", "")
		return task, err
	}); err != nil {
		t.Fatal(err)
	}
	s.acknowledge(ctx, cfg, task.ID)
	s.processingAcknowledgement(ctx, cfg, task.ID)
	if err := s.mutate(ctx, "global", "completion-receipt.recovery.crash-window", func(tx *core.Tx) (any, error) {
		return tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, core.RuntimeAttemptResult{Result: "已完成", Summary: "已完成"}, "")
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx, cfg.ID) }()
	receiverEventually(t, func() bool {
		var state string
		err := s.Store.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE job_id=? AND reason=?", task.ID, core.RuntimeCompletionReceiptPurpose).Scan(&state)
		return err == nil && state == "accepted"
	})
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not stop after completion recovery verification")
	}
	adapter := s.Adapter.(*fakeAdapter)
	if len(adapter.reactions) != 3 || adapter.reactions[2].Emoji != core.RuntimeCompletionAcknowledgement || len(adapter.removed) != 2 || adapter.removed[1].Emoji != core.RuntimeProcessingAcknowledgement {
		t.Fatalf("completion recovery reactions=%+v removed=%+v", adapter.reactions, adapter.removed)
	}
}

type confirmationFailAdapter struct{ fakeAdapter }

func (a *confirmationFailAdapter) Send(ctx context.Context, cfg channel.Config, req channel.SendRequest) (channel.SendResult, error) {
	if req.Transport == "bot_dm" {
		a.requests = append(a.requests, req)
		return channel.SendResult{State: "failed"}, core.Fail("denied", "private delivery rejected")
	}
	return a.fakeAdapter.Send(ctx, cfg, req)
}

func TestPendingGroupReplyAndConfirmationStayInOriginGroup(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	adapter := &confirmationFailAdapter{}
	s.Adapter = adapter
	analyzer := &contextOnlyAnalyzer{}
	executor := &modelReportingExecutor{actions: []core.RuntimeAction{{Kind: "infra_change", Target: "test-infra", Payload: "change capacity"}}}
	s.Analyzer, s.Executor = analyzer, executor
	preset, err := agent.Enable(ctx, s.Home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	err = s.mutate(ctx, "global", "mention.pending.intake", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "pending-question", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "请调整容量", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	s.tick(ctx, cfg, preset)
	tasks, err := core.RuntimeTaskList(ctx, s.Store.DB, cfg.ID, "awaiting_confirmation", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("pending task=%+v err=%v", tasks, err)
	}
	full, err := core.ReadRuntimeTask(ctx, s.Store.DB, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	token := core.ConfirmationToken(full.Actions[0])
	if analyzer.calls != 0 || executor.calls != 1 || len(adapter.requests) != 1 {
		t.Fatalf("unexpected routing or send count: %+v", adapter.requests)
	}
	answer := adapter.requests[0]
	if answer.Transport != "bot_group" || answer.ConversationID != group.ConversationID || answer.ReplyTo != "pending-question" || !strings.Contains(answer.Content, full.Result) || !strings.Contains(answer.Content, token) || !strings.Contains(answer.Content, "change capacity") {
		t.Fatalf("group answer or confirmation preview missing: %+v", answer)
	}
	var resultState string
	if err = s.Store.DB.QueryRow("SELECT state FROM outbox WHERE job_id=? AND reason='result'", full.ID).Scan(&resultState); err != nil {
		t.Fatal(err)
	}
	var privateCount int
	if err = s.Store.DB.QueryRow("SELECT count(*) FROM outbox WHERE job_id=? AND transport='bot_dm'", full.ID).Scan(&privateCount); err != nil {
		t.Fatal(err)
	}
	if resultState != "accepted" || privateCount != 0 || full.Actions[0].Status != "pending" {
		t.Fatalf("group pending delivery: state=%s private=%d action=%s", resultState, privateCount, full.Actions[0].Status)
	}
	if len(adapter.reactions) != 2 || adapter.reactions[1].Emoji != core.RuntimeProcessingAcknowledgement {
		t.Fatalf("approval wait did not remain processing: %+v", adapter.reactions)
	}
	var completionCount int
	if err = s.Store.DB.QueryRow("SELECT count(*) FROM outbox WHERE job_id=? AND reason=?", full.ID, core.RuntimeCompletionReceiptPurpose).Scan(&completionCount); err != nil || completionCount != 0 {
		t.Fatalf("approval wait was marked completed: count=%d err=%v", completionCount, err)
	}
	s.deliver(ctx, cfg, full.ID)
	if len(adapter.requests) != 1 {
		t.Fatal("repeated delivery resent the group reply")
	}
}

type acknowledgementOutcomeAdapter struct {
	fakeAdapter
	state          string
	added, removed []string
}

func (a *acknowledgementOutcomeAdapter) AddReaction(ctx context.Context, cfg channel.Config, req channel.ReactionRequest) error {
	a.added = append(a.added, req.Emoji)
	if err := a.fakeAdapter.AddReaction(ctx, cfg, req); err != nil {
		return err
	}
	if a.state == "failed" {
		return core.Fail("denied", "reaction rejected")
	}
	if a.state == "unknown" {
		return core.Fail("unavailable", "reaction result unknown")
	}
	return nil
}
func (a *acknowledgementOutcomeAdapter) RemoveReaction(_ context.Context, _ channel.Config, req channel.ReactionRequest) error {
	a.removed = append(a.removed, req.Emoji)
	return nil
}

func TestAcknowledgementReactionOutcomeDoesNotBlockAgentOrRepeat(t *testing.T) {
	for _, state := range []string{"accepted", "failed", "unknown"} {
		t.Run(state, func(t *testing.T) {
			s, cfg, app, group, _ := setupGroupMentionService(t)
			ctx := context.Background()
			adapter := &acknowledgementOutcomeAdapter{state: state}
			s.Adapter = adapter
			models := &fakeModels{}
			s.Analyzer, s.Executor = models, models
			preset, err := agent.Enable(ctx, s.Home, "claude", "claude-default")
			if err != nil {
				t.Fatal(err)
			}
			err = s.mutate(ctx, "global", "ack.intake", func(tx *core.Tx) (any, error) {
				return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "ack-question", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "在吗", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
			})
			if err != nil {
				t.Fatal(err)
			}
			s.tick(ctx, cfg, preset)
			tasks, err := core.RuntimeTaskList(ctx, s.Store.DB, cfg.ID, "completed", 10)
			if err != nil || len(tasks) != 1 || models.analyses != 0 || models.executes != 1 || adapter.receipts != 3 || adapter.sends != 1 {
				t.Fatalf("ack blocked Agent: tasks=%+v analysis=%d execution=%d receipts=%d answers=%d err=%v", tasks, models.analyses, models.executes, adapter.receipts, adapter.sends, err)
			}
			var saved string
			if err = s.Store.DB.QueryRow("SELECT state FROM outbox WHERE job_id=? AND reason='runtime_receipt'", tasks[0].ID).Scan(&saved); err != nil || saved != state {
				t.Fatalf("receipt state=%s err=%v", saved, err)
			}
			s.acknowledge(ctx, cfg, tasks[0].ID)
			s.tick(ctx, cfg, preset)
			expectedRemoved := 0
			if state == "accepted" {
				expectedRemoved = 2
			}
			if adapter.receipts != 3 || adapter.sends != 1 || len(adapter.added) != 3 || adapter.added[0] != core.RuntimeAcknowledgement || adapter.added[1] != core.RuntimeProcessingAcknowledgement || adapter.added[2] != core.RuntimeCompletionAcknowledgement || len(adapter.removed) != expectedRemoved {
				t.Fatalf("repeated delivery or incorrect receipt reaction: %+v", adapter)
			}
		})
	}
}

// Expose the normal adapter API without the optional reaction API.
type adapterWithoutReactions struct{ channel.Adapter }

func TestUnsupportedAcknowledgementDoesNotFallBackToText(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	adapter := s.Adapter.(*fakeAdapter)
	s.Adapter = &adapterWithoutReactions{Adapter: adapter}
	models := &fakeModels{}
	s.Analyzer, s.Executor = models, models
	preset, err := agent.Enable(ctx, s.Home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	err = s.mutate(ctx, "global", "ack.unsupported.intake", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "unsupported-question", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "在吗", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	s.tick(ctx, cfg, preset)
	if models.analyses != 0 || models.executes != 1 || adapter.receipts != 0 || len(adapter.requests) != 1 || adapter.requests[0].Format != "group_markdown" {
		t.Fatalf("unsupported reaction blocked Agent or sent text receipt: %+v", adapter)
	}
	var receipts int
	if err = s.Store.DB.QueryRow("SELECT count(*) FROM outbox WHERE reason='runtime_receipt'").Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("unexpected receipt: count=%d err=%v", receipts, err)
	}
}

func TestPreparedAcknowledgementCannotChangeGroupOrIgnoreWithdrawal(t *testing.T) {
	s, cfg, app, group, other := setupGroupMentionService(t)
	ctx := context.Background()
	var task core.RuntimeTask
	var out core.OutboxView
	err := s.mutate(ctx, "global", "ack.prepare", func(tx *core.Tx) (any, error) {
		msg, err := tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "ack-route", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "回答问题", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SyncRuntimeMessages(ctx, cfg.ID); err != nil {
			return nil, err
		}
		batch, err := tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now().Add(time.Hour))
		if err != nil {
			return nil, err
		}
		tasks, err := tx.CompleteRuntimeBatch(ctx, batch, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "mention:" + msg.MessageID, Title: "回答", Instructions: "回答关联的群消息", MessageIDs: []string{msg.MessageID}}}})
		if err != nil {
			return nil, err
		}
		task = tasks[0]
		out, err = tx.PrepareTaskAcknowledgement(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		repeated, err := tx.PrepareTaskAcknowledgement(ctx, task.ID)
		if err == nil && repeated.ID != out.ID {
			t.Fatal("receipt prepare not idempotent")
		}
		return out, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Format != "reaction" || out.Content != core.RuntimeAcknowledgement || out.ReplyTo != "ack-route" {
		t.Fatalf("receipt was not a reaction: %+v", out)
	}
	err = s.mutate(ctx, "global", "ack.reject-text-format", func(tx *core.Tx) (any, error) {
		_, e := tx.Conn.ExecContext(ctx, "UPDATE outbox SET format='markdown' WHERE id=?", out.ID)
		return nil, e
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.mutate(ctx, "global", "ack.begin-text-format", func(tx *core.Tx) (any, error) { return tx.BeginDelivery(ctx, out.ID) })
	if core.ErrorCode(err) != "conflict" {
		t.Fatalf("receipt converted to text: %v", err)
	}
	err = s.mutate(ctx, "global", "ack.restore-format", func(tx *core.Tx) (any, error) {
		_, e := tx.Conn.ExecContext(ctx, "UPDATE outbox SET format='reaction' WHERE id=?", out.ID)
		return nil, e
	})
	if err != nil {
		t.Fatal(err)
	}
	// The second group is not an authorized target for this request.
	err = s.mutate(ctx, "global", "ack.address-change", func(tx *core.Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, "UPDATE outbox SET conversation_id=? WHERE id=?", other.ConversationID, out.ID)
		return nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.mutate(ctx, "global", "ack.begin-wrong-address", func(tx *core.Tx) (any, error) { return tx.BeginDelivery(ctx, out.ID) })
	if core.ErrorCode(err) != "denied" {
		t.Fatalf("changed receipt target allowed: %v", err)
	}
	err = s.mutate(ctx, "global", "ack.withdraw", func(tx *core.Tx) (any, error) {
		if _, err := tx.Conn.ExecContext(ctx, "UPDATE outbox SET conversation_id=? WHERE id=?", group.ConversationID, out.ID); err != nil {
			return nil, err
		}
		cfg, _, err = tx.SyncGroupMentionRoutes(ctx, cfg.ID, nil)
		return cfg, err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.mutate(ctx, "global", "ack.begin-withdrawn", func(tx *core.Tx) (any, error) { return tx.BeginDelivery(ctx, out.ID) })
	if err == nil {
		t.Fatal("withdrawn trigger still allowed receipt sending")
	}
	if len(s.Adapter.(*fakeAdapter).requests) != 0 || len(s.Adapter.(*fakeAdapter).reactions) != 0 {
		t.Fatal("read/preparation sent a receipt")
	}
}

func TestRecoveredMentionSendsPreparedReceiptBeforeAgent(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	var out core.OutboxView
	err := s.mutate(ctx, "global", "recover.pending-mention", func(tx *core.Tx) (any, error) {
		msg, err := tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "recovered-question", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "回答问题", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SyncRuntimeMessages(ctx, cfg.ID); err != nil {
			return nil, err
		}
		batch, err := tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now().Add(time.Hour))
		if err != nil {
			return nil, err
		}
		tasks, err := tx.CompleteRuntimeBatch(ctx, batch, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "mention:" + msg.MessageID, Title: "回答", Instructions: "回答关联的群消息", MessageIDs: []string{msg.MessageID}}}})
		if err != nil {
			return nil, err
		}
		out, err = tx.PrepareTaskAcknowledgement(ctx, tasks[0].ID)
		return out, err
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := s.Adapter.(*fakeAdapter)
	models := &fakeModels{}
	s.Analyzer = models
	s.Executor = &mentionCheckingExecutor{models: models, before: func(ExecutionInput) {
		if len(adapter.requests) != 0 || len(adapter.reactions) != 2 || adapter.reactions[0].Emoji != core.RuntimeAcknowledgement || adapter.reactions[1].Emoji != core.RuntimeProcessingAcknowledgement || adapter.reactions[0].MessageID != "recovered-question" {
			t.Fatalf("recovery started Agent before trigger receipt: %+v", adapter.requests)
		}
	}}
	preset, err := agent.Enable(ctx, s.Home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	s.tick(ctx, cfg, preset)
	var state string
	if err = s.Store.DB.QueryRow("SELECT state FROM outbox WHERE id=?", out.ID).Scan(&state); err != nil || state != "accepted" {
		t.Fatalf("prepared receipt state=%s err=%v", state, err)
	}
	s.tick(ctx, cfg, preset)
	if models.analyses != 0 || models.executes != 1 || adapter.receipts != 3 || adapter.sends != 1 {
		t.Fatal("recovery reclassified or duplicated receipt/answer")
	}
}

type cardActioner struct{ calls int }

func (a *cardActioner) ExecuteConfirmedAction(context.Context, ActionExecutionInput) (core.RuntimeAttemptResult, error) {
	a.calls++
	return core.RuntimeAttemptResult{Result: "操作已执行"}, nil
}
func TestGroupConfirmationCardExecutesAfterOwnerClickAndMentionsRequester(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	history, err := core.ReadChannel(ctx, s.Store.DB, cfg.ContextChannelID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.mutate(ctx, "global", "card.attest", func(tx *core.Tx) (any, error) { return tx.AttestDWSOwner(ctx, history.ID, history.ConfigVersion) }); err != nil {
		t.Fatal(err)
	}
	var principal string
	if err = s.Store.DB.QueryRow(`SELECT principal_id FROM identity_aliases WHERE tenant=? AND id_type='user_id' AND id_value='owner'`, app.Tenant).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	identity := app.Identity
	identity.ConfirmationCardTemplate = "confirm.schema"
	if _, err = s.Store.DB.Exec(`UPDATE channels SET identity=? WHERE id=?`, core.JSON(identity), app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.DB.Exec(`UPDATE runtime_configs SET owner_principal_id=?,owner_id_type='user_id',owner_id_value='owner' WHERE id=?`, principal, cfg.ID); err != nil {
		t.Fatal(err)
	}
	cfg, err = core.ReadRuntime(ctx, s.Store.DB, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &contextOnlyAnalyzer{}
	executor := &modelReportingExecutor{actions: []core.RuntimeAction{{Kind: "infra_change", Target: "test", Payload: "change capacity"}}}
	actioner := &cardActioner{}
	s.Analyzer, s.Executor, s.Actioner = analyzer, executor, actioner
	preset, err := agent.Enable(ctx, s.Home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.mutate(ctx, "global", "card.request", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "card-question", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester", DisplayName: "发起人"}, Mentioned: true, Body: "请调整", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx, cfg, preset)
	adapter := s.Adapter.(*fakeAdapter)
	if analyzer.calls != 0 || actioner.calls != 0 || len(adapter.requests) != 1 || adapter.requests[0].Format != "confirmation_card" {
		t.Fatalf("routing=%+v actions=%d", adapter.requests, actioner.calls)
	}
	var card core.RuntimeCard
	if err = json.Unmarshal([]byte(adapter.requests[0].Content), &card); err != nil {
		t.Fatal(err)
	}
	if len(card.Mentions) != 2 || card.Mentions[0].IDValue != "requester" || card.Mentions[1].IDValue != "owner" {
		t.Fatalf("mentions=%+v", card.Mentions)
	}
	if card.Title != "请调整" || card.Summary != "待确认后执行 · 1 项操作" || card.Details != "目标：test\n内容：change capacity" {
		t.Fatalf("confirmation card presentation=%+v", card)
	}
	if !strings.HasSuffix(card.Text, "\n\n@发起人 @DWS 所有者") {
		t.Fatalf("confirmation card does not visibly mention its recipients: %q", card.Text)
	}
	cb := core.RuntimeCardCallback{EventID: "click", CorpID: app.Tenant, CardID: adapter.requests[0].IdempotencyKey, SpaceID: group.ConversationID, SpaceType: "IM_GROUP", UserID: "owner", UserIDType: 1, Action: "confirm"}
	if err = s.mutate(ctx, "global", "card.click", func(tx *core.Tx) (any, error) { return tx.ConfirmRuntimeCard(ctx, app.ID, cb) }); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx, cfg, preset)
	s.tick(ctx, cfg, preset)
	if actioner.calls != 1 || analyzer.calls != 0 || len(adapter.requests) != 2 || adapter.requests[1].ConversationID != group.ConversationID || adapter.requests[1].Format != "group_markdown" {
		t.Fatalf("actions=%d deliveries=%+v", actioner.calls, adapter.requests)
	}
	if err = json.Unmarshal([]byte(adapter.requests[1].Content), &card); err != nil {
		t.Fatal(err)
	}
	if len(card.Mentions) != 1 || card.Mentions[0].IDValue != "requester" || !strings.Contains(card.Text, "操作已执行") {
		t.Fatalf("final=%+v", card)
	}
	if len(adapter.reactions) != 3 || adapter.reactions[2].Emoji != core.RuntimeCompletionAcknowledgement {
		t.Fatalf("confirmed action did not close the stage marker: %+v", adapter.reactions)
	}
	s.tick(ctx, cfg, preset)
	if actioner.calls != 1 || len(adapter.requests) != 2 {
		t.Fatal("repeated execution or reply")
	}
}

func TestGroupPendingActionUsesTextConfirmationWhenCardsAreDisabled(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	identity := app.Identity
	identity.ConfirmationCardTemplate = "confirm.schema"
	if _, err := s.Store.DB.Exec(`UPDATE channels SET identity=? WHERE id=?`, core.JSON(identity), app.ID); err != nil {
		t.Fatal(err)
	}
	s.DisableConfirmationCards = true
	s.Analyzer = &contextOnlyAnalyzer{}
	s.Executor = &modelReportingExecutor{actions: []core.RuntimeAction{{Kind: "infra_change", Target: "test", Payload: "change capacity"}}}
	s.Actioner = &cardActioner{}
	preset, err := agent.Enable(ctx, s.Home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.mutate(ctx, "global", "text-confirmation.request", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "text-confirmation-question", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "请调整", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx, cfg, preset)
	adapter := s.Adapter.(*fakeAdapter)
	if len(adapter.requests) != 1 || adapter.requests[0].Format != "group_markdown" {
		t.Fatalf("disabled card approval still used card delivery: %+v", adapter.requests)
	}
	tasks, err := core.RuntimeTaskList(ctx, s.Store.DB, cfg.ID, "awaiting_confirmation", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("pending task missing: %+v err=%v", tasks, err)
	}
	task, err := core.ReadRuntimeTask(ctx, s.Store.DB, tasks[0].ID)
	if err != nil || len(task.Actions) != 1 {
		t.Fatalf("pending action missing: %+v err=%v", task, err)
	}
	var reply core.RuntimeCard
	if err = json.Unmarshal([]byte(adapter.requests[0].Content), &reply); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply.Text, core.ConfirmationToken(task.Actions[0])) {
		t.Fatalf("text confirmation token missing from fallback reply: %q", reply.Text)
	}
}
