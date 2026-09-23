package runtime

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

type sharedStreamAdapter struct {
	fakeAdapter
	starts atomic.Int32
	active atomic.Int32
	peak   atomic.Int32
}

func (a *sharedStreamAdapter) RunReceiver(ctx context.Context, cfg channel.Config, opts channel.ReceiverOptions) error {
	a.starts.Add(1)
	active := a.active.Add(1)
	for old := a.peak.Load(); active > old; old = a.peak.Load() {
		if a.peak.CompareAndSwap(old, active) {
			break
		}
	}
	defer a.active.Add(-1)
	opts.Ready(map[string]any{"ready": true})
	<-ctx.Done()
	return nil
}
func (*sharedStreamAdapter) Send(context.Context, channel.Config, channel.SendRequest) (channel.SendResult, error) {
	return channel.SendResult{State: "accepted", Receipt: "fake-receipt"}, nil
}
func receiverEventually(t *testing.T, condition func() bool) {
	t.Helper()
	// This is an eventual coordination assertion, not a latency benchmark.
	// Race-instrumented SQLite transactions can be slow when all packages run.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("receiver coordination did not converge")
}
func runReceiverForTest(t *testing.T, s *Service, cfg core.RuntimeConfig, c core.Channel) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.receiveLoop(ctx, cfg, c) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("receiver failed to stop")
		}
	})
	return cancel, done
}
func TestApplicationReceiverFollowersTakeOverWithoutWarnings(t *testing.T) {
	s, cfg, c, _, _ := setupGroupMentionService(t)
	adapter := &sharedStreamAdapter{}
	s.Adapter = adapter
	other := cfg
	other.ID = core.NewID()
	followerLog, err := runlog.Open(s.Home, other.ID, io.Discard, runlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer followerLog.Close()
	follower := &Service{Home: s.Home, Store: s.Store, Adapter: adapter, Logger: followerLog}
	cancelA, doneA := runReceiverForTest(t, s, cfg, c)
	cancelB, doneB := runReceiverForTest(t, follower, other, c)
	receiverEventually(t, func() bool { return adapter.starts.Load() == 1 })
	receiverEventually(t, func() bool {
		a, _ := runlog.Show(s.Home, cfg.ID, runlog.Filter{})
		b, _ := runlog.Show(s.Home, other.ID, runlog.Filter{})
		for _, e := range append(a, b...) {
			if e.Event == "receiver_standby" {
				return true
			}
		}
		return false
	})
	lease, err := core.ReadLease(context.Background(), s.Store.DB, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	until, _ := time.Parse(time.RFC3339Nano, lease.Until)
	if time.Until(until) > 30*time.Second {
		t.Fatalf("crash failover exceeds lease bound: %v", lease.Until)
	}
	if strings.HasPrefix(lease.Holder, "runtime:"+cfg.ID+":") {
		cancelA()
		<-doneA
	} else {
		cancelB()
		<-doneB
	}
	receiverEventually(t, func() bool { return adapter.starts.Load() == 2 })
	// Restart the stopped participant: it must not fence out the healthy successor.
	restartedCfg := cfg
	if !strings.HasPrefix(lease.Holder, "runtime:"+cfg.ID+":") {
		restartedCfg = other
	}
	restarted := &Service{Home: s.Home, Store: s.Store, Adapter: adapter, Logger: s.Logger}
	cancelRestart, doneRestart := runReceiverForTest(t, restarted, restartedCfg, c)
	time.Sleep(1100 * time.Millisecond)
	if adapter.starts.Load() != 2 || adapter.peak.Load() != 1 {
		t.Fatalf("multiple live streams: starts=%d peak=%d", adapter.starts.Load(), adapter.peak.Load())
	}
	cancelRestart()
	<-doneRestart
	cancelA()
	cancelB()
	<-doneA
	<-doneB
	for _, id := range []string{cfg.ID, other.ID} {
		events, e := runlog.Show(s.Home, id, runlog.Filter{})
		if e != nil {
			t.Fatal(e)
		}
		for _, event := range events {
			if event.Level == "warn" || event.Event == "receiver_reconnect" {
				t.Fatalf("healthy follower emitted warning: %+v", event)
			}
		}
	}
	lease, err = core.ReadLease(context.Background(), s.Store.DB, c.ID)
	if err != nil || lease.Held {
		t.Fatalf("lease survived clean stop: %+v %v", lease, err)
	}
}
func TestApplicationReceiverTakesOverExpiredCrashLease(t *testing.T) {
	s, cfg, c, _, _ := setupGroupMentionService(t)
	adapter := &sharedStreamAdapter{}
	s.Adapter = adapter
	var orphan core.Lease
	if err := s.mutate(context.Background(), "global", "test.orphan", func(tx *core.Tx) (any, error) {
		var err error
		orphan, err = tx.AcquireLease(context.Background(), c.ID, "crashed-process", time.Second)
		return orphan, err
	}); err != nil {
		t.Fatal(err)
	}
	cancel, done := runReceiverForTest(t, s, cfg, c)
	time.Sleep(100 * time.Millisecond)
	if adapter.starts.Load() != 0 {
		t.Fatal("unexpired crash lease was stolen")
	}
	receiverEventually(t, func() bool { return adapter.starts.Load() == 1 })
	next, err := core.ReadLease(context.Background(), s.Store.DB, c.ID)
	if err != nil || next.Fence <= orphan.Fence || next.Token == orphan.Token {
		t.Fatalf("takeover failed to fence crashed receiver: %+v %v", next, err)
	}
	cancel()
	<-done
}

type partialGroupSource struct {
	dwsGroupSourceAdapter
	observation channel.GroupDiscovery
}

func (p *partialGroupSource) DiscoverGroupConversations(context.Context, channel.Config) (channel.GroupDiscovery, error) {
	return p.observation, nil
}
func TestGroupMentionPartialDiscoveryPreservesExistingMounts(t *testing.T) {
	s, cfg, c, a, b := setupGroupMentionService(t)
	s.GroupSource = &partialGroupSource{observation: channel.GroupDiscovery{Groups: []channel.GroupConversation{{ID: b.ConversationID}}, Complete: false}}
	next := s.syncGroups(context.Background(), cfg, c)
	if len(next.RouteIDs) != 2 {
		t.Fatalf("partial positive lost existing mount: %+v", next.RouteIDs)
	}
	s.GroupSource = &partialGroupSource{observation: channel.GroupDiscovery{Groups: []channel.GroupConversation{{ID: b.ConversationID}}, Complete: false, Excluded: []string{a.ConversationID}}}
	next = s.syncGroups(context.Background(), next, c)
	if len(next.RouteIDs) != 1 {
		t.Fatalf("explicit robot exit not applied: %+v", next.RouteIDs)
	}
	events, err := runlog.Show(s.Home, cfg.ID, runlog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Event == "group_sync_failed" {
			t.Fatalf("partial proof treated as failure: %+v", e)
		}
	}
}

type routeTickModels struct {
	fakeModels
	calls, executions atomic.Int32
}

func (m *routeTickModels) Analyze(_ context.Context, b core.RuntimeBatch) (core.RuntimeAnalysis, ModelUsage, error) {
	m.calls.Add(1)
	return core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "discussion", MessageIDs: []string{b.Messages[0].ID}}}}, ModelUsage{}, nil
}
func (m *routeTickModels) Execute(ctx context.Context, input ExecutionInput) (core.RuntimeAttemptResult, error) {
	m.executions.Add(1)
	return m.fakeModels.Execute(ctx, input)
}
func TestSharedApplicationRuntimesBothContinueRouteProcessing(t *testing.T) {
	s, cfg, c, _, _ := setupGroupMentionService(t)
	ctx := context.Background()
	var ownerCfg core.RuntimeConfig
	if err := s.mutate(ctx, "global", "test.direct", func(tx *core.Tx) (any, error) {
		history, err := core.ReadChannel(ctx, tx.Conn, c.Identity.HistoryChannel)
		if err != nil {
			return nil, err
		}
		if _, err := tx.AttestDWSOwner(ctx, history.ID, history.ConfigVersion); err != nil {
			return nil, err
		}
		route, err := core.RouteFor(ctx, tx.Conn, c.ID, "cid:owner-app")
		if err != nil {
			return nil, err
		}
		if _, err = tx.UpdateRoute(ctx, route.ID, route.Version, core.RouteInput{SendPolicy: "draft_only"}, "use callback CID only for direct processing"); err != nil {
			return nil, err
		}
		delivery, err := tx.AddRoute(ctx, c.ID, core.RouteInput{ConversationID: history.Identity.ExpectedUserID, ConversationType: "direct", Mode: "notify", SendPolicy: "dispatch_only"})
		if err != nil {
			return nil, err
		}
		ownerCfg, err = tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Name: "owner-direct", Channel: c.ID, RouteIDs: []string{route.ID}, DeliveryRouteID: delivery.ID, Owner: core.Sender{IDType: "user_id", IDValue: history.Identity.ExpectedUserID}, ApplicationMode: "direct", ItemThreshold: 1, ReconcileSeconds: 10})
		return ownerCfg, err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Enable(ctx, s.Home, "claude", "claude-default"); err != nil {
		t.Fatal(err)
	}
	adapter := &sharedStreamAdapter{}
	modelsA, modelsB := &routeTickModels{}, &routeTickModels{}
	s.Adapter = adapter
	s.Analyzer = modelsA
	s.Executor = modelsA
	s.Actioner = modelsA
	s.Tick = 10 * time.Millisecond
	logger, err := runlog.Open(s.Home, ownerCfg.ID, io.Discard, runlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	other := &Service{Home: s.Home, Store: s.Store, Adapter: adapter, Analyzer: modelsB, Executor: modelsB, Actioner: modelsB, Logger: logger, Tick: 10 * time.Millisecond}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	go func() { doneA <- s.Run(runCtx, cfg.ID) }()
	go func() { doneB <- other.Run(runCtx, ownerCfg.ID) }()
	defer func() {
		cancel()
		select {
		case err := <-doneA:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("group runtime did not stop")
		}
		select {
		case err := <-doneB:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("owner runtime did not stop")
		}
		for _, id := range []string{cfg.ID, ownerCfg.ID} {
			stored, readErr := core.ReadRuntime(ctx, s.Store.DB, id)
			if readErr != nil || stored.Status != "stopped" {
				t.Errorf("cancellation failed to persist stopped runtime: status=%s err=%v", stored.Status, readErr)
			}
		}
		lease, readErr := core.ReadLease(ctx, s.Store.DB, c.ID)
		if readErr != nil || lease.Held {
			t.Errorf("cancellation left receiver leased: held=%t err=%v", lease.Held, readErr)
		}
	}()
	receiverEventually(t, func() bool { return adapter.starts.Load() == 1 })
	if err := s.mutate(ctx, "global", "test.messages", func(tx *core.Tx) (any, error) {
		for i, conversation := range []string{"cid:group-a", "cid:owner-app"} {
			sender := core.Sender{IDType: cfg.OwnerIDType, IDValue: cfg.OwnerIDValue}
			if i == 1 {
				sender = core.Sender{IDType: ownerCfg.OwnerIDType, IDValue: ownerCfg.OwnerIDValue}
			}
			_, err := tx.Intake(ctx, c.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: []string{"group-message", "direct-message"}[i], ConversationID: conversation, Tenant: c.Tenant, Sender: sender, Body: "hello", Mentioned: true, SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
			if err != nil {
				return nil, err
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Both verified group mentions and owner-private messages directly execute
	// Claude while sharing a single application receiver.
	receiverEventually(t, func() bool { return modelsA.executions.Load() > 0 && modelsB.executions.Load() > 0 })
	if modelsA.calls.Load() != 0 || modelsB.calls.Load() != 0 {
		t.Fatal("addressed greeting was sent to the incremental analyzer")
	}
	if adapter.starts.Load() != 1 || adapter.peak.Load() != 1 {
		t.Fatal("independent processing spawned extra receiver")
	}
}
