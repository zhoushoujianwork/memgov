package core

import (
	"context"
	"path/filepath"
	"testing"
)

func interruptedTaskFixture(t *testing.T, native bool) (runtimeFixture, RuntimeTask, RuntimeAttempt) {
	t.Helper()
	f := newRuntimeFixture(t, 1, 300)
	created := createRuntimeTask(t, f, "continue")
	var task RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "test.claim.continue", func(tx *Tx) (any, error) {
		var err error
		task, attempt, err = tx.ClaimRuntimeTask(context.Background(), f.config.ID, NewID(), "profile", "preset", "commit", filepath.Join(filepath.Dir(f.s.Path), "runtime", "tasks", created.ID, "previous"))
		if err == nil && native {
			err = tx.RecordRuntimeAgentSession(context.Background(), task.ID, task.Version, attempt.ID, RuntimeAgentSession{ID: attempt.ID, PolicyDigest: "policy", ContextDigest: "context", Branch: "codex/runtime-test", Base: "base"})
		}
		return task, err
	})
	runtimeMutate(t, f.s, "test.recover.continue", func(tx *Tx) (any, error) {
		return tx.RecoverRuntime(context.Background(), f.config.ID)
	})
	task, err := ReadRuntimeTask(context.Background(), f.s.DB, task.ID)
	if err != nil || task.Status != "failed" || task.ErrorCode != "runtime_restarted" {
		t.Fatalf("recovery: status=%s error=%v", task.Status, err)
	}
	return f, task, attempt
}

func TestRuntimeTaskResumePreservesPreviousAttemptAndQueuesContinuation(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "legacy progress"
		mode := "replay"
		if native {
			name, mode = "native session", "native"
		}
		t.Run(name, func(t *testing.T) {
			f, before, previous := interruptedTaskFixture(t, native)
			var resumed RuntimeTask
			runtimeMutate(t, f.s, "test.resume", func(tx *Tx) (any, error) {
				var err error
				resumed, err = tx.ResumeRuntimeTask(context.Background(), before.ID, before.Version)
				return resumed, err
			})
			if resumed.Status != "pending" || resumed.Version != before.Version+1 || resumed.Resume == nil || resumed.Resume.Mode != mode || resumed.Resume.Prompt != "继续" || resumed.Resume.FromAttemptID != previous.ID {
				t.Fatalf("continuation was not queued: %+v", resumed.Resume)
			}
			if resumed.Instructions != before.Instructions || resumed.Messages[0].Body != before.Messages[0].Body || resumed.Attempts[0].Status != "failed" || resumed.Attempts[0].WorkspaceDir != previous.WorkspaceDir {
				t.Fatal("continuation replaced the original request or previous attempt")
			}
			if native && resumed.Attempts[0].AgentSession.ID != previous.ID {
				t.Fatal("recovery lost the saved native session")
			}
			_, err := f.s.Mutate(context.Background(), Request{Scope: "global", Command: "test.resume.again"}, func(tx *Tx) (any, error) {
				return tx.ResumeRuntimeTask(context.Background(), before.ID, before.Version)
			})
			if ErrorCode(err) != "conflict" {
				t.Fatalf("duplicate continuation: %v", err)
			}
		})
	}
}

func TestRuntimeTaskResumeRejectsChangedSourcesAndUnknownActions(t *testing.T) {
	for _, change := range []string{"source", "route", "external outcome", "task version"} {
		t.Run(change, func(t *testing.T) {
			f, task, _ := interruptedTaskFixture(t, false)
			expected := task.Version
			switch change {
			case "source":
				if _, err := f.s.DB.Exec("UPDATE messages SET availability='recalled' WHERE id=?", task.Messages[0].ID); err != nil {
					t.Fatal(err)
				}
			case "route":
				if _, err := f.s.DB.Exec("UPDATE channel_routes SET mode='ignore' WHERE id=?", task.RouteID); err != nil {
					t.Fatal(err)
				}
			case "external outcome":
				if _, err := f.s.DB.Exec("INSERT INTO runtime_pending_actions(id,task_id,task_version,kind,target,payload,payload_digest,status,created_at,updated_at) VALUES(?,?,?,'send','target','payload','digest','unknown',?,?)", NewID(), task.ID, task.Version, Now(), Now()); err != nil {
					t.Fatal(err)
				}
			case "task version":
				expected++
			}
			_, err := f.s.Mutate(context.Background(), Request{Scope: "global", Command: "test.resume.denied"}, func(tx *Tx) (any, error) {
				return tx.ResumeRuntimeTask(context.Background(), task.ID, expected)
			})
			if ErrorCode(err) != "conflict" && ErrorCode(err) != "denied" {
				t.Fatalf("unsafe continuation accepted: %v", err)
			}
			after, err := ReadRuntimeTask(context.Background(), f.s.DB, task.ID)
			if err != nil || after.Status != "failed" || after.Resume != nil || after.Version != task.Version {
				t.Fatal("rejected continuation changed the task")
			}
		})
	}
}
