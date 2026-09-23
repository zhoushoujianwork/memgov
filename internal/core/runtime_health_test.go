package core

import (
	"context"
	"testing"
)

func TestRuntimeWorkStatusSeparatesGroupAndProactivePools(t *testing.T) {
	ctx := context.Background()
	f := schedulingFixture(t)
	configure := func(name, mode, status string, analysis, execution int) RuntimeConfig {
		t.Helper()
		var config RuntimeConfig
		runtimeMutate(t, f.s, "health.configure", func(tx *Tx) (any, error) {
			var err error
			config, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: name, Channel: f.channel.ID, RouteIDs: f.config.RouteIDs, Owner: f.owner,
				Scheduling: Scheduling{AnalysisConcurrency: analysis, ExecutionConcurrency: execution}})
			return config, err
		})
		// Health reads durable scheduling rows, independently of channel adapters.
		if _, err := f.s.DB.Exec("UPDATE runtime_configs SET application_mode=?,status=? WHERE id=?", mode, status, config.ID); err != nil {
			t.Fatal(err)
		}
		config, err := ReadRuntime(ctx, f.s.DB, config.ID)
		if err != nil {
			t.Fatal(err)
		}
		return config
	}
	second := configure("proactive-second", "proactive", "paused", 3, 2)
	configure("proactive-stopped", "proactive", "stopped", 1, 1)
	group := configure("group-one", "group_mention", "running", 7, 5)
	otherGroup := configure("group-two", "group_mention", "running", 9, 6)
	lease := func(config RuntimeConfig, kind string, released bool) {
		t.Helper()
		runtimeMutate(t, f.s, "health.lease", func(tx *Tx) (any, error) {
			id := NewID()
			if err := tx.claimWork(ctx, config, kind, id, "", "", 0); err != nil {
				return nil, err
			}
			if released {
				return nil, tx.ReleaseWorkLease(ctx, id)
			}
			return nil, nil
		})
	}
	lease(f.config, "analysis", false)
	lease(f.config, "execution", false)
	lease(second, "analysis", false)
	lease(second, "execution", false)
	lease(group, "analysis", false)
	lease(group, "execution", false)
	lease(group, "execution", true)
	lease(otherGroup, "analysis", false)
	lease(otherGroup, "execution", false)
	lease(otherGroup, "execution", false)
	statuses, err := readRuntimeWorkStatuses(ctx, f.s.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		config                         RuntimeConfig
		analysis, execution, aCap, cap int
	}{
		{f.config, 2, 2, 3, 2},
		{second, 2, 2, 3, 2},
		{group, 1, 1, 7, 5},
		{otherGroup, 1, 2, 9, 6},
	} {
		got := statuses[test.config.ID]
		if got.AnalysisActive != test.analysis || got.ExecutionActive != test.execution || got.AnalysisLimit != test.aCap || got.ExecutionLimit != test.cap {
			t.Fatalf("%s reports another pool's workers or limits: %+v", test.config.Name, got)
		}
		single, err := ReadRuntimeWorkStatus(ctx, f.s.DB, test.config)
		if err != nil || single != got {
			t.Fatalf("single runtime health differs from aggregate: %+v %v", single, err)
		}
	}
}

func TestRuntimeWorkStatusCountsGroupActionsAndUnreapedWorkersOnce(t *testing.T) {
	ctx := context.Background()
	f := groupSchedulingFixture(t, 3)
	queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "alice", "health-action")
	task, attempt := claimGroupSchedulingTask(t, f)
	var pending RuntimeTask
	runtimeMutate(t, f.s, "health.propose", func(tx *Tx) (any, error) {
		var err error
		pending, err = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, RuntimeAttemptResult{Result: "prepared", Actions: []RuntimeAction{{Kind: "git_push", Target: "origin/main", Payload: "push prepared commit"}}})
		return pending, err
	})
	if len(pending.Actions) != 1 {
		t.Fatalf("missing pending action: %+v", pending)
	}
	if _, err := f.s.DB.Exec("UPDATE runtime_pending_actions SET status='confirmed',confirmed_by=? WHERE id=?", f.config.OwnerPrincipalID, pending.Actions[0].ID); err != nil {
		t.Fatal(err)
	}
	var action RuntimePendingAction
	var actionAttempt RuntimeActionAttempt
	runtimeMutate(t, f.s, "health.action", func(tx *Tx) (any, error) {
		var err error
		action, actionAttempt, err = tx.ClaimRuntimeAction(ctx, f.config.ID, "", "model")
		return action, err
	})
	queueGroupSchedulingTask(t, f, f.dwsA.ConversationID, "bob", "health-task")
	other, otherAttempt := claimGroupSchedulingTask(t, f)
	assertActive := func(want int) {
		t.Helper()
		status, err := ReadRuntimeWorkStatus(ctx, f.s.DB, f.config)
		if err != nil || status.ExecutionActive != want || status.ExecutionLimit != 3 {
			t.Fatalf("group active=%d limit=%d error=%v; want %d/3", status.ExecutionActive, status.ExecutionLimit, err, want)
		}
	}
	assertActive(2)
	// Legacy running attempts without a lease still occupy group capacity.
	if _, err := f.s.DB.Exec("DELETE FROM runtime_work_leases WHERE id=?", actionAttempt.ID); err != nil {
		t.Fatal(err)
	}
	assertActive(2)
	runtimeMutate(t, f.s, "health.cancel", func(tx *Tx) (any, error) { return tx.SetRuntimeTaskStatus(ctx, other.ID, "cancelled") })
	assertActive(2)
	runtimeMutate(t, f.s, "health.reap", func(tx *Tx) (any, error) { return nil, tx.ReleaseWorkLease(ctx, otherAttempt.ID) })
	assertActive(1)
	runtimeMutate(t, f.s, "health.unknown", func(tx *Tx) (any, error) {
		return tx.UnknownRuntimeAction(ctx, action.ID, actionAttempt.ID, action.TaskVersion, "connection_lost")
	})
	assertActive(0)
}
