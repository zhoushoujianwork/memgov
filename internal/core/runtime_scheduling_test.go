package core

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func schedulingFixture(t *testing.T) runtimeFixture {
	f := newRuntimeFixture(t, 1, 30)
	if _, err := f.s.DB.Exec("UPDATE runtime_configs SET concurrency=4,analysis_concurrency=8 WHERE id=?", f.config.ID); err != nil {
		t.Fatal(err)
	}
	f.config, _ = ReadRuntime(context.Background(), f.s.DB, f.config.ID)
	return f
}

func TestSchedulingAtomicSharedPools(t *testing.T) {
	ctx := context.Background()
	f := schedulingFixture(t)
	var routes []string
	runtimeMutate(t, f.s, "routes", func(tx *Tx) (any, error) {
		for i := 0; i < 9; i++ {
			r, e := tx.AddRoute(ctx, f.channel.ID, RouteInput{ConversationID: fmt.Sprint("room-", i), ConversationType: "group"})
			if e != nil {
				return nil, e
			}
			routes = append(routes, r.ID)
		}
		_, e := tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET route_ids=? WHERE id=?", JSON(routes), f.config.ID)
		return nil, e
	})
	for i := 0; i < 9; i++ {
		intakeRuntimeMessage(t, f.s, f.channel, fmt.Sprint("room-", i), fmt.Sprint("event-", i), f.owner, "investigate", time.Now())
	}
	runtimeMutate(t, f.s, "sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	var mu sync.Mutex
	var batches []RuntimeBatch
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "claim"}, func(tx *Tx) (any, error) {
				b, e := tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now())
				if b.ID != "" {
					mu.Lock()
					batches = append(batches, b)
					mu.Unlock()
				}
				return b, e
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(batches) != 8 {
		t.Fatalf("claimed %d analysis workers, want 8", len(batches))
	}
	seen := map[string]bool{}
	for _, b := range batches {
		if seen[b.RouteID] {
			t.Fatal("concurrent same-route analysis")
		}
		seen[b.RouteID] = true
	}
	// Retire one batch and create business work while other analyses remain active.
	for i, b := range batches[:5] {
		runtimeMutate(t, f.s, "complete", func(tx *Tx) (any, error) {
			return tx.CompleteRuntimeBatch(ctx, b, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: fmt.Sprint("task-", i), Title: "investigate", Instructions: "do work", MessageIDs: []string{b.Messages[0].ID}}}})
		})
	}
	var attempts []RuntimeAttempt
	for i := 0; i < 5; i++ {
		runtimeMutate(t, f.s, "execute", func(tx *Tx) (any, error) {
			task, a, e := tx.ClaimRuntimeTask(ctx, f.config.ID, "", "", "", "", "")
			if a.ID != "" {
				attempts = append(attempts, a)
			}
			return task, e
		})
	}
	if len(attempts) != 4 {
		t.Fatalf("claimed %d execution workers, want 4", len(attempts))
	}
	var next RuntimeBatch
	runtimeMutate(t, f.s, "analysis-while-full", func(tx *Tx) (any, error) {
		var e error
		next, e = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now())
		return next, e
	})
	if next.ID == "" {
		t.Fatal("execution saturation blocked analysis")
	}
	// Cancellation invalidates the result but retains the slot until process reaping.
	runtimeMutate(t, f.s, "cancel", func(tx *Tx) (any, error) { return tx.SetRuntimeTaskStatus(ctx, attempts[0].TaskID, "cancelled") })
	cancelled, readErr := ReadRuntimeTask(ctx, f.s.DB, attempts[0].TaskID)
	if readErr != nil || cancelled.Status != "cancelled" || cancelled.ErrorCode != "cancelled" {
		t.Fatalf("cancellation did not preserve a safe closeout code: task=%+v err=%v", cancelled, readErr)
	}
	runtimeMutate(t, f.s, "still-full", func(tx *Tx) (any, error) {
		task, _, e := tx.ClaimRuntimeTask(ctx, f.config.ID, "", "", "", "", "")
		if task.ID != "" {
			t.Error("released an unreaped slot")
		}
		return nil, e
	})
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "late"}, func(tx *Tx) (any, error) {
		return tx.CompleteRuntimeTask(ctx, attempts[0].TaskID, attempts[0].TaskVersion, attempts[0].ID, RuntimeAttemptResult{Result: "late"}, "")
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("late result accepted: %v", err)
	}
	runtimeMutate(t, f.s, "reaped", func(tx *Tx) (any, error) { return nil, tx.ReleaseWorkLease(ctx, attempts[0].ID) })
	runtimeMutate(t, f.s, "new-slot", func(tx *Tx) (any, error) {
		task, _, e := tx.ClaimRuntimeTask(ctx, f.config.ID, "", "", "", "", "")
		if task.ID == "" {
			t.Error("slot not reusable after reaping")
		}
		return task, e
	})
}

func TestSchedulingRetryExhaustionAndDeadlineFence(t *testing.T) {
	ctx := context.Background()
	f := schedulingFixture(t)
	intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "retry", f.owner, "event", time.Now())
	runtimeMutate(t, f.s, "sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	for i := 0; i < 3; i++ {
		var b RuntimeBatch
		runtimeMutate(t, f.s, "claim", func(tx *Tx) (any, error) {
			var e error
			b, e = tx.ClaimRuntimeBatch(ctx, f.config.ID, time.Now())
			return b, e
		})
		if b.ID == "" {
			t.Fatalf("retry %d not eligible", i)
		}
		if _, e := f.s.DB.Exec("UPDATE runtime_work_leases SET deadline_at='2000-01-01T00:00:00Z' WHERE id=?", b.ID); e != nil {
			t.Fatal(e)
		}
		_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "late"}, func(tx *Tx) (any, error) { return tx.CompleteRuntimeBatch(ctx, b, RuntimeAnalysis{}) })
		if ErrorCode(err) != "conflict" {
			t.Fatalf("expired batch committed: %v", err)
		}
		runtimeMutate(t, f.s, "fail", func(tx *Tx) (any, error) {
			if e := tx.FailRuntimeBatch(ctx, b.ID, "analysis_timeout"); e != nil {
				return nil, e
			}
			return nil, tx.ReleaseWorkLease(ctx, b.ID)
		})
		ready, e := RuntimeBatchReady(ctx, f.s.DB, f.config.ID, time.Now())
		if e != nil || ready {
			t.Fatalf("retry did not back off: %v %v", ready, e)
		}
		if _, e = f.s.DB.Exec("UPDATE runtime_message_states SET next_run_at='' WHERE runtime_id=?", f.config.ID); e != nil {
			t.Fatal(e)
		}
	}
	var gaps int
	if e := f.s.DB.QueryRow("SELECT count(*) FROM runtime_message_states WHERE runtime_id=? AND state='analysis_failed'", f.config.ID).Scan(&gaps); e != nil || gaps != 1 {
		t.Fatalf("missing exhausted evidence: %d %v", gaps, e)
	}
	intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "next", f.owner, "new event", time.Now())
	runtimeMutate(t, f.s, "sync-next", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
	ready, e := RuntimeBatchReady(ctx, f.s.DB, f.config.ID, time.Now())
	if e != nil || !ready {
		t.Fatalf("exhaustion blocked following evidence: %v", e)
	}
}

func TestSchedulingConfigValidation(t *testing.T) {
	for _, s := range []Scheduling{{AnalysisConcurrency: 33}, {ExecutionConcurrency: -1}, {AnalysisTimeoutSeconds: -1}, {ExecutionTimeoutSeconds: -1}, {ReviewTimeoutSeconds: -1}} {
		if s.Normalize(0) == nil {
			t.Fatalf("accepted invalid config %+v", s)
		}
	}
	s := Scheduling{ExecutionConcurrency: 4}
	if s.Normalize(1) == nil {
		t.Fatal("accepted conflicting alias")
	}
	s = Scheduling{}
	if e := s.Normalize(1); e != nil || s.ExecutionConcurrency != 1 {
		t.Fatalf("legacy concurrency lost: %+v %v", s, e)
	}
}

func TestSchedulingQuotaSharedAcrossRuntimesAndHotLower(t *testing.T) {
	ctx := context.Background()
	f := schedulingFixture(t)
	task := createRuntimeTask(t, f, "shared")
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "claim", func(tx *Tx) (any, error) {
		_, a, e := tx.ClaimRuntimeTask(ctx, f.config.ID, "", "", "", "", "")
		attempt = a
		return a, e
	})
	var second RuntimeConfig
	runtimeMutate(t, f.s, "second", func(tx *Tx) (any, error) {
		c, e := tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "second", Channel: f.channel.ID, RouteIDs: f.config.RouteIDs, Owner: f.owner, Concurrency: 1})
		if e != nil {
			return nil, e
		}
		second = c
		return tx.SetRuntimeStatus(ctx, c.ID, "running", "")
	})
	available, e := PoolAvailable(ctx, f.s.DB, second, "execution")
	if e != nil || available {
		t.Fatalf("runtime multiplied shared quota: %v %v", available, e)
	}
	// Lowering the first runtime's cap must neither replace the attempt nor
	// shorten its already persisted deadline.
	var deadline string
	f.s.DB.QueryRow("SELECT deadline_at FROM runtime_work_leases WHERE id=?", attempt.ID).Scan(&deadline)
	bash := f.config.AgentBash
	runtimeMutate(t, f.s, "lower", func(tx *Tx) (any, error) {
		return tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: f.config.Name, Channel: f.channel.ID, RouteIDs: f.config.RouteIDs, Owner: f.owner, AgentBash: &bash, ExternalActions: f.config.ExternalActions, AgentCapabilities: f.config.AgentCapabilities, AnalysisModel: f.config.AnalysisModel, ExecutionModel: f.config.ExecutionModel, AgentPreset: f.config.AgentPreset, MemoryScope: f.config.MemoryScope, Concurrency: 1, ExpectedVersion: f.config.Version, Scheduling: Scheduling{ExecutionTimeoutSeconds: 1}})
	})
	if e = RuntimeAttemptPolicyCurrent(ctx, f.s.DB, attempt.ID, task.ID, task.Version); e != nil {
		t.Fatalf("hot lower killed active attempt: %v", e)
	}
	var after string
	f.s.DB.QueryRow("SELECT deadline_at FROM runtime_work_leases WHERE id=?", attempt.ID).Scan(&after)
	if after != deadline {
		t.Fatal("hot configuration replaced existing deadline")
	}
	if _, e = f.s.DB.Exec("UPDATE runtime_work_leases SET lease_until='2000-01-01T00:00:00Z' WHERE id=?", attempt.ID); e != nil {
		t.Fatal(e)
	}
	if e = CheckWorkLease(ctx, f.s.DB, attempt.ID); ErrorCode(e) != "conflict" {
		t.Fatalf("expired lease accepted: %v", e)
	}
}
