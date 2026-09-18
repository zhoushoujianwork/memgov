package runlog

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoggerWritesSameRedactedSchemaToFileAndStdout(t *testing.T) {
	home := t.TempDir()
	var stdout bytes.Buffer
	now := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	l, err := Open(home, "runtime-1", &stdout, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	path := l.Path()
	if err = l.Emit(Event{Level: "info", Component: "analysis", Event: "completed", TaskID: "task-1", Summary: "api_key=secret https://example.test/private", ToolKinds: []string{"git", "聊天原文不应成为工具类别"}}); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil || dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("log directory permissions: %v %v", dirInfo.Mode().Perm(), err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil || fileInfo.Mode().Perm() != 0600 {
		t.Fatalf("log file permissions: %v %v", fileInfo.Mode().Perm(), err)
	}
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(fileBytes) != stdout.String() {
		t.Fatalf("stdout differs from file\nfile=%s\nout=%s", fileBytes, stdout.String())
	}
	if strings.Contains(string(fileBytes), "secret") || strings.Contains(string(fileBytes), "example.test") {
		t.Fatalf("sensitive text reached log: %s", fileBytes)
	}
	var event Event
	if err = json.Unmarshal(bytes.TrimSpace(fileBytes), &event); err != nil {
		t.Fatal(err)
	}
	if event.SchemaVersion != SchemaVersion || event.Timestamp == "" || event.RuntimeID != "runtime-1" || event.TaskID != "task-1" {
		t.Fatalf("invalid event schema: %+v", event)
	}
	if len(event.ToolKinds) != 2 || event.ToolKinds[0] != "git" || event.ToolKinds[1] != "other" {
		t.Fatalf("tool categories were not bounded: %+v", event.ToolKinds)
	}
}

func TestLoggerSerializesConcurrentWriters(t *testing.T) {
	home := t.TempDir()
	l, err := Open(home, "runtime-concurrent", nil, Options{FileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if emitErr := l.Emit(Event{Component: "worker", Event: "concurrent", TaskID: "task"}); emitErr != nil {
				t.Errorf("emit: %v", emitErr)
			}
		}()
	}
	wg.Wait()
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := Show(home, "runtime-concurrent", Filter{})
	if err != nil || len(events) != 32 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
}

func TestLoggerRotatesFiltersAndEnforcesQuota(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	l, err := Open(home, "runtime-2", nil, Options{Now: func() time.Time { return now }, FileBytes: 220, MaxBytes: 10 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		level := "info"
		if i == 2 {
			level = "error"
		}
		if err = l.Emit(Event{Level: level, Component: "worker", Event: "step", TaskID: "task", Summary: strings.Repeat("x", 80)}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := Files(home, "runtime-2")
	if err != nil || len(files) < 2 {
		t.Fatalf("rotation files=%d err=%v", len(files), err)
	}
	events, err := Show(home, "runtime-2", Filter{Level: "error", TaskID: "task", Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("filtered events=%d err=%v", len(events), err)
	}
	dir := filepath.Join(home, "runtime", "logs", "runtime-2")
	old := filepath.Join(dir, "old.jsonl")
	if err = os.WriteFile(old, bytes.Repeat([]byte("x"), 100), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := now.Add(-31 * 24 * time.Hour)
	if err = os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err = Sweep(dir, Options{Now: func() time.Time { return now }, Retention: 30 * 24 * time.Hour, MaxBytes: 10 << 20}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired file still exists: %v", err)
	}
	active := files[len(files)-1].Path
	if err = Sweep(dir, Options{Now: func() time.Time { return now }, MaxBytes: 1}, active); err == nil {
		t.Fatal("quota exhaustion was not reported while the active file could not be removed")
	}
}

func TestFollowEmitsExistingAndNewCompleteLinesOnce(t *testing.T) {
	home := t.TempDir()
	l, err := Open(home, "runtime-3", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = l.Emit(Event{Component: "runtime", Event: "first", TaskID: "keep"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- Follow(ctx, home, "runtime-3", Filter{TaskID: "keep"}, &out) }()
	time.Sleep(350 * time.Millisecond)
	if err = l.Emit(Event{Component: "runtime", Event: "ignored", TaskID: "other"}); err != nil {
		t.Fatal(err)
	}
	if err = l.Emit(Event{Component: "runtime", Event: "second", TaskID: "keep"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"event":"first"`) || !strings.Contains(lines[1], `"event":"second"`) {
		t.Fatalf("follow output: %s", out.String())
	}
}
