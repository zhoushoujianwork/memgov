package core

import (
	"context"
	"testing"
)

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
	if _, e := f.s.DB.Exec("UPDATE runtime_configs SET agent_bash=0,version=version+1 WHERE id=?", f.config.ID); e != nil {
		t.Fatal(e)
	}
	if e := CheckWorkLease(ctx, f.s.DB, a.ID); ErrorCode(e) != "conflict" {
		t.Fatalf("permission change did not fence: %v", e)
	}
}
