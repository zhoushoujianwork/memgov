package dws

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func historyArg(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func TestHistoryTypedCursorUsesOnlyBoundedTimeInputs(t *testing.T) {
	start := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	window := channel.Window{ConversationID: "cid:group1", Start: start, End: start.Add(time.Hour)}
	var requests [][]string
	a := &Adapter{Run: func(ctx context.Context, args ...string) ([]byte, error) {
		requests = append(requests, append([]string{}, args...))
		return envelope(fmt.Sprintf(`{"messages":[{"messageId":"m1","text":"one","time":"2026-09-14T08:10:00Z","senderId":"alice"}],"complete":false,"hasMore":true,"stopReason":"result_limit","count":200,"nextPage":{"direction":"newer","nextCursor":%d,"time":"2026-09-14T08:10:00.123Z"}}`, start.Add(10*time.Minute+123*time.Millisecond).UnixMilli())), nil
	}}
	out, err := a.ReadWindow(context.Background(), testConfig(), window)
	if err != nil || out.Complete || out.NextCursor == "" {
		t.Fatalf("typed cursor %+v %v", out, err)
	}
	window.Cursor = out.NextCursor
	out, err = a.ReadWindow(context.Background(), testConfig(), window)
	if err != nil || out.Complete || out.NextCursor != "" || out.StopReason != "cursor_not_advancing" {
		t.Fatalf("repeated boundary %+v %v", out, err)
	}
	if historyArg(requests[1], "--start") != "2026-09-14T08:10:00.123Z" || historyArg(requests[1], "--end") != window.End.Format(time.RFC3339) || strings.Contains(strings.Join(requests[1], " "), "page-token") {
		t.Fatalf("unsafe continuation argv: %v", requests[1])
	}
	for _, mutate := range []func(*channel.Window){
		func(w *channel.Window) { w.Cursor = "old-opaque-token" },
		func(w *channel.Window) { w.ConversationID = "other-group" },
		func(w *channel.Window) { w.End = w.End.Add(time.Hour) },
		func(w *channel.Window) { w.Start = w.Start.Add(-time.Hour) },
	} {
		altered := window
		mutate(&altered)
		if _, err = a.ReadWindow(context.Background(), testConfig(), altered); core.ErrorCode(err) != "invalid_input" {
			t.Fatalf("foreign checkpoint accepted: %v", err)
		}
	}
	if len(requests) != 2 {
		t.Fatal("invalid checkpoints called DWS")
	}
}

func historyStoreFixture(t *testing.T) (*core.Store, core.DataSource, core.HistoryImport, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := core.Open(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var source core.DataSource
	var job core.HistoryImport
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "test.history.setup"}, func(tx *core.Tx) (any, error) {
		c, e := tx.AddChannel(ctx, core.ChannelInput{Name: "history-dws", Kind: core.ChannelDwsPersonal, Identity: core.ChannelIdentity{ExpectedCorpID: "corp", ExpectedUserID: "owner"}, Route: &core.RouteInput{ConversationID: "history-group", ConversationType: "group"}})
		if e != nil {
			return nil, e
		}
		if _, e = tx.SetChannelCapabilities(ctx, c.ID, core.Capabilities{Verified: map[string]bool{"history": true}}, "fake"); e != nil {
			return nil, e
		}
		source, e = tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: "history", Channel: c.ID, Workspace: "global"})
		if e != nil {
			return nil, e
		}
		// The cursor fixture starts after successful group discovery.
		source, e = tx.SyncDataSourceGroups(ctx, source.ID, []string{c.Routes[0].ConversationID})
		if e != nil {
			return nil, e
		}
		source, e = tx.SetDataSourceStatus(ctx, source.ID, "running")
		if e != nil {
			return nil, e
		}
		start := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
		job, e = tx.CreateHistoryImport(ctx, source.ID, c.Routes[0].ID, start, start.Add(time.Hour))
		return job, e
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, source, job, path
}

func TestHistoryTimeCursorImportsOver200AndResumesAfterInterruptedProcess(t *testing.T) {
	s, source, job, path := historyStoreFixture(t)
	ctx := context.Background()
	start, _ := time.Parse(time.RFC3339, job.StartAt)
	calls := []string{}
	interrupt := false
	a := &Adapter{Run: func(ctx context.Context, args ...string) ([]byte, error) {
		from := historyArg(args, "--start")
		calls = append(calls, from)
		if historyArg(args, "--page-limit") != "5" || historyArg(args, "--max-items") != "100" || historyArg(args, "--end") != job.EndAt || historyArg(args, "--page-token") != "" {
			t.Fatalf("unbounded history request: %v", args)
		}
		if interrupt {
			interrupt = false
			return nil, context.Canceled
		}
		requestStart, _ := time.Parse(time.RFC3339, from)
		messages := []map[string]string{}
		remaining := 0
		for i := 0; i < 420; i++ {
			stamp := start.Add(time.Duration(10+i*2) * time.Second)
			if !stamp.Before(requestStart) {
				remaining++
				if len(messages) < 100 {
					messages = append(messages, map[string]string{"messageId": fmt.Sprintf("m-%03d", i), "text": "historical text", "time": stamp.Format(time.RFC3339), "senderId": "alice"})
				}
			}
		}
		more := remaining > len(messages)
		response := map[string]any{"messages": messages, "complete": !more, "hasMore": more}
		if more {
			response["stopReason"] = "result_limit"
			last, _ := time.Parse(time.RFC3339, messages[len(messages)-1]["time"])
			response["nextPage"] = map[string]any{"direction": "newer", "nextCursor": last.UnixMilli(), "time": last.Format(time.RFC3339Nano)}
		}
		raw, _ := json.Marshal(response)
		return envelope(string(raw)), nil
	}}
	worker := channel.HistoryImportWorker{Collector: channel.Collector{Store: s, Adapter: a}, Request: core.Request{Scope: "global"}}
	if ran, err := worker.Step(ctx, source.ID); !ran || err != nil {
		t.Fatalf("first step %v %v", ran, err)
	}
	partial, err := core.ReadHistoryImport(ctx, s.DB, job.ID)
	if err != nil || partial.Status != "queued" || partial.Applied != 100 || partial.Cursor == "" {
		t.Fatalf("checkpoint %+v %v", partial, err)
	}
	interrupt = true
	if _, err = worker.Step(ctx, source.ID); err == nil {
		t.Fatal("interruption hidden")
	}
	interrupted, _ := core.ReadHistoryImport(ctx, s.DB, job.ID)
	if interrupted.Cursor != partial.Cursor || interrupted.Applied != 100 {
		t.Fatal("failed read advanced the checkpoint")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	collector := channel.Collector{Store: reopened, Adapter: a}
	for i := 0; i < 4; i++ {
		var claim core.HistoryImport
		_, err = reopened.Mutate(ctx, core.Request{Scope: "global", Command: "test.history.restart.claim"}, func(tx *core.Tx) (any, error) {
			var e error
			claim, e = tx.ClaimHistoryImport(ctx, source.ID, time.Now().Add(time.Hour))
			return claim, e
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = collector.PullImport(ctx, core.Request{Scope: "global"}, claim); err != nil {
			t.Fatal(err)
		}
	}
	complete, err := core.ReadHistoryImport(ctx, reopened.DB, job.ID)
	if err != nil || complete.Status != "completed" || complete.Applied != 420 || complete.Duplicates != 4 || complete.Events != 424 || calls[1] != calls[2] {
		t.Fatalf("resumed history %+v calls=%v err=%v", complete, calls, err)
	}
	var contexts, gaps int
	reopened.DB.QueryRow("SELECT count(*) FROM messages WHERE context_only=1").Scan(&contexts)
	reopened.DB.QueryRow("SELECT count(*) FROM coverage_windows WHERE complete=0").Scan(&gaps)
	if contexts != 420 || gaps != 0 {
		t.Fatalf("context/coverage %d %d", contexts, gaps)
	}
}

func TestHistoryDenseTimestampAndPermissionFailureRemainIncomplete(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprintf("denied=%v", denied), func(t *testing.T) {
			s, source, job, _ := historyStoreFixture(t)
			calls := 0
			a := &Adapter{Run: func(ctx context.Context, args ...string) ([]byte, error) {
				calls++
				if denied {
					return []byte(`{"error":{"code":"permission_denied","message":"private raw error"}}`), nil
				}
				messages := []map[string]string{}
				for i := 0; i < 100; i++ {
					messages = append(messages, map[string]string{"messageId": fmt.Sprintf("dense-%d", i), "text": "text", "time": "2026-09-14T08:00:01Z", "senderId": "alice"})
				}
				raw, _ := json.Marshal(map[string]any{"messages": messages, "complete": false, "hasMore": true, "stopReason": "result_limit", "nextPage": map[string]any{"direction": "newer", "nextCursor": time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC).UnixMilli(), "time": "2026-09-14T08:00:00Z"}})
				return envelope(string(raw)), nil
			}}
			w := channel.HistoryImportWorker{Collector: channel.Collector{Store: s, Adapter: a}, Request: core.Request{Scope: "global"}}
			if ran, err := w.Step(context.Background(), source.ID); !ran || err == nil {
				t.Fatalf("unsafe step accepted ran=%v err=%v", ran, err)
			}
			failed, err := core.ReadHistoryImport(context.Background(), s.DB, job.ID)
			expected := "history_cursor_stalled"
			if denied {
				expected = "denied"
			}
			if err != nil || failed.Status != "failed" || failed.Cursor != "" || failed.ErrorCode != expected {
				t.Fatalf("failure state %+v %v", failed, err)
			}
			var complete, saved int
			s.DB.QueryRow("SELECT count(*) FROM coverage_windows WHERE complete=1").Scan(&complete)
			s.DB.QueryRow("SELECT count(*) FROM messages").Scan(&saved)
			if complete != 0 || (!denied && saved != 100) || (denied && saved != 0) {
				t.Fatalf("partial state complete=%d saved=%d", complete, saved)
			}
			if ran, err := w.Step(context.Background(), source.ID); ran || err != nil || calls != 1 {
				t.Fatalf("failed import was retried automatically %v %v calls=%d", ran, err, calls)
			}
		})
	}
}

func TestHistoryParsesDWSFixedCSTAndRecognizesRangeEnd(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	w := channel.Window{ConversationID: "cid:group1", Start: time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)}
	a := &Adapter{Run: func(ctx context.Context, args ...string) ([]byte, error) {
		return envelope(`{"messages":[{"messageId":"m","text":"text","createTime":"2026-09-15 17:50:20","senderId":"alice"}],"complete":true,"hasMore":false,"stopReason":"range_end"}`), nil
	}}
	out, err := a.ReadWindow(context.Background(), testConfig(), w)
	if err != nil || !out.Complete || len(out.Events) != 1 || out.Events[0].SentAt != "2026-09-15T09:50:20Z" || out.Events[0].HistoryPreviousSentAt != "2026-09-15T17:50:20Z" {
		t.Fatalf("CST output %+v %v", out, err)
	}
}

func TestHistoryMillisecondCursorAdvancesWithinSameDisplayedSecond(t *testing.T) {
	start := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	w := channel.Window{ConversationID: "cid:group1", Start: start, End: start.Add(time.Hour)}
	calls := 0
	a := &Adapter{Run: func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		ms := start.Add(time.Duration(calls) * 200 * time.Millisecond)
		return envelope(fmt.Sprintf(`{"messages":[{"messageId":"m%d","text":"text","createTime":"2026-09-15 16:00:00","senderId":"alice"}],"complete":false,"hasMore":true,"stopReason":"result_limit","nextPage":{"direction":"newer","nextCursor":%d,"time":%q}}`, calls, ms.UnixMilli(), ms.Format(time.RFC3339Nano))), nil
	}}
	for i := 0; i < 2; i++ {
		out, err := a.ReadWindow(context.Background(), testConfig(), w)
		if err != nil || out.Complete || out.NextCursor == "" {
			t.Fatalf("step %d %+v %v", i, out, err)
		}
		w.Cursor = out.NextCursor
	}
	resume, err := historyRequestStart(w)
	if err != nil || !resume.Equal(start.Add(400*time.Millisecond)) {
		t.Fatalf("millisecond checkpoint %v %v", resume, err)
	}
}
