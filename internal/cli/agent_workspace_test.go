package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agentworkspace"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func workspaceTaskFixture(t *testing.T) (string, *core.Store, core.RuntimeConfig, core.RuntimeTask, core.RuntimeAttempt) {
	t.Helper()
	home, c := dualOwnerPrivateFixture(t)
	ctx := context.Background()
	s, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	mutate := func(fn func(*core.Tx) (any, error)) {
		t.Helper()
		if _, err := s.Mutate(ctx, core.Request{Command: "test.workspace"}, fn); err != nil {
			t.Fatal(err)
		}
	}
	mutate(func(tx *core.Tx) (any, error) { return tx.SetRuntimeStatus(ctx, c.ID, "running", "") })
	c, err = core.ReadRuntime(ctx, s.DB, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	route, err := core.ReadRoute(ctx, s.DB, c.RouteIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	ch, err := core.ReadChannel(ctx, s.DB, c.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	var message core.IntakeResult
	mutate(func(tx *core.Tx) (any, error) {
		var err error
		message, err = tx.Intake(ctx, ch.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "workspace-request", ConversationID: route.ConversationID, ConversationType: "direct", Tenant: ch.Tenant, Sender: core.Sender{IDType: c.OwnerIDType, IDValue: c.OwnerIDValue}, Body: "Remember the verified outcome", SentAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)})
		return message, err
	})
	mutate(func(tx *core.Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, c.ID) })
	var batch core.RuntimeBatch
	mutate(func(tx *core.Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, c.ID, time.Now().Add(time.Minute))
		return batch, err
	})
	mutate(func(tx *core.Tx) (any, error) {
		return tx.CompleteRuntimeBatch(ctx, batch, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "workspace", Title: "Record result", Instructions: "Record verified result", MessageIDs: []string{message.MessageID}}}})
	})
	var task core.RuntimeTask
	var attempt core.RuntimeAttempt
	mutate(func(tx *core.Tx) (any, error) {
		var err error
		task, attempt, err = tx.ClaimRuntimeTask(ctx, c.ID, "", "", "", "", "")
		return task, err
	})
	mutate(func(tx *core.Tx) (any, error) {
		p, err := core.ResolveRuntimeTaskAgent(ctx, tx.Conn, c, task)
		if err != nil {
			return nil, err
		}
		return nil, tx.RecordRuntimeAttemptAgent(ctx, c, task, attempt.ID, p, "")
	})
	ref, err := agentworkspace.OwnerRef(c.OwnerPrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = agentworkspace.Ensure(home, ref); err != nil {
		t.Fatal(err)
	}
	task, err = core.ReadRuntimeTask(ctx, s.DB, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return home, s, c, task, attempt
}

func TestWorkspaceCLIRechecksAttemptAndKeepsBodiesOutOfSQLite(t *testing.T) {
	home, s, _, task, attempt := workspaceTaskFixture(t)
	base := []string{"agent", "workspace"}
	call := func(input, op string, extra ...string) (int, map[string]any) {
		args := append(append([]string{}, base...), op, "--task", task.ID, "--attempt", attempt.ID)
		args = append(args, extra...)
		return invoke(t, home, input, args...)
	}
	code, out := call("", "read", "--path", "MEMORY.md")
	if code != 0 {
		t.Fatal(code, out)
	}
	input := `{"path":"notes/result.md","content":"UNIQUE_KNOWLEDGE_BODY_NOT_IN_DB","expected_digest":""}`
	if code, out = call(input, "write", "--idempotency-key", "workspace-key"); code != 0 {
		t.Fatal(code, out)
	}
	if _, ok := data(t, out)["content"]; ok {
		t.Fatal("write returned/cacheable body")
	}
	if code, out = call(input, "write", "--idempotency-key", "workspace-key"); code != 0 || out["cached"] != true {
		t.Fatal("write replay omitted cached envelope", code, out)
	}
	if code, out = call("", "read", "--path", "notes/result.md"); code != 0 || data(t, out)["content"] != "UNIQUE_KNOWLEDGE_BODY_NOT_IN_DB" {
		t.Fatal(code, out)
	}
	for _, args := range [][]string{{"read", "--path", "../owner-other/MEMORY.md"}, {"read", "--path", "MEMORY.md", "--workspace-id", "owner-other"}} {
		if code, out = call("", args[0], args[1:]...); code == 0 {
			t.Fatal("scope escaped", out)
		}
	}
	if _, err := s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil || strings.Contains(string(raw), "UNIQUE_KNOWLEDGE_BODY_NOT_IN_DB") {
		t.Fatal("knowledge body duplicated in SQLite", err)
	}
	if _, err = s.Mutate(context.Background(), core.Request{}, func(tx *core.Tx) (any, error) {
		return tx.SetRuntimeTaskStatus(context.Background(), task.ID, "cancelled")
	}); err != nil {
		t.Fatal(err)
	}
	// Even an idempotent replay must re-check the current task before returning.
	if code, out = call(input, "write", "--idempotency-key", "workspace-key"); code == 0 {
		t.Fatal("cancelled attempt replayed write", out)
	}
	if code, out = call("", "read", "--path", "MEMORY.md"); code == 0 {
		t.Fatal("cancelled task read index", out)
	}
}

func TestWorkspaceCLIRevocationFencesAllOperations(t *testing.T) {
	for _, revoke := range []string{"owner", "policy", "route", "attempt", "evidence", "clear"} {
		t.Run(revoke, func(t *testing.T) {
			home, s, c, task, attempt := workspaceTaskFixture(t)
			queries := map[string]string{
				"owner":    "UPDATE identity_aliases SET verified=0",
				"policy":   "UPDATE runtime_configs SET external_actions='owner_request'",
				"route":    "UPDATE channel_routes SET status='disabled'",
				"attempt":  "UPDATE runtime_attempts SET status='failed'",
				"evidence": "UPDATE messages SET availability='recalled'",
			}
			if revoke == "clear" {
				_, err := s.Mutate(context.Background(), core.Request{}, func(tx *core.Tx) (any, error) {
					_, err := tx.BindRuntimeDirectTurn(context.Background(), c, task)
					if err != nil {
						return nil, err
					}
					_, err = tx.Conn.ExecContext(context.Background(), "UPDATE runtime_direct_sessions SET closed_at=?", core.Now())
					return nil, err
				})
				if err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.DB.Exec(queries[revoke]); err != nil {
				t.Fatal(err)
			}
			for _, op := range []string{"list", "read", "search", "history", "write"} {
				args := []string{"agent", "workspace", op, "--task", task.ID, "--attempt", attempt.ID}
				if op == "read" || op == "history" {
					args = append(args, "--path", "MEMORY.md")
				}
				if op == "search" {
					args = append(args, "--query", "Index")
				}
				if code, out := invoke(t, home, `{"path":"notes/stale.md","content":"must not write","expected_digest":""}`, args...); code == 0 {
					t.Fatal(revoke, op, out)
				}
			}
		})
	}
}
