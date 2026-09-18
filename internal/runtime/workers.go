package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/processtree"
)

func workError(ctx context.Context, stage string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return core.Fail(stage+"_timeout", "%s deadline exceeded", stage)
	}
	return core.Fail("conflict", "work cancelled or superseded")
}

// This control loop is independent of model output. A heartbeat represents
// worker ownership, never progress. Lease release happens after process reaping.
func (s *Service) workContext(parent context.Context, id, task string, version int) (context.Context, func()) {
	var raw string
	err := s.Store.DB.QueryRowContext(parent, `SELECT deadline_at FROM runtime_work_leases WHERE id=? AND released=0`, id).Scan(&raw)
	deadline, parseErr := time.Parse(time.RFC3339Nano, raw)
	if err != nil || parseErr != nil {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, func() { s.releaseWork(id) }
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	ctx = processtree.WithRecorder(ctx, func(pid int, started string) error {
		return s.mutate(ctx, "global", "runtime.work.process", func(tx *core.Tx) (any, error) { return nil, tx.RecordWorkProcess(ctx, id, pid, started) })
	})
	var activityMu sync.Mutex
	var lastActivity time.Time
	ctx = processtree.WithActivity(ctx, func() {
		activityMu.Lock()
		defer activityMu.Unlock()
		if time.Since(lastActivity) < 5*time.Second {
			return
		}
		lastActivity = time.Now()
		_ = s.mutate(ctx, "global", "runtime.work.output", func(tx *core.Tx) (any, error) {
			_, e := tx.Conn.ExecContext(ctx, "UPDATE runtime_work_leases SET model_activity_at=? WHERE id=? AND released=0", core.Now(), id)
			return nil, e
		})
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		renewal := time.Now().Add(10 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				checkCtx, stop := context.WithTimeout(ctx, 3*time.Second)
				err := core.CheckWorkLease(checkCtx, s.Store.DB, id)
				if err == nil && task != "" {
					var current int
					var status string
					err = s.Store.DB.QueryRowContext(checkCtx, `SELECT version,status FROM runtime_tasks WHERE id=?`, task).Scan(&current, &status)
					if err == nil && (current != version || status != "running") {
						err = core.Fail("conflict", "task changed")
					}
				}
				if err == nil && !time.Now().Before(renewal) {
					err = s.mutate(checkCtx, "global", "runtime.work.renew", func(tx *core.Tx) (any, error) { return nil, tx.RenewWorkLease(checkCtx, id) })
					renewal = time.Now().Add(10 * time.Second)
				}
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel()
		<-done
		if processtree.CheckQuiescence(ctx) == nil {
			s.releaseWork(id)
		}
	}
}

func (s *Service) releaseWork(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.mutate(ctx, "global", "runtime.work.release", func(tx *core.Tx) (any, error) { return nil, tx.ReleaseWorkLease(ctx, id) })
}

func (s *Service) wakeWorker() {
	select {
	case s.workerWake <- struct{}{}:
	default:
	}
}

// Startup is not permission to invalidate another healthy service's workers.
// Reap only dead owners, outside the SQLite writer transaction.
func (s *Service) recoverWork(ctx context.Context, runtimeID string) error {
	rows, err := s.Store.DB.QueryContext(ctx, "SELECT id,owner_pid,owner_started,process_pid,process_started FROM runtime_work_leases WHERE runtime_id=? AND released=0", runtimeID)
	if err != nil {
		return err
	}
	type orphan struct {
		id                string
		owner, pid        int
		ownerStamp, stamp string
	}
	var items []orphan
	for rows.Next() {
		var o orphan
		if err = rows.Scan(&o.id, &o.owner, &o.ownerStamp, &o.pid, &o.stamp); err != nil {
			rows.Close()
			return err
		}
		items = append(items, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, o := range items {
		if processtree.Alive(o.owner, o.ownerStamp) {
			return core.Fail("conflict", "runtime has a live worker owner")
		}
		if err = processtree.Reap(o.pid, o.stamp); err != nil {
			return err
		}
		s.releaseWork(o.id)
	}
	return nil
}

func (s *Service) phase(ctx context.Context, id, phase string) {
	_ = s.mutate(ctx, "global", "runtime.work.phase", func(tx *core.Tx) (any, error) { return nil, tx.SetWorkPhase(ctx, id, phase) })
}
