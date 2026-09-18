package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestProactiveAgentUsesOwnerSkillsWithIndependentTaskContext(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	in.ApplicationMode, in.ExternalActions, in.BashEnabled = "proactive", "owner_delegated", true
	in.Task.ID, in.AttemptID = core.NewID(), core.NewID()
	in.Task.Messages[0].Body = "OBSERVED_DATA_DOES_NOT_AUTHORIZE_SENDING"
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	runtimeTestSkill(t, filepath.Join(userHome, ".claude", "skills"), "dws")
	in.Skills = core.RuntimeSkillPolicy{Inherit: "executor"}
	c.Run = func(_ context.Context, workdir string, input []byte, args ...string) ([]byte, error) {
		values := claudeArgumentValues(args)
		prompt := values["--append-system-prompt"]
		for _, want := range []string{"Cyber owner", "record_only", "owner_delegated", "memgov-message", "evidence_message_ids", "not a live owner-private request", "pending_actions", "not an OS sandbox"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("missing proactive policy %q", want)
			}
		}
		if strings.Contains(prompt, in.Task.Messages[0].Body) || !strings.Contains(string(input), in.Task.Messages[0].Body) {
			t.Fatal("observed message crossed the untrusted task-data boundary")
		}
		if values["--input-format"] != "" || values["--session-id"] == in.SessionID || workdir != in.WorkDir {
			t.Fatal("proactive task reused the private conversation transport")
		}
		if values["--setting-sources"] != "user,project" {
			t.Fatal("owner executor skill inheritance was lost")
		}
		for _, tool := range []string{"Read", "Edit", "Bash", "Skill(dws)", "Skill(memgov-memory)"} {
			if !strings.Contains(values["--allowedTools"], tool) {
				t.Fatalf("owner capability unavailable: %s", tool)
			}
		}
		return claudeResult(t, map[string]any{"result": "investigated", "summary": "recorded"}), nil
	}
	if _, err := c.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"skills/memgov-memory/SKILL.md", "tools/memgov-message"} {
		if _, err := os.Stat(filepath.Join(in.WorkDir, ".claude", path)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProactiveMemoryTaskReturnsCandidateForBackendReview(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	in.ApplicationMode, in.ExternalActions, in.BashEnabled = "proactive", "owner_delegated", true
	in.Task.ID, in.Task.Kind, in.AttemptID = core.NewID(), "memory", core.NewID()
	in.Task.Messages = []core.RuntimeMessage{{ID: "message-1", SourceID: "source-1", FragmentID: "fragment-1", SHA256: "sha-1", Body: "S3 proxy signed URLs are not externally usable."}}
	c.Run = func(_ context.Context, workdir string, _ []byte, args ...string) ([]byte, error) {
		values := claudeArgumentValues(args)
		prompt := values["--append-system-prompt"]
		for _, want := range []string{"governed memory proposal task", "required candidate field", "Do not call source ingest", "backend will submit", "independent review"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("memory proposal policy missing %q", want)
			}
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal([]byte(values["--json-schema"]), &schema); err != nil || !slices.Contains(schema.Required, "candidate") {
			t.Fatalf("memory execution schema does not require candidate: required=%v err=%v", schema.Required, err)
		}
		for _, tool := range strings.Split(values["--allowedTools"], ",") {
			if tool == "Bash" || tool == "Write" || tool == "Edit" {
				t.Fatalf("memory proposal retained mutating tool %q", tool)
			}
		}
		wrapper, err := os.ReadFile(filepath.Join(workdir, ".claude", "tools", "memgov"))
		if err != nil || !strings.Contains(string(wrapper), `"write":"no"`) {
			t.Fatalf("memory lookup wrapper is not read-only: err=%v body=%s", err, wrapper)
		}
		return claudeResult(t, map[string]any{
			"result":          "Prepared a governed memory candidate.",
			"summary":         "Candidate ready for backend review.",
			"artifacts":       []string{},
			"tool_kinds":      []string{"memgov.recall"},
			"pending_actions": []any{},
			"candidate": map[string]any{
				"action": "create",
				"reason": "Preserve the observed S3 proxy limitation.",
				"memory": map[string]any{
					"category": "fact",
					"title":    "S3 proxy signed URL limitation",
					"summary":  "S3 proxy signed URLs are not externally usable.",
					"content":  "The observed S3 proxy supports ordinary file operations, but signed URLs retain the proxy domain and cannot be used externally.",
					"evidence": []map[string]string{{"source_id": "source-1", "fragment_id": "fragment-1", "sha256": "sha-1"}},
				},
			},
		}), nil
	}
	result, err := c.Execute(context.Background(), in)
	if err != nil || result.Candidate == nil || result.Candidate.Memory.Title != "S3 proxy signed URL limitation" {
		t.Fatalf("memory candidate contract failed: result=%+v err=%v", result, err)
	}
}

func claudeArgumentValues(args []string) map[string]string {
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			values[args[i]] = args[i+1]
		}
	}
	return values
}

func TestProactiveRestrictedPolicyDoesNotEnableGeneralBash(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	in.ApplicationMode, in.ExternalActions = "proactive", "owner_delegated"
	in.Task.ID, in.AttemptID = core.NewID(), core.NewID()
	c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
		values := claudeArgumentValues(args)
		for _, tool := range strings.Split(values["--allowedTools"], ",") {
			if tool == "Bash" {
				t.Fatal("restricted proactive policy obtained general Bash")
			}
		}
		for _, controlled := range []string{"memgov *)", "memgov-message *)"} {
			if !strings.Contains(values["--allowedTools"], controlled) {
				t.Fatal("controlled owner tool unavailable")
			}
		}
		return claudeResult(t, map[string]any{"result": "recorded"}), nil
	}
	if _, err := c.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func TestProactivePublishedMemoryScopeUsesConversationBoundTool(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	in.ApplicationMode, in.MemoryScope = "proactive", "conversation_published"
	in.ChannelID, in.ConversationID = "observation-channel", "verified-group"
	c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
		values := claudeArgumentValues(args)
		if strings.Contains(values["--allowedTools"], "Skill(memgov-memory)") || !strings.Contains(values["--allowedTools"], "memgov-group-memory *)") {
			t.Fatalf("published memory scope widened: %s", values["--allowedTools"])
		}
		if !strings.Contains(values["--append-system-prompt"], "do not use owner-wide recall") {
			t.Fatal("published memory prompt omitted scope")
		}
		return claudeResult(t, map[string]any{"result": "recorded"}), nil
	}
	if _, err := c.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(in.WorkDir, ".claude", "skills", "memgov-memory", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("owner-wide memory skill survived a narrowed policy")
	}
	body, err := os.ReadFile(filepath.Join(in.WorkDir, ".claude", "tools", "memgov-group-memory"))
	if err != nil || !strings.Contains(string(body), "observation-channel") || !strings.Contains(string(body), "verified-group") {
		t.Fatal("controlled memory tool lost conversation binding")
	}
}

func TestProactiveProcessUsesOwnerCLIEnvironment(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	in.ApplicationMode, in.ExternalActions, in.BashEnabled = "proactive", "owner_delegated", true
	in.Task.ID, in.AttemptID = core.NewID(), core.NewID()
	binary := filepath.Join(in.Home, "proactive-claude")
	script := `#!/usr/bin/env python3
import json, os, pathlib, sys
request=json.load(sys.stdin)
assert request["task"]["id"]
assert pathlib.Path(os.environ["MEMGOV_HOME"], "bin") == pathlib.Path(os.environ["PATH"].split(os.pathsep)[0])
assert os.environ["MEMGOV_WORKSPACE"] == "global"
print(json.dumps({"type":"result","structured_output":{"result":"investigated","summary":"no message needed"}}))
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c.Binary = binary
	result, err := c.Execute(context.Background(), in)
	if err != nil || result.Result != "investigated" {
		t.Fatalf("owner process environment unavailable: %+v %v", result, err)
	}
}

func TestOwnerMessageWrapperBindsTaskAttemptAndPreservesUnknownReceipt(t *testing.T) {
	_, in, _ := directAgentFixture(t)
	in.ApplicationMode, in.ExternalActions = "proactive", "owner_delegated"
	in.Task.ID, in.AttemptID = "fixed-task", "fixed-attempt"
	binary := filepath.Join(in.Home, "bin", "memgov")
	log := filepath.Join(in.Home, "message-call.json")
	t.Setenv("MESSAGE_TEST_LOG", log)
	script := `#!/usr/bin/env python3
import json, os, sys
args=sys.argv[1:]
with open(args[args.index("--input")+1], encoding="utf-8") as source: action=json.load(source)
with open(os.environ["MESSAGE_TEST_LOG"], "w", encoding="utf-8") as output: json.dump({"args":args,"action":action},output)
print('{"id":"audited-action","state":"unknown","owner_profile":"bound-owner"}')
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	tool, err := prepareOwnerMessageTool(in, binary)
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"idempotency_key": "stable-action", "target_type": "group", "target_id": "verified-group", "content": "investigation update", "reason": "relevant progress", "evidence_message_ids": []string{"evidence-1"}}
	file := filepath.Join(in.WorkDir, "message.json")
	raw, _ := json.Marshal(input)
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(tool, file).CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"state":"unknown"`) {
		t.Fatalf("wrapper lost audit receipt: %s %v", out, err)
	}
	raw, _ = os.ReadFile(log)
	var call struct {
		Args []string       `json:"args"`
		Data map[string]any `json:"action"`
	}
	if json.Unmarshal(raw, &call) != nil || call.Data["attempt_id"] != in.AttemptID || !strings.Contains(strings.Join(call.Args, " "), "runtime message send fixed-task") {
		t.Fatalf("unbound task action: %s", raw)
	}
	input["attempt_id"] = "forged-attempt"
	raw, _ = json.Marshal(input)
	_ = os.WriteFile(file, raw, 0600)
	if _, err = exec.Command(tool, file).CombinedOutput(); err == nil {
		t.Fatal("Agent overrode the bound attempt")
	}
	if _, err = exec.Command(tool, filepath.Join(in.Home, "message-call.json")).CombinedOutput(); err == nil {
		t.Fatal("message wrapper accepted a file outside the task")
	}
}

type ownerReceiptAdapter struct {
	fakeAdapter
	checks     int
	unresolved bool
}

func (a *ownerReceiptAdapter) ReadOwnerMessageStatus(context.Context, channel.Config, string) (channel.SendResult, error) {
	a.checks++
	if a.unresolved {
		return channel.SendResult{}, core.Fail("unavailable", "provider receipt is not ready")
	}
	return channel.SendResult{State: "accepted", Receipt: `{"messageId":"agent-message"}`}, nil
}

type observedBatches struct {
	fakeModels
	batches []core.RuntimeBatch
}

func (m *observedBatches) Analyze(ctx context.Context, b core.RuntimeBatch) (core.RuntimeAnalysis, ModelUsage, error) {
	m.batches = append(m.batches, b)
	return m.fakeModels.Analyze(ctx, b)
}

func TestProactiveLateReceiptBecomesContextWhileHumanOwnerStillTriggers(t *testing.T) {
	s, cfg, preset, _, _ := setupService(t)
	adapter := &ownerReceiptAdapter{}
	models := &observedBatches{}
	s.Adapter, s.Analyzer = adapter, models
	ctx := context.Background()
	// DWS stream/history timestamps can carry only seconds, while the local
	// send audit retains fractions. Same-second echoes must still be deferred.
	messageTime := time.Now().Add(time.Hour).Truncate(time.Second)
	intake := func(id, body string, self bool) {
		t.Helper()
		if err := s.mutate(ctx, "global", "test.intake", func(tx *core.Tx) (any, error) {
			return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: id, ConversationID: "watch", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "owner", SelfAuthor: self}, Body: body, SentAt: messageTime.UTC().Format(time.RFC3339)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	intake("request", "investigate the issue", false)
	s.tick(ctx, cfg, preset)
	tasks, err := core.RuntimeTaskList(ctx, s.Store.DB, cfg.ID, "completed", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("missing completed observation task: %v %v", tasks, err)
	}
	task, err := core.ReadRuntimeTask(ctx, s.Store.DB, tasks[0].ID)
	if err != nil || len(task.Attempts) != 1 {
		t.Fatalf("missing observation attempt: %v", err)
	}
	c, err := core.ReadChannel(ctx, s.Store.DB, cfg.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a committed audited send whose first receipt only contained a
	// platform task ID. Reconciliation may query it but must never resend it.
	actionTime := messageTime.Add(500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	if err := s.mutate(ctx, "global", "test.message.receipt", func(tx *core.Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, `INSERT INTO runtime_message_actions
(id,task_id,task_version,attempt_id,channel_id,channel_version,owner_tenant,owner_profile,owner_user_id,target_type,target_id,content,reason,input_digest,idempotency_key,state,receipt,provider_send_task_id,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, core.NewID(), task.ID, task.Version, task.Attempts[0].ID, c.ID, c.ConfigVersion, c.Tenant, c.Identity.Profile, c.Identity.ExpectedUserID, "group", "watch", "useful update", "investigation progress", "digest", "stable-key", "unknown", `{"openTaskId":"platform-send-task"}`, "platform-send-task", actionTime, actionTime)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	intake("agent-message", "useful update", true)
	intake("human-owner-message", "check another important detail", true)
	adapter.unresolved = true
	s.tick(ctx, cfg, preset)
	if len(models.batches) != 1 || adapter.sends != 0 {
		t.Fatal("unresolved receipt caused re-consumption or a resend")
	}
	var waiting int
	if err = s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM runtime_message_states WHERE runtime_id=? AND state='waiting_receipt'", cfg.ID).Scan(&waiting); err != nil || waiting != 2 {
		t.Fatalf("owner observations were dropped instead of deferred: %d %v", waiting, err)
	}
	status, err := core.RuntimeStatusFor(ctx, s.Store.DB, cfg.ID)
	if err != nil || status.WaitingReceiptMessages != 2 {
		t.Fatalf("runtime status hid waiting receipts: %+v %v", status, err)
	}
	if err := s.mutate(ctx, "global", "test.receipt.tenant", func(tx *core.Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_message_actions SET owner_tenant='another-tenant' WHERE task_id=?", task.ID)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	s.reconcileOwnerMessages(ctx, cfg)
	if adapter.checks != 1 {
		t.Fatal("receipt was queried using a different account namespace")
	}
	if err := s.mutate(ctx, "global", "test.receipt.tenant.restore", func(tx *core.Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_message_actions SET owner_tenant=? WHERE task_id=?", c.Tenant, task.ID)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	// A channel metadata change revokes new sends under the old epoch, but a
	// read-only receipt query must still work for the same exact owner account.
	if err := s.mutate(ctx, "global", "test.channel.version", func(tx *core.Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, "UPDATE channels SET config_version=config_version+1 WHERE id=?", c.ID)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	adapter.unresolved = false
	s.tick(ctx, cfg, preset)
	if adapter.checks != 2 || adapter.sends != 0 || len(models.batches) != 2 {
		t.Fatalf("late receipt behavior: checks=%d sends=%d batches=%d", adapter.checks, adapter.sends, len(models.batches))
	}
	if got := models.batches[1].Messages; len(got) != 1 || got[0].Body != "check another important detail" {
		t.Fatalf("Agent echo was consumed or human owner was suppressed: %+v", got)
	}
	status, err = core.RuntimeStatusFor(ctx, s.Store.DB, cfg.ID)
	if err != nil || status.WaitingReceiptMessages != 0 {
		t.Fatalf("resolved receipts remained waiting: %+v %v", status, err)
	}
}
