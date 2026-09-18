package console

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/tasklog"
)

func terminalFixture(t *testing.T, status string) (*Server, *core.Store, *tasklog.Writer, string) {
	t.Helper()
	s, runtimeID, _ := fixture(t, time.Now())
	store, err := core.Open(context.Background(), filepath.Join(s.opts.Home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	attempt := core.NewID()
	if _, err = store.DB.Exec("INSERT INTO runtime_attempts(id,task_id,task_version,status,model,input_digest,started_at) VALUES(?,'task-1',1,?,'fake','digest',?)", attempt, status, core.Now()); err != nil {
		t.Fatal(err)
	}
	w, err := tasklog.Open(s.opts.Home, runtimeID, "task-1", attempt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return s, store, w, attempt
}

func TestTerminalSameOriginAttemptCursorAndSourceBarriers(t *testing.T) {
	s, store, w, attempt := terminalFixture(t, "completed")
	w.Emit("assistant", "公开执行进度 <script>alert(1)</script>")
	w.Emit("tool", "Read file.txt")
	tail, err := tasklog.Read(s.opts.Home, mustRuntimeID(t, s), "task-1", attempt, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, origin, cursor string
		status                     int
		want, absent               string
	}{
		{name: "no cookie", path: "/api/v1/tasks/task-1/terminal", status: 200, want: "公开执行进度"},
		{name: "cross origin", path: "/api/v1/tasks/task-1/terminal", origin: "http://foreign.invalid", status: 403},
		{name: "wrong attempt", path: "/api/v1/tasks/task-1/terminal?attempt=other", status: 404},
		{name: "invalid cursor", path: "/api/v1/tasks/task-1/terminal?cursor=-1", status: 400},
		{name: "history", path: "/api/v1/tasks/task-1/terminal", status: 200, want: "公开执行进度"},
		{name: "cursor", path: "/api/v1/tasks/task-1/terminal", cursor: fmt.Sprint(tail.Events[0].Offset), status: 200, want: "Read file.txt", absent: "公开执行进度"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://"+s.opts.Host+tc.path, nil)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Last-Event-ID", tc.cursor)
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, r)
			if response.Code != tc.status || tc.want != "" && !strings.Contains(response.Body.String(), tc.want) || tc.absent != "" && strings.Contains(response.Body.String(), tc.absent) {
				t.Fatalf("response %d %s", response.Code, response.Body.String())
			}
			if tc.status == 200 && (!strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(response.Body.String(), "event: finished")) {
				t.Fatal("terminal not an SSE stream")
			}
		})
	}
	if _, err = store.DB.Exec("UPDATE messages SET availability='recalled'"); err != nil {
		t.Fatal(err)
	}
	response := request(t, s, "GET", "/api/v1/tasks/task-1/terminal", "", nil)
	if response.Code != 403 || strings.Contains(response.Body.String(), "公开执行进度") {
		t.Fatal("recalled source output visible")
	}
}

func mustRuntimeID(t *testing.T, s *Server) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow("SELECT runtime_id FROM runtime_tasks WHERE id='task-1'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTerminalLiveAppendAndRevocation(t *testing.T) {
	s, store, w, _ := terminalFixture(t, "running")
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/tasks/task-1/terminal", nil)
	r.Host = s.opts.Host
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	lines := make(chan string, 100)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	wait := func(match string) {
		t.Helper()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended before %s", match)
				}
				if strings.Contains(line, match) {
					return
				}
			case <-ctx.Done():
				t.Fatalf("stream did not emit %s", match)
			}
		}
	}
	wait("event: status")
	w.Emit("assistant", "调用仍在运行，实时进度已到达")
	wait("实时进度已到达")
	// A fresh read catches revocation while this connection remains open.
	if _, err = store.DB.Exec("UPDATE messages SET availability='recalled'"); err != nil {
		t.Fatal(err)
	}
	wait("event: invalidated")
}

func TestTerminalExpiredOutputAndAttemptIsolation(t *testing.T) {
	s, _, w, attempt := terminalFixture(t, "completed")
	w.Emit("assistant", "OLD-OUTPUT")
	w.Close()
	path := filepath.Join(s.opts.Home, "runtime", "output", mustRuntimeID(t, s), "task-1", attempt+".jsonl")
	old := time.Now().Add(-tasklog.Retention - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	response := request(t, s, "GET", "/api/v1/tasks/task-1/terminal", "", nil)
	if response.Code != 200 || strings.Contains(response.Body.String(), "OLD-OUTPUT") || !strings.Contains(response.Body.String(), `"available":false`) {
		t.Fatal("expired trace displayed")
	}
}
