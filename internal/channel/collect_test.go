package channel

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// fakeAdapter serves scripted windows and events. It contacts nothing, so the
// collection loop is verified without a platform or real credentials.
type fakeAdapter struct {
	caps      core.Capabilities
	windows   []WindowResult
	window    int
	windowErr error
	events    []core.NormalizedEvent
	rejects   []RejectedEvent
	ready     map[string]any
	runErr    error
	handled   int
	probes    int
}

type blockingAdapter struct {
	fakeAdapter
	started chan Config
}

func TestConfigForExcludesIgnoredRoutes(t *testing.T) {
	cfg := ConfigFor(core.Channel{Routes: []core.Route{
		{ConversationID: "cid:watched", Status: "active", Mode: "collect"},
		{ConversationID: "cid:ignored", Status: "active", Mode: "ignore"},
	}})
	if len(cfg.Conversations) != 1 || cfg.Conversations[0] != "cid:watched" {
		t.Fatalf("unexpected receiver scope: %+v", cfg.Conversations)
	}
}

func (a *blockingAdapter) RunReceiver(ctx context.Context, cfg Config, opts ReceiverOptions) error {
	if opts.Ready != nil {
		opts.Ready(map[string]any{"marker": "ready"})
	}
	a.started <- cfg
	<-ctx.Done()
	return nil
}

func (f *fakeAdapter) Name() string { return "fake" }
func (f *fakeAdapter) ProbeCapabilities(ctx context.Context, cfg Config) (core.Capabilities, error) {
	f.probes++
	return f.caps, nil
}

func TestProbeRunsAgainAfterChannelConfigurationChanges(t *testing.T) {
	adapter := &fakeAdapter{caps: core.Capabilities{Verified: map[string]bool{"history": true, "receive": true, "send": true}}}
	collector, stored := testCollector(t, adapter)
	ctx := context.Background()
	req := core.Request{Scope: "global", Actor: "test"}
	if _, err := collector.Probe(ctx, req, stored.Name); err != nil {
		t.Fatal(err)
	}
	current, err := core.ReadChannel(ctx, collector.Store.DB, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = collector.Store.Mutate(ctx, core.Request{Scope: "global", Command: "channel.configure", Actor: "test"}, func(tx *core.Tx) (any, error) {
		return tx.ApplyChannelConfig(ctx, core.ChannelInput{Name: current.Name, Kind: current.Kind, Identity: current.Identity}, current.ConfigVersion, 0, "test configuration change")
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = collector.Probe(ctx, req, stored.Name); err != nil {
		t.Fatal(err)
	}
	if adapter.probes != 2 {
		t.Fatalf("platform probes = %d; want 2", adapter.probes)
	}
	current, err = core.ReadChannel(ctx, collector.Store.DB, stored.ID)
	if err != nil || !current.Capabilities.Verified["receive"] {
		t.Fatalf("capabilities were not restored: %+v err=%v", current.Capabilities, err)
	}
}
func (f *fakeAdapter) ReadWindow(ctx context.Context, cfg Config, w Window) (WindowResult, error) {
	if f.windowErr != nil {
		return WindowResult{}, f.windowErr
	}
	if f.window >= len(f.windows) {
		return WindowResult{Complete: true}, nil
	}
	out := f.windows[f.window]
	f.window++
	return out, nil
}
func (f *fakeAdapter) RunReceiver(ctx context.Context, cfg Config, opts ReceiverOptions) error {
	if f.ready != nil && opts.Ready != nil {
		opts.Ready(f.ready)
	}
	for _, r := range f.rejects {
		if opts.Reject != nil {
			if err := opts.Reject(ctx, r); err != nil {
				return err
			}
		}
	}
	for _, e := range f.events {
		f.handled++
		if err := opts.Handle(ctx, e); err != nil {
			return err
		}
	}
	return f.runErr
}
func (f *fakeAdapter) Send(ctx context.Context, cfg Config, req SendRequest) (SendResult, error) {
	return SendResult{State: "blocked"}, Unsupported("fake", "send")
}

func testCollector(t *testing.T, adapter Adapter) (Collector, core.Channel) {
	t.Helper()
	ctx := context.Background()
	s, err := core.Open(ctx, filepath.Join(t.TempDir(), "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var c core.Channel
	if _, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "channel.add", Key: "dws-main"}, func(tx *core.Tx) (any, error) {
		c, err = tx.AddChannel(ctx, core.ChannelInput{Name: "dws-main", Kind: core.ChannelDwsPersonal,
			Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "user1"},
			Route:    &core.RouteInput{ConversationID: "cid:group1", ConversationType: "group"}})
		return c, err
	}); err != nil {
		t.Fatal(err)
	}
	return Collector{Store: s, Adapter: adapter}, c
}

func event(id, body string) core.NormalizedEvent {
	return core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "fake/1", Origin: "history",
		ProviderMessageID: id, ConversationID: "cid:group1", Sender: core.Sender{IDType: "union_id", IDValue: "alice"},
		Body: body, SentAt: "2026-09-14T08:10:00Z", EventAt: "2026-09-14T08:10:00Z"}
}

// A partial window records a gap and does not advance coverage, so an
// interrupted backfill can never look finished.
func TestPullRecordsGapsAndOnlyAdvancesOnCompleteWindows(t *testing.T) {
	ctx := context.Background()
	adapter := &fakeAdapter{caps: core.Capabilities{Verified: map[string]bool{"history": true}}}
	adapter.windows = []WindowResult{
		{Events: []core.NormalizedEvent{event("m1", "第一条")}, Complete: false, StopReason: "page_or_item_cap_reached", NextCursor: "cursor-2"},
		{Events: []core.NormalizedEvent{event("m2", "第二条")}, Complete: true},
	}
	c, stored := testCollector(t, adapter)
	req := core.Request{Scope: "global", Actor: "test"}
	if _, err := c.Store.Mutate(ctx, core.Request{Scope: "global", Command: "probe"}, func(tx *core.Tx) (any, error) {
		return tx.SetChannelCapabilities(ctx, stored.ID, adapter.caps, "fake")
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	partial, err := c.Pull(ctx, req, stored.Name, "cid:group1", start, start.Add(time.Hour), 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Complete || partial.Applied != 1 || partial.StopReason == "" {
		t.Fatalf("partial pull: %+v", partial)
	}
	if !partial.Watermark.GapUnresolved {
		t.Fatal("a partial window must leave an unresolved gap")
	}
	if partial.Watermark.CoveredUntil != "" {
		t.Fatal("coverage advanced over a partial window")
	}
	if !strings.Contains(partial.Note, "not the end of the history") {
		t.Fatalf("partial note: %q", partial.Note)
	}
	full, err := c.Pull(ctx, req, stored.Name, "cid:group1", start.Add(time.Hour), start.Add(2*time.Hour), 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !full.Complete || full.Watermark.CoveredUntil == "" {
		t.Fatalf("complete pull did not advance coverage: %+v", full)
	}
	// The earlier gap stays visible even after a later window succeeded.
	report, err := core.CoverageReport(ctx, c.Store.DB, stored.Name)
	if err != nil {
		t.Fatal(err)
	}
	conversations := report.(map[string]any)["conversations"].([]map[string]any)
	gaps := conversations[0]["gaps"].([]map[string]any)
	if len(gaps) == 0 {
		t.Fatal("the unresolved gap disappeared from the report")
	}
	if !strings.Contains(conversations[0]["next_action"].(string), "not complete") {
		t.Fatalf("next action: %+v", conversations[0]["next_action"])
	}
}

// Collection is refused before a capability was verified, and a read failure is
// recorded as a gap rather than an empty range.
func TestPullRequiresVerifiedCapabilityAndRecordsFailures(t *testing.T) {
	ctx := context.Background()
	adapter := &fakeAdapter{}
	c, stored := testCollector(t, adapter)
	req := core.Request{Scope: "global", Actor: "test"}
	start := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	if _, err := c.Pull(ctx, req, stored.Name, "cid:group1", start, start.Add(time.Hour), 0, 0); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("pull ran without a verified capability: %v", err)
	}
	adapter.caps = core.Capabilities{Verified: map[string]bool{"history": true}}
	if _, err := c.Probe(ctx, req, stored.Name); err != nil {
		t.Fatal(err)
	}
	// An unbound conversation is refused before any platform call.
	if _, err := c.Pull(ctx, req, stored.Name, "cid:unbound", start, start.Add(time.Hour), 0, 0); core.ErrorCode(err) != "denied" {
		t.Fatalf("unbound conversation pulled: %v", err)
	}
	adapter.windowErr = core.Fail("unavailable", "dws call timed out")
	if _, err := c.Pull(ctx, req, stored.Name, "cid:group1", start, start.Add(time.Hour), 0, 0); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("read failure not surfaced: %v", err)
	}
	report, err := core.CoverageReport(ctx, c.Store.DB, stored.Name)
	if err != nil {
		t.Fatal(err)
	}
	conversations := report.(map[string]any)["conversations"].([]map[string]any)
	gaps := conversations[0]["gaps"].([]map[string]any)
	if len(gaps) != 1 || !strings.Contains(gaps[0]["stop_reason"].(string), "read_failed") {
		t.Fatalf("a failed read was not recorded as a gap: %+v", gaps)
	}
}

// A live session holds the channel lease, counts only committed events, and
// isolates rejected ones instead of dropping them.
func TestReceiveHoldsLeaseAndPersistsBeforeCounting(t *testing.T) {
	ctx := context.Background()
	adapter := &fakeAdapter{caps: core.Capabilities{Verified: map[string]bool{"receive": true}},
		ready:   map[string]any{"marker": "[event] ready", "event_count": 2},
		events:  []core.NormalizedEvent{event("m1", "第一条"), event("m1", "第一条")},
		rejects: []RejectedEvent{{Reason: "invalid_input", Detail: "unsupported event type", Payload: `{"event_type":"user_im_reaction_add"}`}}}
	c, stored := testCollector(t, adapter)
	wakes := 0
	c.OnIntake = func(result core.IntakeResult) {
		wakes++
		if result.ChannelID != stored.ID {
			t.Fatalf("wake channel=%s want %s", result.ChannelID, stored.ID)
		}
		var committed int
		if err := c.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE channel_id=?", stored.ID).Scan(&committed); err != nil || committed != 1 {
			t.Fatalf("intake callback ran before commit: count=%d err=%v", committed, err)
		}
	}
	req := core.Request{Scope: "global", Actor: "test"}
	if _, err := c.Probe(ctx, req, stored.Name); err != nil {
		t.Fatal(err)
	}
	out, err := c.Receive(ctx, core.Request{ID: core.NewID(), Scope: req.Scope, Actor: req.Actor}, stored.Name, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Listening || out.Ready == nil {
		t.Fatalf("readiness was not propagated: %+v", out)
	}
	if out.Applied != 1 || out.Duplicate != 1 {
		t.Fatalf("a redelivered event was double-counted: %+v", out)
	}
	if wakes != 1 {
		t.Fatalf("fresh committed intake wakes=%d want 1", wakes)
	}
	if len(out.Rejected) != 1 {
		t.Fatalf("rejected events: %+v", out.Rejected)
	}
	// A rejected event is stored so the parser gap is findable later.
	inbox, err := core.InboxList(ctx, c.Store.DB, stored.Name, "rejected", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox.(map[string]any)["events"].([]map[string]any)) != 1 {
		t.Fatal("a rejected event was dropped instead of recorded")
	}
	// The lease is released after the session, so the next receiver can start.
	lease, err := core.ReadLease(ctx, c.Store.DB, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Held {
		t.Fatal("the lease was not released after the session ended")
	}
	if !strings.Contains(out.Note, "not transactional") {
		t.Fatalf("the note must state that delivery is not loss-free: %q", out.Note)
	}
}

func TestReceiveRenewsLeaseAndNarrowsConversations(t *testing.T) {
	adapter := &blockingAdapter{fakeAdapter: fakeAdapter{caps: core.Capabilities{Verified: map[string]bool{"receive": true}}}, started: make(chan Config, 1)}
	c, stored := testCollector(t, adapter)
	ctx := context.Background()
	if _, err := c.Probe(ctx, core.Request{Scope: "global", Actor: "test"}, stored.Name); err != nil {
		t.Fatal(err)
	}
	c.Conversations = []string{"cid:group1"}
	receiveCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := c.Receive(receiveCtx, core.Request{ID: core.NewID(), Scope: "global", Actor: "runtime-instance"}, stored.Name, time.Second)
		done <- err
	}()
	select {
	case cfg := <-adapter.started:
		if len(cfg.Conversations) != 1 || cfg.Conversations[0] != "cid:group1" {
			t.Fatalf("receiver conversations: %+v", cfg.Conversations)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}
	initial, err := core.ReadLease(ctx, c.Store.DB, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	renewed, err := core.ReadLease(ctx, c.Store.DB, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.Held || renewed.Token != initial.Token || renewed.Until <= initial.Until {
		t.Fatalf("lease was not renewed: initial=%+v renewed=%+v", initial, renewed)
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not stop")
	}
	released, err := core.ReadLease(ctx, c.Store.DB, stored.ID)
	if err != nil || released.Held {
		t.Fatalf("lease remained held after cancellation: %+v, %v", released, err)
	}
}

func TestReceiverLeaseRenewalRetriesTransientContention(t *testing.T) {
	current := core.Lease{Token: "token", Fence: 7, Until: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)}
	calls := 0
	next, err := renewReceiverLease(context.Background(), current, func(context.Context) (core.Lease, error) {
		calls++
		if calls == 1 {
			return core.Lease{}, core.Fail("unavailable", "database busy")
		}
		return core.Lease{Token: current.Token, Fence: current.Fence, Until: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)}, nil
	})
	if err != nil || calls != 2 || next.Token != current.Token || next.Fence != current.Fence {
		t.Fatalf("transient renewal did not recover: next=%+v calls=%d err=%v", next, calls, err)
	}

	calls = 0
	_, err = renewReceiverLease(context.Background(), current, func(context.Context) (core.Lease, error) {
		calls++
		return core.Lease{}, core.Fail("conflict", "lease was replaced")
	})
	if core.ErrorCode(err) != "conflict" || calls != 1 {
		t.Fatalf("replaced lease was retried: calls=%d err=%v", calls, err)
	}

	expired := current
	expired.Until = time.Now().Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
	calls = 0
	_, err = renewReceiverLease(context.Background(), expired, func(context.Context) (core.Lease, error) {
		calls++
		return core.Lease{Token: current.Token, Fence: current.Fence}, nil
	})
	if core.ErrorCode(err) != "unavailable" || calls != 0 {
		t.Fatalf("expired lease was revived: calls=%d err=%v", calls, err)
	}
}

// A write failure inside a live session stops receiving and is reported, so a
// lost event is never acknowledged as received.
func TestReceiveStopsWhenAnEventCannotBePersisted(t *testing.T) {
	ctx := context.Background()
	bad := event("m1", "内容")
	bad.ConversationID = "cid:unbound"
	adapter := &fakeAdapter{caps: core.Capabilities{Verified: map[string]bool{"receive": true}},
		ready: map[string]any{"marker": "[event] ready"}, events: []core.NormalizedEvent{bad, event("m2", "第二条")}}
	c, stored := testCollector(t, adapter)
	req := core.Request{Scope: "global", Actor: "test"}
	if _, err := c.Probe(ctx, req, stored.Name); err != nil {
		t.Fatal(err)
	}
	out, err := c.Receive(ctx, req, stored.Name, time.Minute)
	if core.ErrorCode(err) != "denied" {
		t.Fatalf("an unpersistable event was accepted: %v", err)
	}
	if out.Applied != 0 {
		t.Fatalf("a failed write was counted as received: %+v", out)
	}
	if adapter.handled != 1 {
		t.Fatalf("the receiver continued after a failed write: %d", adapter.handled)
	}
	if !strings.Contains(out.Note, "overlapping window") {
		t.Fatalf("recovery guidance missing: %q", out.Note)
	}
	// The lease is still released so a restart is not blocked by a crash.
	lease, err := core.ReadLease(ctx, c.Store.DB, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Held {
		t.Fatal("a failed session left the channel leased")
	}
}

// Offline replay works with dws disconnected, marks events as imported and
// refuses an event that claims an unverified online identity.
func TestIngestReplaysOfflineWithoutForgingIdentity(t *testing.T) {
	ctx := context.Background()
	c, stored := testCollector(t, &fakeAdapter{})
	req := core.Request{Scope: "global", Actor: "test"}
	// An import may only claim an identity that was explicitly linked, so alice
	// is verified first and boss deliberately is not.
	if _, err := c.Store.Mutate(ctx, core.Request{Scope: "global", Command: "identity.link"}, func(tx *core.Tx) (any, error) {
		return tx.LinkIdentity(ctx, stored.Tenant, core.Sender{IDType: "union_id", IDValue: "alice"},
			core.Sender{IDType: "union_id", IDValue: "alice"}, "operator_confirmed")
	}); err != nil {
		t.Fatal(err)
	}
	forged := event("m3", "冒充上级")
	forged.Sender = core.Sender{IDType: "union_id", IDValue: "boss"}
	lines := strings.Join([]string{
		core.JSON(event("m1", "导入的内容")),
		core.JSON(event("m1", "导入的内容")),
		`{"kind":"message","adapter":"fake"`,
		core.JSON(forged),
		core.JSON(core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "fake/1",
			ProviderMessageID: "m2", ConversationID: "cid:group1",
			Sender: core.Sender{IDType: "unknown", IDValue: "anon"}, Body: "来源不明", SentAt: "2026-09-14T08:20:00Z"}),
	}, "\n")
	out, err := c.Ingest(ctx, req, stored.Name, strings.NewReader(lines))
	if err != nil {
		t.Fatal(err)
	}
	// Two lines apply, the repeat is a duplicate, and the malformed line plus the
	// forged identity are rejected.
	if out.Applied != 2 || out.Rejected != 2 {
		t.Fatalf("import results: %+v", out)
	}
	if out.Duplicates != 1 {
		t.Fatalf("a repeated import line was not deduplicated: %+v", out)
	}
	joined := strings.Join(out.Reasons, ";")
	if !strings.Contains(joined, "denied") || !strings.Contains(joined, "boss") {
		t.Fatalf("an unverified identity was imported: %q", joined)
	}
	var origin string
	if err = c.Store.DB.QueryRowContext(ctx, "SELECT origin FROM inbox_events WHERE status='applied' LIMIT 1").Scan(&origin); err != nil {
		t.Fatal(err)
	}
	if origin != "import" {
		t.Fatalf("imported events must be labelled import, got %q", origin)
	}
	// An import does not advance coverage; only a read window can.
	mark, err := core.ReadWatermark(ctx, c.Store.DB, stored.ID, "cid:group1")
	if err != nil {
		t.Fatal(err)
	}
	if mark.CoveredUntil != "" {
		t.Fatal("an offline import advanced coverage")
	}
}

// The next window overlaps the covered range, because dws receiving an event and
// memgov committing it are separate processes with no exact resume point.
func TestNextWindowOverlapsCoveredRange(t *testing.T) {
	ctx := context.Background()
	c, stored := testCollector(t, &fakeAdapter{})
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	start, end, err := core.NextWindow(ctx, c.Store.DB, stored.ID, "cid:group1", now, time.Hour, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !end.Equal(now) || !start.Equal(now.Add(-time.Hour)) {
		t.Fatalf("first window %s..%s", start, end)
	}
	if _, err = c.Store.Mutate(ctx, core.Request{Scope: "global", Command: "coverage"}, func(tx *core.Tx) (any, error) {
		return tx.RecordCoverage(ctx, core.CoverageWindow{ChannelID: stored.ID, ConversationID: "cid:group1",
			StartAt: now.Add(-time.Hour).Format(time.RFC3339), EndAt: now.Format(time.RFC3339), Complete: true})
	}); err != nil {
		t.Fatal(err)
	}
	start, _, err = core.NextWindow(ctx, c.Store.DB, stored.ID, "cid:group1", now.Add(30*time.Minute), time.Hour, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(now.Add(-5 * time.Minute)) {
		t.Fatalf("the next window did not overlap the covered range: %s", start)
	}
}

type cardLeaseAdapter struct {
	fakeAdapter
	store  *core.Store
	called bool
	t      *testing.T
}

func (a *cardLeaseAdapter) RunReceiver(ctx context.Context, cfg Config, opts ReceiverOptions) error {
	if _, err := a.store.DB.Exec(`UPDATE channel_leases SET fence=fence+1 WHERE channel_id=?`, cfg.ChannelID); err != nil {
		a.t.Fatal(err)
	}
	_, err := opts.ConfirmCard(ctx, core.RuntimeCardCallback{EventID: "click"})
	a.called = true
	if core.ErrorCode(err) != "unavailable" {
		a.t.Fatalf("stale card lease was acknowledged: %v", err)
	}
	if err = opts.Reject(ctx, RejectedEvent{Reason: "denied", Detail: "card refusal"}); err == nil {
		a.t.Fatal("stale receiver recorded/acknowledged card rejection")
	}
	return nil
}
func TestCardConfirmationAndRejectionRequireCurrentReceiverFence(t *testing.T) {
	a := &cardLeaseAdapter{fakeAdapter: fakeAdapter{caps: core.Capabilities{Verified: map[string]bool{"receive": true}}}, t: t}
	collector, stored := testCollector(t, a)
	a.store = collector.Store
	ctx := context.Background()
	if _, err := collector.Probe(ctx, core.Request{Scope: "global", Actor: "test"}, stored.Name); err != nil {
		t.Fatal(err)
	}
	_, _ = collector.Receive(ctx, core.Request{Scope: "global", Actor: "test"}, stored.Name, time.Minute)
	if !a.called {
		t.Fatal("card handler was not called")
	}
}
