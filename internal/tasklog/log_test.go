package tasklog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClaudePublicOutputAndCredentialRedaction(t *testing.T) {
	home := t.TempDir()
	w, err := Open(home, "runtime-1", "task-1", "attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, raw := range []string{
		`{"type":"system","subtype":"init","model":"test-model","api_key":"INIT-SECRET"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"PRIVATE-THINKING"},{"type":"text","text":"正在检查失败原因"},{"type":"tool_use","name":"Bash","input":{"command":"curl https://secret.invalid/path?token=ABC", "api_key":"SECRET-KEY"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"OWNER-PROMPT"},{"type":"tool_result","content":[{"type":"thinking","text":"HIDDEN"},{"type":"text","text":"error: password=PASSWORD-SECRET\nfailed test"}],"is_error":true}]}}`,
		`{"type":"result","result":"已核对文件，未重复发布"}`,
	} {
		w.Claude([]byte(raw))
	}
	tail, err := Read(home, "runtime-1", "task-1", "attempt-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(tail)
	for _, value := range []string{"PRIVATE-THINKING", "INIT-SECRET", "SECRET-KEY", "PASSWORD-SECRET", "OWNER-PROMPT", "secret.invalid", "HIDDEN"} {
		if strings.Contains(string(raw), value) {
			t.Fatalf("leaked %s: %s", value, raw)
		}
	}
	for _, text := range []string{`Authorization: Bearer AUTH-SECRET`, `{"Authorization":"Basic AUTH-SECRET"}`, `token=AUTH-SECRET`} {
		if strings.Contains(SafeText(text), "AUTH-SECRET") {
			t.Fatalf("authorization leaked: %s", SafeText(text))
		}
	}
	if len(tail.Events) != 5 || !strings.Contains(string(raw), "failed test") || !tail.Available {
		t.Fatalf("missing public events: %s", raw)
	}
	next, err := Read(home, "runtime-1", "task-1", "attempt-1", tail.Cursor)
	if err != nil || len(next.Events) != 0 {
		t.Fatalf("duplicate events: %+v %v", next, err)
	}
	st, err := os.Stat(filepath.Join(home, "runtime", "output", "runtime-1", "task-1", "attempt-1.jsonl"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("unsafe output permissions")
	}
}

func TestConcurrentBoundedTailAndQuota(t *testing.T) {
	home := t.TempDir()
	w, err := Open(home, "r", "t", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 160; j++ {
				w.Emit("assistant", strings.Repeat("中文", 4000))
			}
		}()
	}
	wg.Wait()
	tail, err := Read(home, "r", "t", "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !tail.Truncated || len(tail.Events) == 0 || tail.Events[len(tail.Events)-1].Kind != "limit" || tail.Cursor > MaxFileBytes {
		t.Fatalf("unbounded output: %+v", tail)
	}
	for _, e := range tail.Events {
		if e.Offset <= 0 {
			t.Fatal("missing cursor")
		}
	}
}

func TestPartialLineRetentionAndUnsafePaths(t *testing.T) {
	home := t.TempDir()
	w, err := Open(home, "r", "t", "a")
	if err != nil {
		t.Fatal(err)
	}
	w.Emit("assistant", "first")
	w.Close()
	name := filepath.Join(home, "runtime", "output", "r", "t", "a.jsonl")
	file, _ := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = file.WriteString(`{"kind":"assistant","text":"partial`)
	file.Close()
	first, err := Read(home, "r", "t", "a", 0)
	if err != nil || len(first.Events) != 1 {
		t.Fatalf("partial exposed: %+v %v", first, err)
	}
	file, _ = os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = file.WriteString("\"}\n")
	file.Close()
	second, err := Read(home, "r", "t", "a", first.Cursor)
	if err != nil || len(second.Events) != 1 || second.Events[0].Text != "partial" {
		t.Fatalf("partial lost: %+v %v", second, err)
	}
	old := time.Now().Add(-Retention - time.Hour)
	if err = os.Chtimes(name, old, old); err != nil {
		t.Fatal(err)
	}
	expired, err := Read(home, "r", "t", "a", 0)
	if err != nil || expired.Available {
		t.Fatal("expired log visible")
	}
	if _, err = Open(home, "r", "../escape", "a"); err == nil {
		t.Fatal("path traversal admitted")
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Join(home, "runtime", "output", "symlink")); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(home, "symlink", "t", "a"); err == nil {
		t.Fatal("symlink admitted")
	}
}
