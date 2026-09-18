// Package observation records disposable runner heartbeats, never business state.
package observation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type Runner struct {
	RuntimeID   string    `json:"runtime_id"`
	Home        string    `json:"home"`
	InstanceID  string    `json:"instance_id"`
	PID         int       `json:"pid"`
	Version     string    `json:"version"`
	Build       string    `json:"build"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

func CanonicalHome(home string) string {
	home, _ = filepath.Abs(home)
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		return resolved
	}
	return home
}

// Watch reports the wrapper's heartbeat, not the model's current tool or health.
// Each process owns a separate file so it cannot erase another runner's record.
func Watch(ctx context.Context, home, runtimeID, version, build string) (func(), error) {
	home = CanonicalHome(home)
	dir := filepath.Join(home, "runtime", "observations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	entry := Runner{RuntimeID: runtimeID, Home: home, InstanceID: core.NewID(), PID: os.Getpid(), Version: version, Build: build, StartedAt: time.Now().UTC()}
	path := filepath.Join(dir, entry.InstanceID+".json")
	write := func() error {
		entry.HeartbeatAt = time.Now().UTC()
		body, _ := json.Marshal(entry)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, body, 0600); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	if err := write(); err != nil {
		return nil, err
	}
	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				_ = write()
			}
		}
	}()
	return func() { cancel(); <-done; _ = os.Remove(path); _ = os.Remove(path + ".tmp") }, nil
}

func Read(home, runtimeID string, now time.Time) []Runner {
	home = CanonicalHome(home)
	dir := filepath.Join(home, "runtime", "observations")
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	entries := []Runner{}
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		info, err := file.Info()
		if err != nil || info.Size() > 8192 {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			continue
		}
		var entry Runner
		if json.Unmarshal(body, &entry) != nil || entry.Home != home || entry.RuntimeID != runtimeID || entry.InstanceID+".json" != file.Name() {
			continue
		}
		age := now.Sub(entry.HeartbeatAt)
		if age >= 0 && age < 20*time.Second {
			entries = append(entries, entry)
		}
	}
	return entries
}
