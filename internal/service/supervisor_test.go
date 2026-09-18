package service

import (
	"context"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func launchSupervisor(t *testing.T, s Supervisor) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("supervisor did not shut down")
		}
	})
	return cancel, done
}
func TestOneProcessPerHomeAndScopedStop(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir()
	var starts, stops atomic.Int32
	worker := func(ctx context.Context) error { starts.Add(1); <-ctx.Done(); stops.Add(1); return nil }
	_, _ = launchSupervisor(t, Supervisor{Home: home, Tick: 10 * time.Millisecond, Select: func(context.Context) ([]Spec, error) { return []Spec{{Key: "worker", Run: worker}}, nil }})
	eventually(t, func() bool { s, _ := Read(home); return s.State == "running" && starts.Load() == 1 })
	duplicate := Supervisor{Home: home, Select: func(context.Context) ([]Spec, error) { t.Error("duplicate selected modules"); return nil, nil }}
	if err := duplicate.Run(context.Background()); core.ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	if err := Stop(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 0 {
		t.Fatal("other home stopped worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Stop(ctx, home); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 1 {
		t.Fatal("shutdown did not await worker")
	}
	s, err := Read(home)
	if err != nil || s.State != "stopped" {
		t.Fatal(s, err)
	}
}
func TestEpochReplacementWaitsAndDisabledModuleStops(t *testing.T) {
	home := t.TempDir()
	var mu sync.Mutex
	epoch := "1"
	enabled := true
	var active, maxActive, starts atomic.Int32
	selectModules := func(context.Context) ([]Spec, error) {
		mu.Lock()
		defer mu.Unlock()
		if !enabled {
			return nil, nil
		}
		return []Spec{{Key: "agent", Epoch: epoch, Run: func(ctx context.Context) error {
			n := active.Add(1)
			starts.Add(1)
			for old := maxActive.Load(); n > old && !maxActive.CompareAndSwap(old, n); old = maxActive.Load() {
			}
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond)
			active.Add(-1)
			return nil
		}}}, nil
	}
	launchSupervisor(t, Supervisor{Home: home, Tick: 5 * time.Millisecond, Select: selectModules})
	eventually(t, func() bool { return starts.Load() == 1 })
	mu.Lock()
	epoch = "2"
	mu.Unlock()
	eventually(t, func() bool { return starts.Load() == 2 })
	if maxActive.Load() != 1 {
		t.Fatal("overlapping generations")
	}
	mu.Lock()
	enabled = false
	mu.Unlock()
	eventually(t, func() bool { return active.Load() == 0 })
}
func TestFailureIsolationAndObsoleteStopRequest(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(directory(home), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory(home), "stop.request"), []byte("old-generation"), 0600); err != nil {
		t.Fatal(err)
	}
	var healthy atomic.Int32
	launchSupervisor(t, Supervisor{Home: home, Tick: 5 * time.Millisecond, Select: func(context.Context) ([]Spec, error) {
		return []Spec{
			{Key: "healthy", Run: func(ctx context.Context) error { healthy.Add(1); <-ctx.Done(); return nil }},
			{Key: "denied", Run: func(context.Context) error { return core.Fail("denied", "permission denied") }},
		}, nil
	}})
	eventually(t, func() bool {
		s, _ := Read(home)
		return s.State == "degraded" && len(s.Modules) == 2 && s.Modules[0].State == "blocked"
	})
	if healthy.Load() != 1 {
		t.Fatal("healthy module restarted")
	}
}
func TestPausedModuleStartsOnResumeAndExplicitStopStaysStopped(t *testing.T) {
	var mu sync.Mutex
	paused, resume := true, false
	var starts atomic.Int32
	finished := make(chan struct{})
	launchSupervisor(t, Supervisor{Home: t.TempDir(), Tick: 5 * time.Millisecond, Select: func(context.Context) ([]Spec, error) {
		mu.Lock()
		defer mu.Unlock()
		return []Spec{{Key: "agent", Paused: paused, Resume: resume, Run: func(ctx context.Context) error {
			starts.Add(1)
			select {
			case <-ctx.Done():
			case <-finished:
			}
			return nil
		}}}, nil
	}})
	time.Sleep(30 * time.Millisecond)
	if starts.Load() != 0 {
		t.Fatal("paused worker started")
	}
	mu.Lock()
	paused = false
	mu.Unlock()
	eventually(t, func() bool { return starts.Load() == 1 })
	close(finished)
	time.Sleep(30 * time.Millisecond)
	if starts.Load() != 1 {
		t.Fatal("explicitly stopped worker restarted")
	}
}

func TestStopDuringPreflightPreventsModuleLaunch(t *testing.T) {
	home := t.TempDir()
	checking, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	launchSupervisor(t, Supervisor{Home: home, Tick: 5 * time.Millisecond, Check: func(context.Context) error { close(checking); <-release; return nil }, Select: func(context.Context) ([]Spec, error) {
		return []Spec{{Key: "agent", Run: func(ctx context.Context) error { starts.Add(1); <-ctx.Done(); return nil }}}, nil
	}})
	<-checking
	s, err := Read(home)
	if err != nil || s.State != "starting" || s.ID == "" {
		t.Fatal(s, err)
	}
	stopped := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { stopped <- Stop(ctx, home) }()
	eventually(t, func() bool {
		body, _ := os.ReadFile(filepath.Join(directory(home), "stop.request"))
		return string(body) == s.ID
	})
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 0 {
		t.Fatal("stopped startup launched a module")
	}
}
