// Package service owns the single local service process. Its files are
// disposable process observations; configuration and work remain in SQLite.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/observation"
	"golang.org/x/sys/unix"
)

type Module struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	ErrorCode string `json:"error_code,omitempty"`
	Attempts  int    `json:"attempts"`
}

type Snapshot struct {
	ID          string    `json:"id,omitempty"`
	Home        string    `json:"home"`
	ConfigPath  string    `json:"config_path,omitempty"`
	PID         int       `json:"pid,omitempty"`
	Version     string    `json:"version,omitempty"`
	Build       string    `json:"build,omitempty"`
	State       string    `json:"state"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	HeartbeatAt time.Time `json:"heartbeat_at,omitempty"`
	Modules     []Module  `json:"modules"`
}

func directory(home string) string {
	return filepath.Join(observation.CanonicalHome(home), "runtime", "service")
}

// Locked verifies process ownership independently of the heartbeat file.
func Locked(home string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(directory(home), "process.lock"), os.O_RDWR, 0600)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

func acquire(home string) (*os.File, error) {
	if err := os.MkdirAll(directory(home), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(directory(home), "process.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, core.Fail("conflict", "unified service already running for this home")
		}
		return nil, err
	}
	return f, nil
}

func Read(home string) (Snapshot, error) {
	s := Snapshot{Home: observation.CanonicalHome(home), State: "stopped", Modules: []Module{}}
	locked, err := Locked(home)
	if err != nil {
		return s, err
	}
	if !locked {
		return s, nil
	}
	s.State = "unverified"
	body, err := os.ReadFile(filepath.Join(directory(home), "status.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var recorded Snapshot
	if len(body) > 1<<20 || json.Unmarshal(body, &recorded) != nil || recorded.Home != s.Home || recorded.ID == "" {
		return s, nil
	}
	age := time.Since(recorded.HeartbeatAt)
	if age < 0 || age > 20*time.Second {
		recorded.State = "unverified"
	}
	return recorded, nil
}

func write(home string, s Snapshot) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	path := filepath.Join(directory(home), "status.json")
	if err = os.WriteFile(path+".tmp", body, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Stop requests cancellation of the exact service generation, without sending
// a signal to a potentially reused PID or changing another home's workers.
func Stop(ctx context.Context, home string) error {
	s, err := Read(home)
	if err != nil {
		return err
	}
	if s.State == "stopped" {
		return nil
	}
	if s.ID == "" {
		return core.Fail("unavailable", "service is starting; retry stop shortly")
	}
	path := filepath.Join(directory(home), "stop.request")
	if err = os.WriteFile(path, []byte(s.ID), 0600); err != nil {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		locked, err := Locked(home)
		if err != nil || !locked {
			return err
		}
		current, readErr := Read(home)
		if readErr != nil {
			return readErr
		}
		if current.ID != "" && current.ID != s.ID {
			return nil
		}
		select {
		case <-ctx.Done():
			return core.Fail("unavailable", "service did not stop before the timeout")
		case <-ticker.C:
		}
	}
}
