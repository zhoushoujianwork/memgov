package service

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/observation"
)

type Spec struct {
	Key, Name, Kind, Epoch string
	Paused, Resume         bool
	Run                    func(context.Context) error
}

type Supervisor struct {
	Home, ConfigPath, Version, Build string
	Select                           func(context.Context) ([]Spec, error)
	Tick                             time.Duration
	// Check runs under the process lock, before any module starts.
	Check func(context.Context) error
}

type entry struct {
	spec                                 Spec
	module                               Module
	cancel                               context.CancelFunc
	active, stopping, completed, blocked bool
	retryAt                              time.Time
}
type result struct {
	key string
	err error
}

func (s *Supervisor) Run(ctx context.Context) error {
	lock, err := acquire(s.Home)
	if err != nil {
		return err
	}
	defer lock.Close()
	snapshot := Snapshot{ID: core.NewID(), Home: observation.CanonicalHome(s.Home), ConfigPath: s.ConfigPath, PID: os.Getpid(), Version: s.Version, Build: s.Build, State: "starting", StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(), Modules: []Module{}}
	if err := write(s.Home, snapshot); err != nil {
		return err
	}
	defer func() {
		snapshot.State = "stopped"
		snapshot.HeartbeatAt = time.Now().UTC()
		_ = write(s.Home, snapshot)
	}()
	if s.Check != nil {
		if err := s.Check(ctx); err != nil {
			return err
		}
	}
	if s.Select == nil {
		return core.Fail("invalid_input", "service module selector is required")
	}
	specs, err := s.Select(ctx)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	entries := map[string]*entry{}
	results := make(chan result)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	launch := func(e *entry) {
		workerCtx, stop := context.WithCancel(runCtx)
		e.cancel, e.active, e.stopping, e.completed = stop, true, false, false
		e.module.State, e.module.ErrorCode = "active", ""
		e.module.Attempts++
		spec := e.spec
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := spec.Run(workerCtx)
			select {
			case results <- result{spec.Key, err}:
			case <-runCtx.Done():
			}
		}()
	}
	reconcile := func(wanted []Spec) error {
		byKey := map[string]Spec{}
		for _, spec := range wanted {
			if spec.Key == "" || spec.Run == nil {
				return core.Fail("invalid_input", "invalid service module")
			}
			if _, exists := byKey[spec.Key]; exists {
				return core.Fail("conflict", "duplicate service module")
			}
			byKey[spec.Key] = spec
		}
		for key, e := range entries {
			spec, exists := byKey[key]
			if !exists || spec.Epoch != e.spec.Epoch {
				if e.active {
					e.stopping, e.module.State = true, "stopping"
					e.cancel()
				} else {
					delete(entries, key)
				}
			}
		}
		for key, spec := range byKey {
			e, exists := entries[key]
			if !exists {
				e = &entry{spec: spec, module: Module{Key: key, Name: spec.Name, Kind: spec.Kind, State: "paused"}}
				entries[key] = e
				if !spec.Paused {
					launch(e)
				}
				continue
			}
			if e.active && !e.stopping {
				e.module.State = "active"
				if spec.Paused {
					e.module.State = "paused"
				}
			}
			if e.active || e.blocked || spec.Paused {
				continue
			}
			if e.completed && !spec.Resume {
				continue
			}
			if time.Now().Before(e.retryAt) {
				continue
			}
			launch(e)
		}
		snapshot.Modules = []Module{}
		snapshot.State = "running"
		for _, e := range entries {
			snapshot.Modules = append(snapshot.Modules, e.module)
			if e.module.ErrorCode != "" {
				snapshot.State = "degraded"
			}
		}
		sort.Slice(snapshot.Modules, func(i, j int) bool { return snapshot.Modules[i].Key < snapshot.Modules[j].Key })
		snapshot.HeartbeatAt = time.Now().UTC()
		return write(s.Home, snapshot)
	}
	if body, err := os.ReadFile(filepath.Join(directory(s.Home), "stop.request")); err == nil && string(body) == snapshot.ID {
		return nil
	}
	if err := reconcile(specs); err != nil {
		return err
	}
	interval := s.Tick
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-results:
			e := entries[r.key]
			if e == nil {
				continue
			}
			e.cancel()
			e.active = false
			if e.stopping {
				delete(entries, r.key)
				continue
			}
			if r.err == nil {
				e.completed, e.module.State = true, "stopped"
			} else {
				e.module.ErrorCode = core.ErrorCode(r.err)
				e.module.State = "retrying"
				e.retryAt = time.Now().Add(time.Duration(min(e.module.Attempts, 30)) * time.Second)
				if e.module.ErrorCode == "denied" || e.module.ErrorCode == "invalid_input" || e.module.ErrorCode == "conflict" {
					e.blocked, e.module.State = true, "blocked"
				}
			}
		case <-ticker.C:
			body, err := os.ReadFile(filepath.Join(directory(s.Home), "stop.request"))
			if err == nil && string(body) == snapshot.ID {
				return nil
			}
			specs, err := s.Select(runCtx)
			if err != nil {
				return err
			}
			if err := reconcile(specs); err != nil {
				return err
			}
		}
	}
}
