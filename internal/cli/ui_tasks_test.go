package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func taskResumeFixture(t *testing.T) (string, core.RuntimeTask) {
	t.Helper()
	home, config := dualOwnerPrivateFixture(t)
	ctx := context.Background()
	s, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := core.NewID()
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "test.interrupted.private.task"}, func(tx *core.Tx) (any, error) {
		if _, err := tx.SetRuntimeStatus(ctx, config.ID, "running", ""); err != nil {
			return nil, err
		}
		route, err := core.ReadRoute(ctx, tx.Conn, config.RouteIDs[0])
		if err != nil {
			return nil, err
		}
		message, err := tx.Intake(ctx, config.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/4", Origin: "stream", ProviderMessageID: core.NewID(), ConversationID: route.ConversationID, ConversationType: "direct", Tenant: "corp1", Sender: core.Sender{IDType: config.OwnerIDType, IDValue: config.OwnerIDValue}, Body: "原先的请求", SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Conn.ExecContext(ctx, "INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,title,instructions,status,error_code,created_at,updated_at) VALUES(?,?,?,?,'test','原先的请求','failed','runtime_restarted',?,?)", id, config.ID, route.ID, id, core.Now(), core.Now()); err != nil {
			return nil, err
		}
		if _, err := tx.Conn.ExecContext(ctx, "INSERT INTO runtime_task_messages(task_id,message_id,revision) VALUES(?,?,1)", id, message.MessageID); err != nil {
			return nil, err
		}
		if _, err := tx.Conn.ExecContext(ctx, "INSERT INTO runtime_attempts(id,task_id,task_version,status,model,workspace_dir,input_digest,error_code,started_at,finished_at) VALUES(?,?,1,'failed','profile',?,'digest','runtime_restarted',?,?)", core.NewID(), id, filepath.Join(home, "runtime", "sessions", "old"), core.Now(), core.Now()); err != nil {
			return nil, err
		}
		return tx.SetRuntimeStatus(ctx, config.ID, "paused", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := core.ReadRuntimeTask(ctx, s.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	return home, task
}

func TestCLITaskResumeQueuesContinuationAndWakesModule(t *testing.T) {
	home, task := taskResumeFixture(t)
	code, raw := invoke(t, home, "", "runtime", "task", "resume", task.ID)
	if code != 0 || data(t, raw)["status"] != "pending" {
		t.Fatalf("CLI continuation: code=%d result=%v", code, raw)
	}
	if data(t, raw)["resume"].(map[string]any)["prompt"] != "继续" {
		t.Fatal("CLI continuation lost its instruction")
	}
	code, raw = invoke(t, home, "", "runtime", "status", task.RuntimeID)
	if code != 0 || data(t, raw)["runtime"].(map[string]any)["status"] != "running" {
		t.Fatalf("paused module was not resumed: %v", raw)
	}
	code, _ = invoke(t, home, "", "runtime", "task", "resume", task.ID)
	if code != 3 {
		t.Fatalf("duplicate CLI continuation: code=%d", code)
	}
}

func TestWebTaskResumeUsesVersionAndKeepsMutationAuditable(t *testing.T) {
	home, task := taskResumeFixture(t)
	control := taskResumeControl(home)
	if _, err := control(context.Background(), task.ID, task.Version+1); core.ErrorCode(err) != "conflict" {
		t.Fatalf("stale Web continuation: %v", err)
	}
	result, err := control(context.Background(), task.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["mode"] != "replay" || result.(map[string]any)["runtime_status"] != "running" {
		t.Fatalf("Web continuation result: %v", result)
	}
	s, err := core.Open(context.Background(), filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var operations int
	if err := s.DB.QueryRow("SELECT count(*) FROM requests WHERE command='runtime.task.resume' AND actor='console-owner' AND ok=1").Scan(&operations); err != nil || operations != 1 {
		t.Fatalf("Web continuation audit: count=%d error=%v", operations, err)
	}
}
