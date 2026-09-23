package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestCommittedIntakeWakeBypassesFallbackTicker(t *testing.T) {
	service, cfg, _, _, _ := setupService(t)
	wake := make(chan struct{}, 1)
	service.ExternalReceiver = true
	service.Tick = time.Hour
	service.Wake = wake
	before, err := core.ReadRuntime(context.Background(), service.Store.DB, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx, cfg.ID) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runtime stop: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("runtime did not stop")
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		current, err := core.ReadRuntime(ctx, service.Store.DB, cfg.ID)
		if err == nil && current.LastStartedAt != "" && current.LastStartedAt != before.LastStartedAt {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "fast-path.intake"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "wake-message", ConversationID: "watch", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "alice"}, Body: "process immediately", SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	wake <- struct{}{}

	deadline = time.Now().Add(2 * time.Second)
	for {
		tasks, err := core.RuntimeTaskList(ctx, service.Store.DB, cfg.ID, "completed", 10)
		if err == nil && len(tasks) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("committed intake waited for the one-hour fallback ticker: tasks=%+v err=%v", tasks, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type blockingStageAdapter struct {
	fakeAdapter
	started chan struct{}
	release chan struct{}
}

func (a *blockingStageAdapter) AddReaction(ctx context.Context, cfg channel.Config, req channel.ReactionRequest) error {
	select {
	case a.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.release:
		return a.fakeAdapter.AddReaction(ctx, cfg, req)
	}
}

func TestSlowStageFeedbackDoesNotBlockAgentOrResult(t *testing.T) {
	service, cfg, app, group, _ := setupGroupMentionService(t)
	models := &fakeModels{}
	adapter := &blockingStageAdapter{started: make(chan struct{}, 1), release: make(chan struct{})}
	service.Adapter = adapter
	service.Analyzer, service.Executor, service.Actioner = models, models, models
	preset, err := agent.Enable(context.Background(), service.Home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	service.stageQueue = make(chan stageDelivery, 8)
	stageDone := make(chan struct{})
	go func() {
		defer close(stageDone)
		service.runStageDeliveries(ctx)
	}()
	defer func() {
		close(adapter.release)
		cancel()
		<-stageDone
		service.stageQueue = nil
	}()

	if err = service.mutate(ctx, "global", "fast-path.mention", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "slow-reaction", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "answer now", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	tickDone := make(chan struct{})
	go func() {
		service.tick(ctx, cfg, preset)
		close(tickDone)
	}()
	select {
	case <-adapter.started:
	case <-time.After(time.Second):
		t.Fatal("receipt delivery did not start")
	}
	select {
	case <-tickDone:
	case <-time.After(time.Second):
		t.Fatal("slow receipt blocked Agent execution")
	}
	if models.executes != 1 || adapter.sends != 1 {
		t.Fatalf("Agent/result did not bypass slow feedback: executes=%d sends=%d", models.executes, adapter.sends)
	}
}

type blockingGroupSource struct {
	started chan struct{}
	release chan struct{}
}

func (s *blockingGroupSource) ListGroupConversations(ctx context.Context, _ channel.Config) ([]channel.GroupConversation, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return []channel.GroupConversation{}, nil
	}
}

func TestGroupDiscoveryDoesNotBlockMentionWake(t *testing.T) {
	service, cfg, app, group, _ := setupGroupMentionService(t)
	models := &fakeModels{}
	service.Analyzer, service.Executor, service.Actioner = models, models, models
	service.ExternalReceiver = true
	service.Tick = time.Hour
	wake := make(chan struct{}, 1)
	service.Wake = wake
	source := &blockingGroupSource{started: make(chan struct{}, 1), release: make(chan struct{})}
	service.GroupSource = source
	if _, err := agent.Enable(context.Background(), service.Home, "claude", "claude-default"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx, cfg.ID) }()
	defer func() {
		close(source.release)
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runtime stop: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("runtime did not stop")
		}
	}()
	select {
	case <-source.started:
	case <-time.After(2 * time.Second):
		t.Fatal("group discovery did not start")
	}
	if _, err := service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "fast-path.group-intake"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "mention-during-discovery", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "do not wait for discovery", SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	wake <- struct{}{}

	deadline := time.Now().Add(2 * time.Second)
	for {
		tasks, err := core.RuntimeTaskList(ctx, service.Store.DB, cfg.ID, "completed", 10)
		if err == nil && len(tasks) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mention waited for blocked group discovery: tasks=%+v err=%v", tasks, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
