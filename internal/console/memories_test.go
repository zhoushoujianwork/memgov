package console

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func saveConsoleMemory(t *testing.T, store *core.Store, scope, title, category, status string, source *core.Source) core.Memory {
	t.Helper()
	ctx := context.Background()
	var memory core.Memory
	_, err := store.Mutate(ctx, core.Request{Scope: scope, Command: "console.memory.fixture"}, func(tx *core.Tx) (any, error) {
		var s core.Source
		if source != nil {
			s = *source
		} else {
			var e error
			s, e = tx.Ingest(ctx, core.SourceInput{Kind: "note", URI: "fixture://" + title, Content: "可复用经验的证据原文"})
			if e != nil {
				return nil, e
			}
		}
		f := s.Fragments[0]
		m := core.Memory{Category: category, Title: title, Summary: "摘要中的发布经验", Content: "# 核对\n\n只在正文里出现的中文检索词。\n\n**完整内容**不截断。\n\n前提：已核对。步骤：按证据执行。验证：核对结果。", Status: status, WorkspaceID: scope, Evidence: []core.Evidence{{SourceID: s.ID, FragmentID: f.ID, SHA256: f.SHA256, Quote: f.Content}}}
		items, _, e := tx.SaveMemories(ctx, "memory.fixture", "浏览验收", []core.Memory{m}, map[string]int{})
		if e == nil {
			memory = items[0]
		}
		return items, e
	})
	if err != nil {
		t.Fatal(err)
	}
	return memory
}

func getMemoryData(t *testing.T, s *Server, path string) map[string]json.RawMessage {
	t.Helper()
	w := request(t, s, "GET", path, "", nil)
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	var payload struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Data
}

func TestMemoryCardsSearchScopePaginationAndReadOnly(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	store, err := core.Open(context.Background(), filepath.Join(s.opts.Home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []string{"project-a", "project-b"} {
		if _, err = store.DB.Exec("INSERT INTO workspaces VALUES(?,?,NULL,?)", id, id, core.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 30; i++ {
		saveConsoleMemory(t, store, "global", fmt.Sprintf("记忆%02d", i), "fact", "active", nil)
	}
	private := saveConsoleMemory(t, store, "project-a", "A 专属", "decision", "active", nil)
	saveConsoleMemory(t, store, "project-b", "B 专属", "lesson", "active", nil)
	saveConsoleMemory(t, store, "global", "退役记忆", "fact", "retired", nil)
	if _, err = store.DB.Exec("UPDATE memories SET updated_at='2026-09-01T00:00:00Z'"); err != nil {
		t.Fatal(err)
	}
	var countBefore int
	store.DB.QueryRow("SELECT count(*) FROM operations").Scan(&countBefore)
	for _, tc := range []struct {
		query        string
		total, items int
	}{
		{"", 30, 24}, {"?page=2", 30, 6}, {"?page=999", 30, 6}, {"?workspace=project-a", 31, 24}, {"?all_workspaces=true", 32, 24}, {"?status=all", 31, 24},
		{"?q=中文检索词", 30, 24}, {"?q=发布经验", 30, 24}, {"?q=记忆00", 1, 1}, {"?q=%25", 0, 0}, {"?workspace=project-a&category=decision", 1, 1}, {"?status=retired", 1, 1},
	} {
		d := getMemoryData(t, s, "/api/v1/memories"+tc.query)
		var total int
		var cards []map[string]any
		json.Unmarshal(d["total"], &total)
		json.Unmarshal(d["cards"], &cards)
		if total != tc.total || len(cards) != tc.items {
			t.Fatalf("%s: total=%d cards=%d", tc.query, total, len(cards))
		}
	}
	first := getMemoryData(t, s, "/api/v1/memories")
	again := getMemoryData(t, s, "/api/v1/memories")
	if string(first["cards"]) != string(again["cards"]) {
		t.Fatal("unstable tied timestamps")
	}
	var page1, page2 []struct {
		ID string `json:"id"`
	}
	json.Unmarshal(first["cards"], &page1)
	json.Unmarshal(getMemoryData(t, s, "/api/v1/memories?page=2")["cards"], &page2)
	seen := map[string]bool{}
	for _, c := range append(page1, page2...) {
		if seen[c.ID] {
			t.Fatal("duplicate across pages")
		}
		seen[c.ID] = true
	}
	for _, path := range []string{"/api/v1/memories?status=active%7Cdisputed", "/api/v1/memories?category=unknown", "/api/v1/memories?page=0", "/api/v1/memories?all_workspaces=1", "/api/v1/memories/" + private.ID + "?version=0"} {
		if w := request(t, s, "GET", path, "", nil); w.Code != 400 {
			t.Fatalf("invalid query accepted: %s %d", path, w.Code)
		}
	}
	for _, suffix := range []string{"", "/history", "/sources"} {
		path := "/api/v1/memories/" + private.ID + suffix
		if w := request(t, s, "GET", path, "", nil); w.Code != 404 {
			t.Fatal("private memory crossed global scope", w.Code)
		}
		getMemoryData(t, s, path+"?workspace=project-a")
		if w := request(t, s, "GET", path+"?workspace=project-b", "", nil); w.Code != 404 {
			t.Fatal("private memory crossed workspace", w.Code)
		}
	}
	if w := request(t, s, "GET", "/api/v1/memories", "", nil); w.Code != 200 {
		t.Fatal("direct memory read failed")
	}
	if w := request(t, s, "POST", "/api/v1/memories", "{}", nil); w.Code != 405 {
		t.Fatal("memory write enabled")
	}
	var countAfter int
	store.DB.QueryRow("SELECT count(*) FROM operations").Scan(&countAfter)
	if countBefore != countAfter {
		t.Fatal("browsing wrote operations")
	}
}

func TestMemoryEvidenceAndHistoryRecheckAvailability(t *testing.T) {
	for _, mode := range []string{"expired", "recalled", "redacted"} {
		t.Run(mode, func(t *testing.T) {
			s, _, mid := fixture(t, time.Now())
			store, err := core.Open(context.Background(), filepath.Join(s.opts.Home, "state.db"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var sid string
			if err = store.DB.QueryRow("SELECT source_id FROM source_origins WHERE message_id=?", mid).Scan(&sid); err != nil {
				t.Fatal(err)
			}
			source, err := core.ReadSource(context.Background(), store.DB, sid, "global")
			if err != nil {
				t.Fatal(err)
			}
			memory := saveConsoleMemory(t, store, "global", "保留结论", "lesson", "active", &source)
			d := getMemoryData(t, s, "/api/v1/memories/"+memory.ID)
			if !strings.Contains(string(d["memory"]), "SECRET-BODY") {
				t.Fatal("available evidence missing")
			}
			if string(d["expires_at"]) == `""` {
				t.Fatal("missing expiry timer")
			}
			switch mode {
			case "expired":
				_, err = store.DB.Exec("UPDATE messages SET sent_at=? WHERE id=?", time.Now().AddDate(0, 0, -8).Format(time.RFC3339Nano), mid)
			case "recalled":
				_, err = store.DB.Exec("UPDATE messages SET availability='recalled' WHERE id=?", mid)
			case "redacted":
				_, err = store.DB.Exec("UPDATE sources SET redacted=1 WHERE id=?", sid)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "/sources", "/history"} {
				w := request(t, s, "GET", "/api/v1/memories/"+memory.ID+suffix, "", nil)
				if w.Code != 200 || strings.Contains(w.Body.String(), "SECRET-BODY") {
					t.Fatalf("%s leaked unavailable source: %s", suffix, w.Body.String())
				}
			}
		})
	}
}

func TestTaskExecutionArtifactsFollowSourceAndVersion(t *testing.T) {
	s, store, _, attempt := terminalFixture(t, "completed")
	if _, err := store.DB.Exec("UPDATE runtime_attempts SET summary='完成发布核对',artifacts='[\"report.md\"]' WHERE id=?", attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec("UPDATE runtime_tasks SET result_summary='发布核对通过' WHERE id='task-1'"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/tasks/task-1", "/api/v1/tasks?status=all"} {
		w := request(t, s, "GET", path, "", nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "发布核对") {
			t.Fatal(w.Body.String())
		}
	}
	w := request(t, s, "GET", "/api/v1/tasks/task-1", "", nil)
	if !strings.Contains(w.Body.String(), "report.md") {
		t.Fatal("artifact missing")
	}
	if _, err := store.DB.Exec("UPDATE runtime_tasks SET version=2 WHERE id='task-1'"); err != nil {
		t.Fatal(err)
	}
	w = request(t, s, "GET", "/api/v1/tasks/task-1", "", nil)
	if strings.Contains(w.Body.String(), "report.md") || strings.Contains(w.Body.String(), "SECRET-RESULT") || strings.Contains(w.Body.String(), "完成发布核对") {
		t.Fatal("old version output leaked", w.Body.String())
	}
	if w = request(t, s, "GET", "/api/v1/tasks/task-1/terminal?attempt="+attempt, "", nil); w.Code != 403 {
		t.Fatal("old version terminal available")
	}
	if _, err := store.DB.Exec("UPDATE runtime_tasks SET version=1 WHERE id='task-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec("UPDATE runtime_task_messages SET revision=0 WHERE task_id='task-1'"); err != nil {
		t.Fatal(err)
	}
	w = request(t, s, "GET", "/api/v1/tasks/task-1", "", nil)
	if strings.Contains(w.Body.String(), "report.md") || strings.Contains(w.Body.String(), "SECRET-RESULT") || strings.Contains(w.Body.String(), "完成发布核对") {
		t.Fatal("changed request output leaked", w.Body.String())
	}
}

func TestMemorySixCategoriesAndHistoricalVersion(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(s.opts.Home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, category := range []string{"fact", "preference", "constraint", "decision", "procedure", "lesson"} {
		m := saveConsoleMemory(t, store, "global", category, category, "active", nil)
		old := m.Content
		_, err := store.Mutate(ctx, core.Request{Scope: "global", Command: "memory.revision.fixture"}, func(tx *core.Tx) (any, error) {
			m.Content += "\n修订后的条件"
			m.Status = "disputed"
			items, _, e := tx.SaveMemories(ctx, "memory.revise", "条件变化", []core.Memory{m}, map[string]int{m.ID: 1})
			return items, e
		})
		if err != nil {
			t.Fatal(err)
		}
		d := getMemoryData(t, s, "/api/v1/memories?status=disputed&category="+category)
		if string(d["total"]) != "1" {
			t.Fatal("category missing", category)
		}
		var read core.Memory
		json.Unmarshal(getMemoryData(t, s, "/api/v1/memories/"+m.ID+"?version=1")["memory"], &read)
		if read.Version != 1 || read.Content != old || read.Status != "active" {
			t.Fatal("historical memory replaced by current version")
		}
		var history []core.AuditedRevision
		json.Unmarshal(getMemoryData(t, s, "/api/v1/memories/"+m.ID+"/history")["revisions"], &history)
		if len(history) != 2 || history[1].Operation.Reason != "条件变化" || history[1].Content == old {
			t.Fatal("history audit mismatch")
		}
	}
}

func TestTaskMultipleAttemptsRemainInOneRecord(t *testing.T) {
	s, store, _, attempt := terminalFixture(t, "failed")
	if _, err := store.DB.Exec("UPDATE runtime_attempts SET error_code='execution_failed',finished_at='2026-01-01T00:01:00Z',started_at='2026-01-01T00:00:00Z' WHERE id=?", attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(`INSERT INTO runtime_attempts(id,task_id,task_version,status,model,input_digest,started_at,finished_at,summary,artifacts) VALUES('second','task-1',1,'completed','fake','digest','2026-01-01T00:02:00Z','2026-01-01T00:03:00Z','第二次完成','["second.md"]')`); err != nil {
		t.Fatal(err)
	}
	d := getMemoryData(t, s, "/api/v1/tasks/task-1")
	var attempts []struct {
		ID        string   `json:"id"`
		Status    string   `json:"status"`
		Artifacts []string `json:"artifacts"`
		Error     string   `json:"error_code"`
	}
	json.Unmarshal(d["attempts"], &attempts)
	if len(attempts) != 2 || attempts[0].ID != attempt || attempts[0].Error != "execution_failed" || attempts[1].Status != "completed" || len(attempts[1].Artifacts) != 1 {
		t.Fatalf("attempt history mismatch: %+v", attempts)
	}
	var tasks []TaskCard
	json.Unmarshal(getMemoryData(t, s, "/api/v1/tasks?status=all")["tasks"], &tasks)
	if len(tasks) != 1 {
		t.Fatal("attempts became separate tasks")
	}
	if _, err := store.DB.Exec(`INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,input_digest,state,created_at,updated_at)
	 SELECT 'failed-delivery',r.channel_id,r.id,r.version,'task-1',r.conversation_id,'fixture','fixture','failed',?,? FROM channel_routes r JOIN runtime_tasks t ON t.route_id=r.id WHERE t.id='task-1'`, core.Now(), core.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec("UPDATE runtime_tasks SET status='completed' WHERE id='task-1'"); err != nil {
		t.Fatal(err)
	}
	d = getMemoryData(t, s, "/api/v1/tasks/task-1")
	var card TaskCard
	var deliveries []struct {
		State string `json:"state"`
	}
	json.Unmarshal(d["task"], &card)
	json.Unmarshal(d["deliveries"], &deliveries)
	if card.Status != "completed" || len(deliveries) != 1 || deliveries[0].State != "failed" {
		t.Fatal("execution and delivery were conflated")
	}
}
