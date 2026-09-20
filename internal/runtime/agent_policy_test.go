package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

func TestGroupDirectorySnapshotsAreBoundedReadOnlyAndRejectSymlinks(t *testing.T) {
	ctx := context.Background()
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "notes.txt"), []byte("group-approved notes"), 0600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(source, ".env"), []byte("private"), 0600)
	files, err := stageGroupDirectories(ctx, work, []string{source})
	if err != nil || len(files) != 1 || files[0].Content != "group-approved notes" || strings.Contains(files[0].Path, source) {
		t.Fatalf("snapshot %+v %v", files, err)
	}
	info, err := os.Stat(filepath.Join(work, files[0].Path))
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("copy permissions %+v %v", info, err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("outside"), 0600)
	os.Symlink(outside, filepath.Join(source, "escape"))
	if _, err = stageGroupDirectories(ctx, t.TempDir(), []string{source}); core.ErrorCode(err) != "denied" {
		t.Fatalf("symlink accepted: %v", err)
	}
	os.Remove(filepath.Join(source, "escape"))
	os.WriteFile(filepath.Join(source, "large"), make([]byte, snapshotMaxFileBytes+1), 0600)
	if _, err = stageGroupDirectories(ctx, t.TempDir(), []string{source}); core.ErrorCode(err) != "denied" {
		t.Fatalf("unbounded file accepted: %v", err)
	}
}
func TestGroupArtifactsCannotReferenceHostInputsOrSymlinks(t *testing.T) {
	work := t.TempDir()
	os.Mkdir(filepath.Join(work, "artifacts"), 0700)
	os.Mkdir(filepath.Join(work, "inputs"), 0700)
	os.WriteFile(filepath.Join(work, "artifacts", "answer.md"), []byte("answer"), 0600)
	os.WriteFile(filepath.Join(work, "inputs", "private.txt"), []byte("input"), 0600)
	out, err := validateGroupArtifacts(work, []string{"artifacts/answer.md"})
	if err != nil || len(out) != 1 || out[0] != filepath.Join(work, "artifacts", "answer.md") {
		t.Fatalf("artifact %v %v", out, err)
	}
	os.Symlink(filepath.Join(work, "inputs", "private.txt"), filepath.Join(work, "artifacts", "link"))
	for _, bad := range []string{"inputs/private.txt", "artifacts/../inputs/private.txt", "../outside", "https://example.test/capability", "artifacts/link", work} {
		if _, err = validateGroupArtifacts(work, []string{bad}); core.ErrorCode(err) != "denied" {
			t.Errorf("accepted %q: %v", bad, err)
		}
	}
}
func TestGroupClaudeOnlyAllowsScopedArtifactToolsAndSelectedModel(t *testing.T) {
	preset, err := agent.Enable(context.Background(), t.TempDir(), "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	var input string
	c := &Claude{ExecutionModel: "owner-model", Profile: "owner", Run: func(_ context.Context, _ string, raw []byte, argv ...string) ([]byte, error) {
		args = argv
		input = string(raw)
		return claudeResult(t, core.RuntimeAttemptResult{Result: "answer"}), nil
	}}
	_, err = c.Execute(context.Background(), ExecutionInput{WorkDir: t.TempDir(), Preset: preset, ApplicationMode: "group_mention", Capabilities: []string{"local_read", "local_write", "artifact_create"}, PolicyResolved: true, ExecutionModel: "group-model", DirectorySnapshots: []DirectorySnapshot{{Path: "inputs/001/notes", Content: "approved"}}})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for i := 0; i < len(args)-1; i++ {
		if strings.HasPrefix(args[i], "--") {
			values[args[i]] = args[i+1]
		}
	}
	if values["--model"] != "group-model" || values["--tools"] != "Write,Edit" || values["--allowedTools"] != "Edit(./artifacts/**)" || values["--setting-sources"] != "" {
		t.Fatalf("unsafe invocation %+v", values)
	}
	if !strings.Contains(input, "approved") || strings.Contains(values["--allowedTools"], "Read") || strings.Contains(values["--allowedTools"], "Bash") {
		t.Fatal("group tool boundary not enforced")
	}
	if c.ExecutionModel != "owner-model" || c.Profile != "owner" {
		t.Fatal("task override mutated shared Claude")
	}
}

type groupPolicyExecutor struct {
	input ExecutionInput
	calls int
}

func (e *groupPolicyExecutor) Execute(_ context.Context, in ExecutionInput) (core.RuntimeAttemptResult, error) {
	e.input = in
	e.calls++
	return core.RuntimeAttemptResult{Result: "group answer", Summary: "done"}, nil
}

func TestGroupServiceExecutesSelectedPresetAndRecordsActualAttempt(t *testing.T) {
	service, original, _, _, adapter := setupService(t)
	ctx := context.Background()
	preset, err := agent.Enable(ctx, service.Home, "claude", "group-special")
	if err != nil {
		t.Fatal(err)
	}
	dws, err := core.ReadChannel(ctx, service.Store.DB, original.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	var app core.Channel
	var route core.Route
	var cfg core.RuntimeConfig
	err = service.mutate(ctx, "global", "test.group.setup", func(tx *core.Tx) (any, error) {
		var e error
		app, e = tx.AddChannel(ctx, core.ChannelInput{Name: "app-group-policy", Kind: core.ChannelDingTalkApp, Identity: core.ChannelIdentity{ExpectedCorpID: dws.Tenant, ClientID: "client", RobotCode: "robot", HistoryChannel: dws.ID}})
		if e != nil {
			return nil, e
		}
		app, e = tx.SetChannelCapabilities(ctx, app.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "send": true}}, "fake")
		if e != nil {
			return nil, e
		}
		route, e = tx.AddRoute(ctx, app.ID, core.RouteInput{ConversationID: "watch", ConversationType: "group", Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		if e != nil {
			return nil, e
		}
		cfg, e = tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Name: "group-policy", Channel: app.ID, RouteIDs: []string{route.ID}, DeliveryRouteID: route.ID, Owner: core.Sender{IDType: "user_id", IDValue: "owner"}, ApplicationMode: "group_mention", ContextChannel: dws.ID, AgentPreset: "claude-default"})
		if e != nil {
			return nil, e
		}
		cfg, e = tx.SetRuntimeStatus(ctx, cfg.ID, "running", "")
		return cfg, e
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Store.DB.ExecContext(ctx, "UPDATE workspaces SET path=? WHERE id=?", t.TempDir(), route.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	declaration := core.JSON(map[string]any{"agents": map[string]core.RuntimeAgentPolicy{"base": {Preset: "claude-default", MemoryScope: "conversation_published", ExternalActions: "owner_confirmation", Capabilities: []string{}}, "special": {Preset: preset.Name, ExecutionModel: "group-only-model", MemoryScope: "conversation_published", ExternalActions: "owner_confirmation", Capabilities: []string{}}}, "applications": map[string]any{"group_mention": map[string]any{"enabled": true, "default_agent": "base", "bindings": []map[string]string{{"conversation_id": "watch", "agent": "special"}}}}})
	err = service.mutate(ctx, "global", "test.group.apply", func(tx *core.Tx) (any, error) {
		return tx.CommitAppliedConfig(ctx, 0, 1, []byte(declaration), []core.ManagedConfigObject{{Kind: "application", Name: "group_mention", ObjectType: "runtime", ObjectID: cfg.ID}})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = service.mutate(ctx, "global", "test.group.message", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "1", Origin: "stream", ProviderMessageID: "group-question", ConversationID: "watch", ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "staff_id", IDValue: "peer"}, Body: "answer group", Mentioned: true, SentAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	logger, err := runlog.Open(service.Home, cfg.ID, io.Discard, runlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	service.Logger = logger
	executor := &groupPolicyExecutor{}
	service.Executor = executor
	service.tick(ctx, cfg, preset)
	if executor.calls != 1 || executor.input.Preset.Name != preset.Name || executor.input.ExecutionModel != "group-only-model" || !executor.input.PolicyResolved {
		rows, _ := service.Store.DB.Query("SELECT status,error_code FROM runtime_batches WHERE runtime_id=?", cfg.ID)
		if rows != nil {
			for rows.Next() {
				var status, code string
				rows.Scan(&status, &code)
				t.Log("batch", status, code)
			}
			rows.Close()
		}
		rows, _ = service.Store.DB.Query("SELECT status,error_code FROM runtime_tasks WHERE runtime_id=?", cfg.ID)
		if rows != nil {
			for rows.Next() {
				var status, code string
				rows.Scan(&status, &code)
				t.Log("task", status, code)
			}
			rows.Close()
		}
		t.Fatalf("selected input %+v calls=%d", executor.input, executor.calls)
	}
	if executor.input.MemoryContext != "" || len(executor.input.ConversationContext) != 0 {
		t.Fatal("undeclared context capabilities were exposed")
	}
	if executor.input.WorkspacePath != "" {
		t.Fatalf("group Agent received source workspace path %q", executor.input.WorkspacePath)
	}
	var actualPreset, actualCommit, actualModel string
	err = service.Store.DB.QueryRow("SELECT preset_name,preset_commit,model FROM runtime_attempts WHERE task_id=?", executor.input.Task.ID).Scan(&actualPreset, &actualCommit, &actualModel)
	if err != nil || actualPreset != preset.Name || actualCommit != preset.Commit || actualModel != "group-only-model" {
		t.Fatalf("attempt %s %s %s %v", actualPreset, actualCommit, actualModel, err)
	}
	if adapter.sends != 1 {
		t.Fatalf("group response not sent: %d", adapter.sends)
	}
}

func TestServiceRejectsDifferentHarnessBeforeTaskExecution(t *testing.T) {
	service, cfg, preset, models, _ := setupService(t)
	service.HarnessName = "alternative"
	ctx := context.Background()
	_, err := service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "test.harness.message"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "harness-mismatch", ConversationID: "watch", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "alice"}, Body: "please answer", SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	service.tick(ctx, cfg, preset)
	if models.analyses != 1 || models.executes != 0 {
		t.Fatalf("wrong harness executed task: analysis=%d execution=%d", models.analyses, models.executes)
	}
	tasks, err := core.RuntimeTaskList(ctx, service.Store.DB, cfg.ID, "failed", 10)
	if err != nil || len(tasks) != 1 || tasks[0].ErrorCode != "invalid_input" {
		t.Fatalf("mismatched task was not safely failed: %+v %v", tasks, err)
	}
}
