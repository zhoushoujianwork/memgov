package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

func historyFixture(t *testing.T) (*Store, DataSource, HistoryImport) {
	t.Helper()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	ctx := context.Background()
	var source DataSource
	var h HistoryImport
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "test.history.setup"}, func(tx *Tx) (any, error) {
		var e error
		source, e = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "archive", Channel: c.ID, Workspace: "global"})
		if e != nil {
			return nil, e
		}
		// This fixture starts after a successful bounded discovery.
		source, e = tx.SyncDataSourceGroups(ctx, source.ID, []string{r.ConversationID})
		if e != nil {
			return nil, e
		}
		source, e = tx.SetDataSourceStatus(ctx, source.ID, "running")
		if e != nil {
			return nil, e
		}
		start, _ := time.Parse(time.RFC3339, "2026-09-01T00:00:00Z")
		end := start.Add(20 * 24 * time.Hour)
		h, e = tx.CreateHistoryImport(ctx, source.ID, r.ID, start, end)
		return h, e
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, source, h
}
func historyClaim(t *testing.T, s *Store, id string) HistoryImport {
	t.Helper()
	var h HistoryImport
	ctx := context.Background()
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "test.history.claim"}, func(tx *Tx) (any, error) { var e error; h, e = tx.ClaimHistoryImport(ctx, id, time.Now()); return h, e })
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func historyFinish(t *testing.T, s *Store, h HistoryImport, events []NormalizedEvent, complete bool, cursor string) (HistoryImport, error) {
	t.Helper()
	var out HistoryImport
	ctx := context.Background()
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "test.history.finish"}, func(tx *Tx) (any, error) {
		var e error
		out, e = tx.FinishHistoryImport(ctx, h, events, complete, cursor, "")
		return out, e
	})
	return out, err
}
func TestHistoryImportCheckpointDedupContextAndCoverage(t *testing.T) {
	s, source, h := historyFixture(t)
	ctx := context.Background()
	live := sampleEvent("live", "live demand", Sender{IDType: "union_id", IDValue: "alice"})
	live.ProviderEventID = ""
	intake(t, s, source.ChannelID, live, "live")
	historical := sampleEvent("old", "historical demand", live.Sender)
	historical.ProviderEventID = ""
	h = historyClaim(t, s, source.ID)
	partial, err := historyFinish(t, s, h, []NormalizedEvent{historical, live}, false, "page2")
	if err != nil {
		t.Fatal(err)
	}
	if partial.Cursor != "page2" || partial.Status != "queued" || partial.Applied != 1 || partial.Duplicates != 1 {
		t.Fatalf("partial %+v", partial)
	}
	// A second handle represents a restarted process; the fixed window and cursor survive.
	reopened, err := Open(ctx, s.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resumed := historyClaim(t, reopened, source.ID)
	if resumed.Cursor != "page2" || resumed.StartAt != h.StartAt || resumed.EndAt != h.EndAt {
		t.Fatalf("resumed %+v", resumed)
	}
	done, err := historyFinish(t, reopened, resumed, []NormalizedEvent{historical}, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "completed" || done.Duplicates != 2 {
		t.Fatalf("done %+v", done)
	}
	var oldContext, liveContext int
	if err = s.DB.QueryRow("SELECT context_only FROM messages WHERE provider_message_id='old'").Scan(&oldContext); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow("SELECT context_only FROM messages WHERE provider_message_id='live'").Scan(&liveContext); err != nil {
		t.Fatal(err)
	}
	if oldContext != 1 || liveContext != 0 {
		t.Fatalf("context old=%d live=%d", oldContext, liveContext)
	}
	mark, err := ReadWatermark(ctx, s.DB, source.ChannelID, "cid:group1")
	if err != nil || mark.GapUnresolved || mark.CoveredUntil != h.EndAt {
		t.Fatalf("mark %+v %v", mark, err)
	}
}
func TestHistoryImportAtomicFailureAndRecallTombstone(t *testing.T) {
	s, source, _ := historyFixture(t)
	h := historyClaim(t, s, source.ID)
	e := sampleEvent("old", "text", Sender{IDType: "union_id", IDValue: "alice"})
	e.ProviderEventID = ""
	bad := e
	bad.ProviderMessageID = "bad"
	bad.ConversationID = "cid:other"
	if _, err := historyFinish(t, s, h, []NormalizedEvent{e, bad}, false, "p2"); ErrorCode(err) != "forbidden" {
		t.Fatalf("expected refused foreign event: %v", err)
	}
	var n int
	s.DB.QueryRow("SELECT count(*) FROM messages").Scan(&n)
	if n != 0 {
		t.Fatal("partial messages committed")
	}
	s.DB.QueryRow("SELECT count(*) FROM coverage_windows").Scan(&n)
	if n != 0 {
		t.Fatal("coverage committed without checkpoint")
	}
	if _, err := historyFinish(t, s, h, []NormalizedEvent{e}, false, "p2"); err != nil {
		t.Fatal(err)
	}
	recall := NormalizedEvent{Kind: EventRecall, Adapter: "dws", ParseVersion: "1", ProviderMessageID: "old", ConversationID: "cid:group1", RecalledAt: "2026-09-15T10:00:00Z"}
	intake(t, s, source.ChannelID, recall, "recall")
	h = historyClaim(t, s, source.ID)
	if _, err := historyFinish(t, s, h, []NormalizedEvent{e}, true, ""); err != nil {
		t.Fatal(err)
	}
	var availability string
	s.DB.QueryRow("SELECT availability FROM messages WHERE provider_message_id='old'").Scan(&availability)
	if availability != "recalled" {
		t.Fatalf("history resurrected tombstone: %s", availability)
	}
}
func TestHistoryImportCancelRetryAndLeaseFence(t *testing.T) {
	s, source, _ := historyFixture(t)
	h := historyClaim(t, s, source.ID)
	ctx := context.Background()
	_, err := s.Mutate(ctx, Request{Command: "test.cancel"}, func(tx *Tx) (any, error) { return tx.CancelHistoryImport(ctx, h.ID) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = historyFinish(t, s, h, nil, true, ""); ErrorCode(err) != "conflict" {
		t.Fatalf("cancelled step committed: %v", err)
	}
	_, err = s.Mutate(ctx, Request{Command: "test.retry"}, func(tx *Tx) (any, error) { return tx.RetryHistoryImport(ctx, h.ID) })
	if err != nil {
		t.Fatal(err)
	}
	newer := historyClaim(t, s, source.ID)
	if newer.LeaseToken == h.LeaseToken {
		t.Fatal("retry reused claim")
	}
	if _, err = historyFinish(t, s, h, nil, true, ""); ErrorCode(err) != "conflict" {
		t.Fatalf("old worker committed: %v", err)
	}
	s.DB.Exec("UPDATE history_imports SET lease_until='2000-01-01T00:00:00Z' WHERE id=?", h.ID)
	recovered := historyClaim(t, s, source.ID)
	if recovered.LeaseToken == newer.LeaseToken {
		t.Fatal("expired claim not replaced")
	}
	if _, err = historyFinish(t, s, recovered, nil, true, ""); err != nil {
		t.Fatal(err)
	}
}
func TestHistoryImportConcurrentClaimsAndFailureBackoff(t *testing.T) {
	s, source, _ := historyFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	claims := make(chan HistoryImport, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var h HistoryImport
			_, err := s.Mutate(ctx, Request{Command: "test.claim"}, func(tx *Tx) (any, error) {
				var e error
				h, e = tx.ClaimHistoryImport(ctx, source.ID, time.Now())
				return h, e
			})
			if err == nil {
				claims <- h
			} else {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(claims)
	close(errs)
	if len(claims) != 1 || len(errs) != 1 {
		t.Fatalf("claims %d errors %d", len(claims), len(errs))
	}
	h := <-claims
	var failed HistoryImport
	_, err := s.Mutate(ctx, Request{Command: "test.fail"}, func(tx *Tx) (any, error) {
		var e error
		failed, e = tx.FailHistoryImport(ctx, h, "rate_limited")
		return failed, e
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Cursor != h.Cursor || failed.ErrorCode != "rate_limited" || failed.NextAttemptAt <= Now() || failed.Failures != 1 {
		t.Fatalf("backoff %+v", failed)
	}
	_, err = s.Mutate(ctx, Request{Command: "test.early"}, func(tx *Tx) (any, error) { return tx.ClaimHistoryImport(ctx, source.ID, time.Now()) })
	if ErrorCode(err) != "not_found" {
		t.Fatalf("backoff ignored: %v", err)
	}
}
func TestHistoryImportInitialSchedulingDoesNotRecreateCancelledWork(t *testing.T) {
	s, source, h := historyFixture(t)
	ctx := context.Background()
	_, err := s.Mutate(ctx, Request{Command: "test.ensure"}, func(tx *Tx) (any, error) {
		if _, e := tx.CancelHistoryImport(ctx, h.ID); e != nil {
			return nil, e
		}
		return nil, tx.EnsureHistoryImports(ctx, source.ID, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := ListHistoryImports(ctx, s.DB, source.ID)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "cancelled" {
		t.Fatalf("jobs %+v %v", jobs, err)
	}
}

func TestDisabledAutomaticHistoryImportKeepsSourceCollecting(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, route := fixtureChannel(t, s, ChannelDwsPersonal, "cid:work")
	disabled := false
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "test.history-disabled"}, func(tx *Tx) (any, error) {
		source, err := tx.ConfigureDataSource(ctx, DataSourceInput{Name: "work", Channel: c.ID, Workspace: "global", HistoryEnabled: &disabled})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetDataSourceStatus(ctx, source.ID, "running"); err != nil {
			return nil, err
		}
		return nil, tx.EnsureHistoryImports(ctx, source.ID, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM history_imports WHERE route_id=?", route.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("disabled history was scheduled: %d %v", count, err)
	}
}
