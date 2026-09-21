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

// taskContext keeps a claimed task's process-bound execution tied to its
// durable state. Cancelling a task through the CLI changes its status and
// version in SQLite; this watcher turns that change into context cancellation
// so a model or tool process can stop instead of continuing invisibly.
func (s *Service) taskContext(parent context.Context, taskID string, version int) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(done)
			cancel()
		})
	}
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkCtx, checkCancel := context.WithTimeout(context.Background(), time.Second)
				var status string
				var currentVersion int
				err := s.Store.DB.QueryRowContext(checkCtx, "SELECT status,version FROM runtime_tasks WHERE id=?", taskID).Scan(&status, &currentVersion)
				checkCancel()
				// Completion and confirmation states still need the caller's
				// context for delivery and acknowledgement work. Only durable
				// invalidation states should interrupt the active Agent.
				if err == nil && (status == "cancelled" || status == "stale" || (status == "running" && currentVersion != version)) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, finish
}

// workContext creates the one durable deadline for a piece of work. Runtime
// activity stays in memory; there is no heartbeat, lease renewal, phase, or
// model-activity write while the model or tools are running.
func (s *Service) workContext(parent context.Context, id, task string, version int) (context.Context, func()) {
	_ = task
	_ = version
	var raw string
	err := s.Store.DB.QueryRowContext(parent, `SELECT deadline_at FROM runtime_work_leases WHERE id=? AND released=0`, id).Scan(&raw)
	deadline, parseErr := time.Parse(time.RFC3339Nano, raw)
	if err != nil || parseErr != nil {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, func() { s.releaseWork(id) }
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	return ctx, func() {
		cancel()
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

func (s *Service) phase(context.Context, string, string) {}
