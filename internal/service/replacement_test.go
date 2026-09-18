package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplacementWaitsThroughMissingAndInvalidFile(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "memgov")
	if err := os.WriteFile(path, body, 0700); err != nil {
		t.Fatal(err)
	}
	initial, _ := os.Stat(path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- WaitReplacement(ctx, path, initial, 10*time.Millisecond) }()
	assertWaiting := func() {
		t.Helper()
		select {
		case <-done:
			t.Fatal("restarted before valid replacement")
		case <-time.After(60 * time.Millisecond):
		}
	}
	assertWaiting()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertWaiting()
	if err := os.WriteFile(path, []byte("incomplete download"), 0700); err != nil {
		t.Fatal(err)
	}
	assertWaiting()
	if err := os.WriteFile(path+".new", body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	assertWaiting()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	select {
	case replaced := <-done:
		if !replaced {
			t.Fatal("missed replacement")
		}
	case <-time.After(time.Second):
		t.Fatal("no restart after replacement")
	}
}

func TestReplacementCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if WaitReplacement(ctx, "missing", nil, time.Millisecond) {
		t.Fatal("cancel requested a restart")
	}
}
