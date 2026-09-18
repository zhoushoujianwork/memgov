package core

import (
	"context"
	"testing"
)

func TestTaskOutputSharesDirectClearAndSourceRevisionBarriers(t *testing.T) {
	f, task, _ := interruptedTaskFixture(t, false)
	ctx := context.Background()
	current, err := RuntimeTaskOutputCurrent(ctx, f.s.DB, task.ID)
	if err != nil || !current {
		t.Fatalf("current output denied: %v", err)
	}
	session := NewID()
	if _, err = f.s.DB.Exec("INSERT INTO runtime_direct_sessions(id,runtime_id,route_id,created_at,closed_at) VALUES(?,?,?,?,?)", session, task.RuntimeID, task.RouteID, Now(), Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("INSERT INTO runtime_direct_turns(task_id,session_id) VALUES(?,?)", task.ID, session); err != nil {
		t.Fatal(err)
	}
	current, err = RuntimeTaskOutputCurrent(ctx, f.s.DB, task.ID)
	if err != nil || current {
		t.Fatalf("closed session output visible: %v", err)
	}
	if _, err = f.s.DB.Exec("DELETE FROM runtime_direct_turns WHERE task_id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("UPDATE runtime_task_messages SET revision=revision+1 WHERE task_id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	current, err = RuntimeTaskOutputCurrent(ctx, f.s.DB, task.ID)
	if err != nil || current {
		t.Fatalf("changed source output visible: %v", err)
	}
}
