package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type confirmationModels struct {
	fakeModels
	actions int
}

func (m *confirmationModels) Execute(context.Context, ExecutionInput) (core.RuntimeAttemptResult, error) {
	m.executes++
	return core.RuntimeAttemptResult{Result: "Prepared operation", Actions: []core.RuntimeAction{{Kind: "send_message", Target: "other-user", Payload: "reviewed message"}}}, nil
}
func (m *confirmationModels) ExecuteConfirmedAction(context.Context, ActionExecutionInput) (core.RuntimeAttemptResult, error) {
	m.actions++
	return core.RuntimeAttemptResult{Result: "Operation verified"}, nil
}

func TestProactiveBlockedActionDoesNotAcceptBotConfirmation(t *testing.T) {
	s, cfg, preset, _, _ := setupService(t)
	ctx := context.Background()
	models := &confirmationModels{}
	s.Analyzer, s.Executor, s.Actioner = models, models, models
	var app core.Channel
	var inbox core.Route
	_, err := s.Store.Mutate(ctx, core.Request{Scope: "global", Command: "test.confirmation.setup"}, func(tx *core.Tx) (any, error) {
		dws, e := core.ReadChannel(ctx, tx.Conn, cfg.ChannelID)
		if e != nil {
			return nil, e
		}
		if _, e = tx.AttestDWSOwner(ctx, dws.ID, dws.ConfigVersion); e != nil {
			return nil, e
		}
		app, e = tx.AddChannel(ctx, core.ChannelInput{Name: "owner-app", Kind: core.ChannelDingTalkApp, Identity: core.ChannelIdentity{ExpectedCorpID: "corp", ClientID: "client", RobotCode: "robot", HistoryChannel: dws.ID}})
		if e != nil {
			return nil, e
		}
		inbox, e = tx.AddRoute(ctx, app.ID, core.RouteInput{ConversationID: "owner-app-inbox", ConversationType: "direct"})
		if e != nil {
			return nil, e
		}
		if _, e = tx.UpdateRoute(ctx, inbox.ID, inbox.Version, core.RouteInput{SendPolicy: "dispatch_only"}, "owner inbox"); e != nil {
			return nil, e
		}
		return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderEventID: "request", ProviderMessageID: "request", ConversationID: "watch", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "owner"}, Body: "send the prepared report", SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	s.tick(ctx, cfg, preset)
	tasks, err := core.RuntimeTaskList(ctx, s.Store.DB, cfg.ID, "blocked", 10)
	if err != nil || len(tasks) != 1 || models.actions != 0 {
		t.Fatalf("task preparation: %+v %v actions=%d", tasks, err, models.actions)
	}
	task, err := core.ReadRuntimeTask(ctx, s.Store.DB, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	intake := func(id, user string) {
		t.Helper()
		_, e := s.Store.Mutate(ctx, core.Request{Scope: "global", Command: "test.confirmation.intake"}, func(tx *core.Tx) (any, error) {
			return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderEventID: id, ProviderMessageID: id, ConversationID: inbox.ConversationID, Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: user}, Body: core.ConfirmationToken(task.Actions[0]), SentAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)})
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	intake("wrong-owner", "mallory")
	s.tick(ctx, cfg, preset)
	if models.actions != 0 {
		t.Fatal("another user triggered an external action")
	}
	intake("owner-approval", "owner")
	s.tick(ctx, cfg, preset)
	s.tick(ctx, cfg, preset)
	if models.actions != 0 || models.executes != 1 || models.analyses != 1 {
		t.Fatalf("execution counts action=%d execution=%d analysis=%d", models.actions, models.executes, models.analyses)
	}
	current, err := core.ReadRuntimeTask(ctx, s.Store.DB, task.ID)
	if err != nil || current.Status != "blocked" || current.Actions[0].Status == "executed" {
		t.Fatalf("confirmed task %+v %v", current, err)
	}
}
