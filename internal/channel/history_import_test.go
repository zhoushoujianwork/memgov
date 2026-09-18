package channel

import (
	"context"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type importAdapter struct {
	fakeAdapter
	requests []Window
	cancel   func()
}

func (a *importAdapter) ReadWindow(ctx context.Context, cfg Config, w Window) (WindowResult, error) {
	a.requests = append(a.requests, w)
	if a.cancel != nil {
		a.cancel()
	}
	return a.fakeAdapter.ReadWindow(ctx, cfg, w)
}
func importWorkerFixture(t *testing.T, adapter *importAdapter) (HistoryImportWorker, core.DataSource, core.HistoryImport) {
	t.Helper()
	c, stored := testCollector(t, adapter)
	ctx := context.Background()
	var source core.DataSource
	var h core.HistoryImport
	_, err := c.Store.Mutate(ctx, core.Request{Command: "test.history.setup", Scope: "global"}, func(tx *core.Tx) (any, error) {
		_, e := tx.SetChannelCapabilities(ctx, stored.ID, core.Capabilities{Verified: map[string]bool{"history": true}}, "fake")
		if e != nil {
			return nil, e
		}
		source, e = tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: "archive", Channel: stored.ID, Workspace: "global"})
		if e != nil {
			return nil, e
		}
		// This fixture starts after a successful bounded discovery.
		source, e = tx.SyncDataSourceGroups(ctx, source.ID, []string{stored.Routes[0].ConversationID})
		if e != nil {
			return nil, e
		}
		source, e = tx.SetDataSourceStatus(ctx, source.ID, "running")
		if e != nil {
			return nil, e
		}
		start, _ := time.Parse(time.RFC3339, "2026-09-01T00:00:00Z")
		h, e = tx.CreateHistoryImport(ctx, source.ID, source.RouteIDs[0], start, start.Add(20*24*time.Hour))
		return h, e
	})
	if err != nil {
		t.Fatal(err)
	}
	return HistoryImportWorker{Collector: c, Request: core.Request{Scope: "global", Actor: "test"}}, source, h
}
func TestHistoryImportWorkerBoundsResumesAndStopsAtCompletion(t *testing.T) {
	a := &importAdapter{fakeAdapter: fakeAdapter{windows: []WindowResult{{Events: []core.NormalizedEvent{event("m1", "context")}, NextCursor: "next", StopReason: "page_limit"}, {Events: []core.NormalizedEvent{event("m1", "context"), event("m2", "more context")}, Complete: true}}}}
	w, source, h := importWorkerFixture(t, a)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		ran, err := w.Step(ctx, source.ID)
		if !ran || err != nil {
			t.Fatalf("step %d ran=%v err=%v", i, ran, err)
		}
	}
	if len(a.requests) != 2 || a.requests[0].Cursor != "" || a.requests[1].Cursor != "next" {
		t.Fatalf("cursor %+v", a.requests)
	}
	for _, r := range a.requests {
		if r.PageLimit != 5 || r.MaxItems != 100 || r.Start.Format(time.RFC3339) != h.StartAt || r.End.Format(time.RFC3339) != h.EndAt {
			t.Fatalf("bounds %+v", r)
		}
	}
	job, err := core.ReadHistoryImport(ctx, w.Collector.Store.DB, h.ID)
	if err != nil || job.Status != "completed" || job.Applied != 2 || job.Duplicates != 1 {
		t.Fatalf("job %+v %v", job, err)
	}
	ran, err := w.Step(ctx, source.ID)
	if ran || err != nil || len(a.requests) != 2 {
		t.Fatalf("idle invoked model/provider ran=%v err=%v", ran, err)
	}
}
func TestHistoryImportWorkerReadFailureRetainsCursorAndSanitizesError(t *testing.T) {
	a := &importAdapter{fakeAdapter: fakeAdapter{windowErr: core.Fail("rate_limited", "credential=secret raw stderr")}}
	w, source, h := importWorkerFixture(t, a)
	ctx := context.Background()
	if ran, err := w.Step(ctx, source.ID); !ran || err == nil {
		t.Fatal("failure not returned")
	}
	job, err := core.ReadHistoryImport(ctx, w.Collector.Store.DB, h.ID)
	if err != nil || job.Status != "queued" || job.ErrorCode != "rate_limited" || job.Cursor != "" || job.Events != 0 {
		t.Fatalf("job %+v %v", job, err)
	}
	if ran, err := w.Step(ctx, source.ID); ran || err != nil || len(a.requests) != 1 {
		t.Fatalf("retry backoff ignored: %v %v", ran, err)
	}
}
func TestHistoryImportWorkerCancellationBlocksInFlightPage(t *testing.T) {
	a := &importAdapter{fakeAdapter: fakeAdapter{windows: []WindowResult{{Events: []core.NormalizedEvent{event("m1", "context")}, Complete: true}}}}
	w, source, h := importWorkerFixture(t, a)
	ctx := context.Background()
	a.cancel = func() {
		_, err := w.Collector.Store.Mutate(ctx, core.Request{Command: "test.cancel"}, func(tx *core.Tx) (any, error) { return tx.CancelHistoryImport(ctx, h.ID) })
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Step(ctx, source.ID); core.ErrorCode(err) != "conflict" {
		t.Fatalf("cancelled read committed: %v", err)
	}
	var n int
	w.Collector.Store.DB.QueryRow("SELECT count(*) FROM messages").Scan(&n)
	if n != 0 {
		t.Fatal("cancelled page persisted")
	}
}

func TestHistoryCursorFailureKeepsItsSafeLogCode(t *testing.T) {
	if got := sourceErrorCode(core.Fail("history_cursor_stalled", "safe summary")); got != "history_cursor_stalled" {
		t.Fatalf("history failure hidden as %q", got)
	}
}
