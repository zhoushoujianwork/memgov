package runtime

import (
	"context"
	"testing"
	"time"
)

func TestTaskContextCancelsWhenTaskVersionChanges(t *testing.T) {
	s, cfg, _, _, _ := setupService(t)
	ctx := context.Background()
	taskID := "task-context-cancel"
	if _, err := s.Store.DB.ExecContext(ctx, `INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,kind,title,instructions,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, taskID, cfg.ID, cfg.RouteIDs[0], "task-context-cancel", "task", "test", "test", "running", time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	workCtx, finish := s.taskContext(ctx, taskID, 1)
	defer finish()
	if _, err := s.Store.DB.ExecContext(ctx, "UPDATE runtime_tasks SET status='cancelled',version=version+1 WHERE id=?", taskID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-workCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("task context did not observe durable cancellation")
	}
}

func TestTaskContextStopsWatchingAfterFinish(t *testing.T) {
	s, cfg, _, _, _ := setupService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskID := "task-context-finish"
	if _, err := s.Store.DB.ExecContext(ctx, `INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,kind,title,instructions,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, taskID, cfg.ID, cfg.RouteIDs[0], "task-context-finish", "task", "test", "test", "running", time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	workCtx, finish := s.taskContext(ctx, taskID, 1)
	finish()
	select {
	case <-workCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("finished task context did not cancel")
	}
}
