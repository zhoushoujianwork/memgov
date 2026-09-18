package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

type countingQueryer struct {
	core.Queryer
	queries int
}

func (q *countingQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.queries++
	return q.Queryer.QueryContext(ctx, query, args...)
}

func (q *countingQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.queries++
	return q.Queryer.QueryRowContext(ctx, query, args...)
}

func fixture(t *testing.T, sent time.Time) (*Server, string, string) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	var cfg core.RuntimeConfig
	var msg core.IntakeResult
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "console.fixture"}, func(tx *core.Tx) (any, error) {
		c, e := tx.AddChannel(ctx, core.ChannelInput{Name: "test-channel", Kind: core.ChannelDwsPersonal, Identity: core.ChannelIdentity{ExpectedCorpID: "corp", ExpectedUserID: "owner"}, Route: &core.RouteInput{ConversationID: "group", ConversationType: "group"}})
		if e != nil {
			return nil, e
		}
		direct, e := tx.AddRoute(ctx, c.ID, core.RouteInput{ConversationID: "owner", ConversationType: "direct", Mode: "notify", SendPolicy: "dispatch_only"})
		if e != nil {
			return nil, e
		}
		owner := core.Sender{IDType: "staff_id", IDValue: "owner"}
		msg, e = tx.Intake(ctx, c.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "history", ProviderMessageID: "msg", ConversationID: "group", Tenant: "corp", Sender: owner, Body: "SECRET-BODY <script>alert(1)</script>", Format: "text", SentAt: sent.UTC().Format(time.RFC3339Nano), EventAt: sent.UTC().Format(time.RFC3339Nano)})
		if e != nil {
			return nil, e
		}
		cfg, e = tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Name: "owner-private", Channel: c.ID, RouteIDs: []string{c.Routes[0].ID}, DeliveryRouteID: direct.ID, Owner: owner, ItemThreshold: 1, MaxWaitSeconds: 30, ReconcileSeconds: 10})
		if e != nil {
			return nil, e
		}
		if _, e = tx.SetRuntimeStatus(ctx, cfg.ID, "stopped", ""); e != nil {
			return nil, e
		}
		if _, e = tx.SetChannelCapabilities(ctx, c.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); e != nil {
			return nil, e
		}
		history := false
		source, e := tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: "work", Channel: c.ID, Workspace: "global", HistoryEnabled: &history, RetentionDays: 7})
		if e != nil {
			return nil, e
		}
		if _, e = tx.Conn.ExecContext(ctx, "UPDATE data_sources SET route_ids=? WHERE id=?", core.JSON([]string{c.Routes[0].ID}), source.ID); e != nil {
			return nil, e
		}
		_, e = tx.Conn.ExecContext(ctx, `INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,kind,title,instructions,status,result,created_at,updated_at) VALUES('task-1',?,?,'one','task','SECRET-TITLE','SECRET-INSTRUCTIONS','running','SECRET-RESULT',?,?)`, cfg.ID, c.Routes[0].ID, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
		if e != nil {
			return nil, e
		}
		_, e = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_task_messages(task_id,message_id,revision) VALUES('task-1',?,1)", msg.MessageID)
		return nil, e
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	server, err := Open(ctx, Options{Home: home, Host: "127.0.0.1:8787", Version: "test", Build: "test-build"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server, cfg.ID, msg.MessageID
}

func request(t *testing.T, s *Server, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, strings.NewReader(body))
	if cookie != nil {
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestServiceRestartRequiresSameOriginConsoleRequest(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	if w := request(t, s, "POST", "/api/v1/service/restart", "{}", nil); w.Code != 405 {
		t.Fatal("standalone console enabled restart")
	}
	prepared, committed := 0, 0
	var handoff RestartRequest
	var response *httptest.ResponseRecorder
	s.opts.Restart = func(ctx context.Context, session RestartRequest) (func(), error) {
		prepared++
		handoff = session
		if prepared > 1 {
			return nil, core.Fail("conflict", "already requested")
		}
		return func() {
			if !response.Flushed || !strings.Contains(response.Body.String(), "restarting") {
				t.Fatal("shutdown requested before acknowledgement was flushed")
			}
			committed++
		}, nil
	}
	for _, tc := range []struct {
		name, body, origin, header string
		status                     int
	}{
		{name: "missing origin", body: "{}", header: "1", status: 403},
		{name: "foreign origin", body: "{}", origin: "http://evil.example", header: "1", status: 403},
		{name: "missing header", body: "{}", origin: "http://" + s.opts.Host, status: 403},
		{name: "invalid JSON", body: "{", origin: "http://" + s.opts.Host, header: "1", status: 400},
		{name: "accepted", body: "{}", origin: "http://" + s.opts.Host, header: "1", status: 200},
		{name: "duplicate", body: "{}", origin: "http://" + s.opts.Host, header: "1", status: 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+s.opts.Host+"/api/v1/service/restart", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-Memgov-Console", tc.header)
			response = httptest.NewRecorder()
			s.Handler().ServeHTTP(response, r)
			if response.Code != tc.status {
				t.Fatalf("status: got %d, want %d", response.Code, tc.status)
			}
			if len(response.Result().Cookies()) != 0 {
				t.Fatal("restart issued an unexpected cookie")
			}
		})
	}
	if prepared != 2 || committed != 1 || handoff.Host != s.opts.Host {
		t.Fatal("restart preparation, commit or listener handoff differed")
	}
}

func TestConsoleReopenNeedsNoSession(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	replacement, err := Open(context.Background(), s.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if w := request(t, replacement, "GET", "/api/v1/meta", "", nil); w.Code != 200 {
		t.Fatalf("replacement without cookie: %d %s", w.Code, w.Body.String())
	}
}

func TestConsoleRejectsNonLoopbackHost(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	for _, host := range []string{"0.0.0.0:8787", "192.168.1.1:8787", "example.com:8787", "localhost:8787", "127.0.0.1"} {
		opts := s.opts
		opts.Host = host
		server, err := Open(context.Background(), opts)
		if err == nil {
			server.Close()
			t.Fatalf("accepted host %s", host)
		}
	}
}

func TestTaskContinuationRequiresSameOriginAndCurrentVersion(t *testing.T) {
	s, runtimeID, _ := fixture(t, time.Now())
	if w := request(t, s, "POST", "/api/v1/tasks/task-1/resume", `{"expected_version":1}`, nil); w.Code != 405 {
		t.Fatal("diagnostic console enabled continuation")
	}
	store, err := core.Open(context.Background(), filepath.Join(s.opts.Home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.DB.Exec("UPDATE runtime_tasks SET status='failed',error_code='runtime_restarted' WHERE id='task-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec("INSERT INTO runtime_attempts(id,task_id,task_version,status,model,workspace_dir,input_digest,started_at,finished_at) VALUES(?,'task-1',1,'failed','model',?,'digest',?,?)", core.NewID(), filepath.Join(s.opts.Home, "runtime", "tasks", "task-1", "old"), core.Now(), core.Now()); err != nil {
		t.Fatal(err)
	}
	s.opts.ResumeTask = func(ctx context.Context, id string, version int) (any, error) {
		result, err := store.Mutate(ctx, core.Request{Scope: "global", Actor: "console-owner", Command: "runtime.task.resume"}, func(tx *core.Tx) (any, error) {
			return tx.ResumeRuntimeTask(ctx, id, version)
		})
		if err != nil {
			return nil, err
		}
		return result.Data, nil
	}
	if w := request(t, s, "GET", "/api/v1/tasks/task-1", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"can_resume":true`) || !strings.Contains(w.Body.String(), `"resume_mode":"replay"`) {
		t.Fatal("resumable task did not advertise its continuation control")
	}
	for _, tc := range []struct {
		name, body, origin, header string
		status                     int
	}{
		{name: "no origin", body: `{"expected_version":1}`, header: "1", status: 403},
		{name: "no header", body: `{"expected_version":1}`, origin: "http://" + s.opts.Host, status: 403},
		{name: "no version", body: `{}`, origin: "http://" + s.opts.Host, header: "1", status: 400},
		{name: "stale version", body: `{"expected_version":2}`, origin: "http://" + s.opts.Host, header: "1", status: 409},
		{name: "accepted", body: `{"expected_version":1}`, origin: "http://" + s.opts.Host, header: "1", status: 200},
		{name: "duplicate", body: `{"expected_version":1}`, origin: "http://" + s.opts.Host, header: "1", status: 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+s.opts.Host+"/api/v1/tasks/task-1/resume", strings.NewReader(tc.body))
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Memgov-Console", tc.header)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status: got %d, want %d", w.Code, tc.status)
			}
		})
	}
	task, err := core.ReadRuntimeTask(context.Background(), store.DB, "task-1")
	if err != nil || task.Status != "pending" || task.Version != 2 || task.Resume == nil || task.Resume.Prompt != "继续" {
		t.Fatal("Web continuation did not queue the same task")
	}
	config, err := core.ReadRuntime(context.Background(), store.DB, runtimeID)
	if err != nil || config.Status != "running" {
		t.Fatal("Web continuation did not wake the stopped module")
	}
	if _, err := s.db.Exec("UPDATE runtime_tasks SET status='failed'"); err == nil {
		t.Fatal("console query connection lost its read-only constraint")
	}
}
func TestDirectLoopbackAccessOriginAndReadOnly(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	if w := request(t, s, "GET", "/api/v1/tasks", "", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"meta", "runtimes", "tasks", "tasks/task-1", "sources", "agents", "tasks/task-1/logs"} {
		if w := request(t, s, "GET", "/api/v1/"+path, "", nil); w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		} else if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cacheable private response")
		}
	}
	if w := request(t, s, "POST", "/api/v1/tasks", "{}", nil); w.Code != 405 {
		t.Fatal("write endpoint enabled")
	}
	if w := request(t, s, "POST", "/api/v1/session", `{"token":"legacy"}`, nil); w.Code != 405 {
		t.Fatal("legacy login endpoint still enabled")
	}
	for _, change := range []func(*http.Request){func(r *http.Request) { r.Host = "evil.example" }, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/v1/tasks", nil)
		change(r)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("foreign request accepted")
		}
	}
	if _, err := s.db.Exec("UPDATE runtime_tasks SET status='completed'"); err == nil {
		t.Fatal("database connection permits writes")
	}
	var status string
	s.db.QueryRow("SELECT status FROM runtime_tasks WHERE id='task-1'").Scan(&status)
	if status != "running" {
		t.Fatal("read changed business state")
	}
	if s.URL() != "http://127.0.0.1:8787/" {
		t.Fatal("startup URL requires a token")
	}
	oldCookie := &http.Cookie{Name: "memgov_console_legacy", Value: "expired"}
	if w := request(t, s, "GET", "/api/v1/meta", "", oldCookie); w.Code != 200 || len(w.Result().Cookies()) != 0 {
		t.Fatal("legacy cookie interfered with direct access")
	}
}

func TestRuntimesUseBoundedSnapshotAndShortCache(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingQueryer{Queryer: tx}
	views, err := s.runtimes(ctx, counting)
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if counting.queries != 3 {
		t.Fatalf("runtime snapshot used %d queries, want 3 independent of runtime count", counting.queries)
	}
	if len(views) != 1 || views[0].Tasks["running"] != 1 {
		t.Fatalf("unexpected runtime snapshot: %+v", views)
	}

	read := func() RuntimeView {
		t.Helper()
		response := request(t, s, "GET", "/api/v1/runtimes", "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("runtime response: %d %s", response.Code, response.Body.String())
		}
		var envelope struct {
			Data []RuntimeView `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Data) != 1 {
			t.Fatalf("decode runtime response: %v %+v", err, envelope)
		}
		return envelope.Data[0]
	}
	if got := read(); got.Tasks["running"] != 1 {
		t.Fatalf("initial cached snapshot: %+v", got)
	}

	store, err := core.Open(ctx, filepath.Join(s.opts.Home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.runtime.cache"}, func(tx *core.Tx) (any, error) {
		_, updateErr := tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status='completed',updated_at=? WHERE id='task-1'", core.Now())
		return nil, updateErr
	})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Tasks["running"] != 1 || got.Tasks["completed"] != 0 {
		t.Fatalf("short cache did not retain its snapshot: %+v", got)
	}
	s.runtimesMu.Lock()
	s.runtimesExpires = time.Time{}
	s.runtimesMu.Unlock()
	if got := read(); got.Tasks["completed"] != 1 || got.Tasks["running"] != 0 {
		t.Fatalf("expired cache was not refreshed: %+v", got)
	}
}

func TestLogsAreTaskScopedAndNeverExposeSummaries(t *testing.T) {
	s, runtimeID, _ := fixture(t, time.Now())
	dir := filepath.Join(s.opts.Home, "runtime", "logs", runtimeID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for _, event := range []runlog.Event{
		{Timestamp: "2026-09-16T00:00:00Z", RuntimeID: runtimeID, TaskID: "task-1", Component: "agent", Event: "attempt.finished", Summary: "PRIVATE-LOG-QUOTE"},
		{Timestamp: "2026-09-16T00:00:01Z", RuntimeID: runtimeID, TaskID: "other-task", Event: "OTHER-TASK-SECRET"},
		{Timestamp: "2026-09-16T00:00:02Z", RuntimeID: "other-runtime", TaskID: "task-1", Event: "OTHER-RUNTIME-SECRET"},
	} {
		value, _ := json.Marshal(event)
		body.Write(value)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "test.jsonl"), []byte(body.String()), 0600); err != nil {
		t.Fatal(err)
	}
	w := request(t, s, "GET", "/api/v1/tasks/task-1/logs", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "attempt.finished") || strings.Contains(w.Body.String(), "SECRET") || strings.Contains(w.Body.String(), "PRIVATE-LOG") {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "GET", "/api/v1/tasks/task-1/logs?since=2026-09-16T00:00:00Z", "", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "attempt.finished") {
		t.Fatal(w.Body.String())
	}
}

func TestAgentEditorRequiresSameOriginJSONAndKeepsDBReadOnly(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	called := 0
	s.opts.AgentConfig = &AgentConfigAccess{
		List: func(context.Context, core.Queryer) (any, error) { return map[string]any{"items": []any{}}, nil },
		Preview: func(context.Context, core.Queryer, json.RawMessage) (any, error) {
			return map[string]bool{"preview": true}, nil
		},
		Save: func(context.Context, core.Queryer, json.RawMessage) (any, error) {
			called++
			return map[string]bool{"saved": true}, nil
		},
	}
	if w := request(t, s, "POST", "/api/v1/agent-config/save", "{}", nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
	for _, headers := range []map[string]string{
		{},
		{"Origin": "http://127.0.0.1:8787", "Content-Type": "application/json"},
		{"Origin": "http://127.0.0.1:8787", "Content-Type": "text/plain", "X-Memgov-Console": "1"},
		{"Origin": "http://evil.example", "Content-Type": "application/json", "X-Memgov-Console": "1"},
		{"Origin": "http://127.0.0.1:8787", "Content-Type": "application/json", "X-Memgov-Console": "1"},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/v1/agent-config/save", strings.NewReader("{}"))
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if len(headers) == 3 && headers["Origin"] == "http://127.0.0.1:8787" && headers["Content-Type"] == "application/json" {
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
		} else if w.Code != 403 {
			t.Fatal("cross-origin edit accepted", headers, w.Code)
		}
	}
	if called != 1 {
		t.Fatal("unauthorized edit reached handler", called)
	}
	if _, err := s.db.Exec("UPDATE runtime_tasks SET status='completed'"); err == nil {
		t.Fatal("editor enabled database writes")
	}
	if w := request(t, s, "DELETE", "/api/v1/tasks/task-1", "", nil); w.Code != 405 {
		t.Fatal("unrelated task mutation enabled")
	}
}
func TestTaskStatusExpiryAndPagination(t *testing.T) {
	s, _, mid := fixture(t, time.Now().AddDate(0, 0, -8))
	for _, path := range []string{"/api/v1/tasks", "/api/v1/tasks/task-1"} {
		w := request(t, s, "GET", path, "", nil)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		for _, secret := range []string{"SECRET-BODY", "SECRET-TITLE", "SECRET-INSTRUCTIONS", "SECRET-RESULT"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("expired content leaked: %s", w.Body.String())
			}
		}
		if !strings.Contains(w.Body.String(), `"redacted":true`) || !strings.Contains(w.Body.String(), "stopped_record") {
			t.Fatal(w.Body.String())
		}
	}
	var original string
	if err := s.db.QueryRow("SELECT body FROM message_revisions WHERE message_id=?", mid).Scan(&original); err != nil || !strings.Contains(original, "SECRET") {
		t.Fatalf("test didn't cover pre-cleanup expiry: %s %v", original, err)
	}
	for _, query := range []string{"limit=-1", "cursor=BAD!", "status=no-such-status"} {
		if w := request(t, s, "GET", "/api/v1/tasks?"+query, "", nil); w.Code != 400 {
			t.Fatal(query, w.Code)
		}
	}
	// A new insertion before the cursor must not duplicate the previous page.
	writer, err := core.Open(context.Background(), filepath.Join(s.opts.Home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	_, err = writer.DB.Exec(`INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,kind,title,instructions,status,created_at,updated_at) SELECT 'task-2',runtime_id,route_id,'two',kind,'Second','','running','2020-01-01T00:00:00Z','2020-01-01T00:00:00Z' FROM runtime_tasks WHERE id='task-1'`)
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, s, "GET", "/api/v1/tasks?limit=1", "", nil)
	var envelope struct {
		Data struct {
			Tasks []TaskCard `json:"tasks"`
			Next  string     `json:"next_cursor"`
		} `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Data.Next == "" {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "GET", "/api/v1/tasks?limit=1&cursor="+envelope.Data.Next, "", nil)
	if !strings.Contains(w.Body.String(), "task-2") || strings.Contains(w.Body.String(), "task-1") {
		t.Fatal(w.Body.String())
	}
	if !strings.Contains(shellQuote("a'$(x)"), "'\"'\"'") {
		t.Fatal("unsafe command quote")
	}
}
func TestOpenNeverInitializesOrMigrates(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	if _, err := Open(context.Background(), Options{Home: home, Host: "127.0.0.1:8787"}); core.ErrorCode(err) != "not_found" {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("created home")
	}
	s, _, _ := fixture(t, time.Now())
	writer, err := core.Open(context.Background(), filepath.Join(s.opts.Home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.DB.Exec("DELETE FROM schema_migrations WHERE version=?", core.SchemaVersion); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if _, err = Open(context.Background(), Options{Home: s.opts.Home, Host: s.opts.Host}); core.ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	var version int
	s.db.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version)
	if version == core.SchemaVersion {
		t.Fatal("UI migrated schema")
	}
}

func TestConsolePagePaths(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	for _, path := range []string{"/", "/tasks", "/memories", "/running", "/settings", "/tasks/", "/memories/", "/running/", "/settings/", "/memories?q=中文"} {
		for _, method := range []string{"GET", "HEAD"} {
			w := request(t, s, method, path, "", nil)
			if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
			}
			if method == "GET" && (!strings.Contains(w.Body.String(), `src="/assets/`) || !strings.Contains(w.Body.String(), `id="root"`)) {
				t.Fatal("page did not serve the application with root-relative assets and links")
			}
		}
	}
	index := request(t, s, "GET", "/tasks", "", nil)
	refs := regexp.MustCompile(`(?:src|href)="(/assets/[^"\s]+)"`).FindAllStringSubmatch(index.Body.String(), -1)
	if len(refs) < 2 {
		t.Fatal("production scripts and styles were not embedded")
	}
	for _, ref := range refs {
		for _, method := range []string{"GET", "HEAD"} {
			w := request(t, s, method, ref[1], "", nil)
			if w.Code != 200 || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %s: %d", method, ref[1], w.Code)
			}
		}
	}
	for _, path := range []string{"/unknown", "/memories/private", "/missing.js", "/assets/", "/src/main.tsx", "/assets/../index.html", "/assets/missing.js"} {
		if w := request(t, s, "GET", path, "", nil); w.Code != 404 {
			t.Fatalf("unknown path %s: %d", path, w.Code)
		}
	}
	if w := request(t, s, "POST", "/memories", "{}", nil); w.Code != 405 {
		t.Fatal("page route accepted a write")
	}
}
