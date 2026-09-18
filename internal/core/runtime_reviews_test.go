package core

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestReviewSharedCapacityPriorityAndParentBudget(t *testing.T) {
	f := schedulingFixture(t)
	ctx := context.Background()
	parent := time.Now().Add(45 * time.Second).UTC().Format(time.RFC3339Nano)
	for i := 0; i < 3; i++ {
		id := fmt.Sprint("review-task-", i)
		if _, e := f.s.DB.Exec(`INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,kind,title,instructions,status,created_at,updated_at) VALUES(?,?,?,?,'task','done','done','completed',?,?)`, id, f.config.ID, f.watch.ID, id, Now(), Now()); e != nil {
			t.Fatal(e)
		}
		if _, e := f.s.DB.Exec(`INSERT INTO runtime_reviews(id,task_id,task_version,runtime_id,source_attempt_id,candidate_input,deadline_at,created_at) VALUES(?,?,1,?,'parent','null',?,?)`, NewID(), id, f.config.ID, parent, Now()); e != nil {
			t.Fatal(e)
		}
	}
	task := createRuntimeTask(t, f, "business-first")
	runtimeMutate(t, f.s, "test.review.priority", func(tx *Tx) (any, error) {
		r, e := tx.ClaimRuntimeReview(ctx, f.config)
		if r.ID != "" {
			t.Fatal("review passed pending business")
		}
		return r, e
	})
	if _, e := f.s.DB.Exec("UPDATE runtime_tasks SET status='completed' WHERE id=?", task.ID); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		runtimeMutate(t, f.s, "test.review.claim", func(tx *Tx) (any, error) {
			r, e := tx.ClaimRuntimeReview(ctx, f.config)
			if e != nil {
				return nil, e
			}
			if (i < 2) != (r.ID != "") {
				t.Fatalf("review slots: claim %d returned %+v", i, r)
			}
			if r.ID != "" {
				var deadline string
				if e = tx.Conn.QueryRowContext(ctx, "SELECT deadline_at FROM runtime_work_leases WHERE id=?", r.ExecutionID).Scan(&deadline); e != nil {
					return nil, e
				}
				if deadline != parent {
					t.Fatalf("parent deadline extended: %s != %s", deadline, parent)
				}
			}
			return r, nil
		})
	}
}

func TestRuntimeLeasePolicyDigestAllowsSchedulingButFencesPermissions(t *testing.T) {
	f := schedulingFixture(t)
	ctx := context.Background()
	task := createRuntimeTask(t, f, "epoch")
	var a RuntimeAttempt
	runtimeMutate(t, f.s, "test.claim", func(tx *Tx) (any, error) {
		var e error
		_, a, e = tx.ClaimRuntimeTask(ctx, f.config.ID, NewID(), "m", "p", "c", "")
		return nil, e
	})
	if _, e := f.s.DB.Exec("UPDATE runtime_configs SET concurrency=1,analysis_concurrency=2,execution_timeout_seconds=60,version=version+1 WHERE id=?", f.config.ID); e != nil {
		t.Fatal(e)
	}
	if e := CheckWorkLease(ctx, f.s.DB, a.ID); e != nil {
		t.Fatalf("scheduling cancelled old lease: %v", e)
	}
	if _, e := ResolveRuntimeTaskAgent(ctx, f.s.DB, f.config, task); e != nil {
		t.Fatalf("scheduling cancelled Agent: %v", e)
	}
	if _, e := f.s.DB.Exec("UPDATE runtime_configs SET memory_scope='conversation_published',version=version+1 WHERE id=?", f.config.ID); e != nil {
		t.Fatal(e)
	}
	if e := CheckWorkLease(ctx, f.s.DB, a.ID); ErrorCode(e) != "conflict" {
		t.Fatalf("permission change did not fence: %v", e)
	}
}
