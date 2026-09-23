package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func ownerRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
func ownerDirectoryPolicy(root string) core.RuntimeAgentPolicy {
	return core.RuntimeAgentPolicy{Directories: []string{root}, Capabilities: []string{"local_read", "local_write", "artifact_create"}}
}
func TestOwnerDeclaredDirectoriesCopyDoesNotModifyOriginal(t *testing.T) {
	ctx := context.Background()
	root := ownerRoot(t)
	home := ownerRoot(t)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("original"), 0600)
	prepared, err := prepareOwnerDirectories(ctx, home, core.RuntimeTask{ID: "task"}, "attempt", core.Workspace{}, ownerDirectoryPolicy(root))
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(prepared.WorkDir, "work", "001", "notes.txt")
	if !pathInRoot(copyPath, prepared.WorkDir) || pathInRoot(copyPath, root) {
		t.Fatal("copy is not isolated")
	}
	if err = os.WriteFile(copyPath, []byte("edited copy"), 0600); err != nil {
		t.Fatal(err)
	}
	products, commit, err := finishOwnerDirectories(ctx, prepared, ownerDirectoryPolicy(root), []string{"work/001/notes.txt"})
	if err != nil || len(products) != 1 || commit != "" {
		t.Fatalf("products %v commit %s error %v", products, commit, err)
	}
	original, _ := os.ReadFile(filepath.Join(root, "notes.txt"))
	if string(original) != "original" {
		t.Fatal("original changed")
	}
	if _, _, err = finishOwnerDirectories(ctx, prepared, ownerDirectoryPolicy(root), []string{filepath.Join(root, "notes.txt")}); core.ErrorCode(err) != "denied" {
		t.Fatalf("source disclosed as product: %v", err)
	}
}
func TestOwnerDirectoryWorkspaceGateRejectsSiblingAndSymlink(t *testing.T) {
	root := ownerRoot(t)
	outside := ownerRoot(t)
	home := ownerRoot(t)
	ctx := context.Background()
	for _, path := range []string{outside, root + "-sibling"} {
		os.MkdirAll(path, 0700)
		_, err := prepareOwnerDirectories(ctx, home, core.RuntimeTask{ID: "task"}, core.NewID(), core.Workspace{Path: path}, ownerDirectoryPolicy(root))
		if core.ErrorCode(err) != "denied" {
			t.Errorf("outside workspace accepted %s: %v", path, err)
		}
	}
	os.Symlink(outside, filepath.Join(root, "escape"))
	if _, err := prepareOwnerDirectories(ctx, home, core.RuntimeTask{ID: "task"}, "symlink", core.Workspace{}, ownerDirectoryPolicy(root)); core.ErrorCode(err) != "denied" {
		t.Fatalf("source symlink accepted: %v", err)
	}
	policy := ownerDirectoryPolicy(root)
	policy.Capabilities = append(policy.Capabilities, "local_test")
	if _, err := prepareOwnerDirectories(ctx, home, core.RuntimeTask{ID: "task"}, "test", core.Workspace{}, policy); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("unsandboxed test admitted: %v", err)
	}
}
func TestOwnerDirectoryCodeUsesPrivateWorktreeAndLocalCommit(t *testing.T) {
	ctx := context.Background()
	root := ownerRoot(t)
	home := ownerRoot(t)
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Source"}, {"config", "user.email", "source@example.test"}} {
		if _, err := isolatedGit(ctx, root, args...); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package demo\n"), 0600)
	if _, err := isolatedGit(ctx, root, "add", "main.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := isolatedGit(ctx, root, "commit", "-m", "baseline"); err != nil {
		t.Fatal(err)
	}
	originalHead, _ := isolatedGit(ctx, root, "rev-parse", "HEAD")
	hookSentinel := filepath.Join(home, "source-hook-ran")
	os.WriteFile(filepath.Join(root, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\ntouch '"+hookSentinel+"'\nexit 1\n"), 0700)
	os.WriteFile(filepath.Join(root, "uncommitted.txt"), []byte("owner pending work"), 0600)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package demo\n\n// owner pending edit\n"), 0600)
	prepared, err := prepareOwnerDirectories(ctx, home, core.RuntimeTask{ID: "task"}, "attempt", core.Workspace{Path: root}, ownerDirectoryPolicy(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(prepared.WorkDir, "uncommitted.txt")); !os.IsNotExist(err) {
		t.Fatal("uncommitted owner file entered task worktree")
	}
	if baseline, _ := os.ReadFile(filepath.Join(prepared.WorkDir, "main.go")); string(baseline) != "package demo\n" {
		t.Fatal("owner uncommitted tracked edit entered isolated worktree")
	}
	if prepared.Branch == "" || prepared.Base != strings.TrimSpace(string(originalHead)) {
		t.Fatalf("worktree %+v", prepared)
	}
	os.WriteFile(filepath.Join(prepared.WorkDir, "main.go"), []byte("package demo\n\nconst Answer = 42\n"), 0600)
	_, commit, err := finishOwnerDirectories(ctx, prepared, ownerDirectoryPolicy(root), []string{"main.go"})
	if err != nil || commit == "" || commit == prepared.Base {
		t.Fatalf("commit %s %v", commit, err)
	}
	parent, err := isolatedGit(ctx, prepared.WorkDir, "rev-parse", "HEAD^")
	if err != nil || strings.TrimSpace(string(parent)) != prepared.Base {
		t.Fatalf("commit ancestry %s %v", parent, err)
	}
	after, _ := isolatedGit(ctx, root, "rev-parse", "HEAD")
	if string(after) != string(originalHead) {
		t.Fatal("source branch changed")
	}
	original, _ := os.ReadFile(filepath.Join(root, "main.go"))
	if string(original) != "package demo\n\n// owner pending edit\n" {
		t.Fatal("source checkout changed")
	}
	if _, err = os.Stat(hookSentinel); !os.IsNotExist(err) {
		t.Fatal("source hook ran from isolated task")
	}
}
func TestOwnerDirectoryCodeRejectsRepositoryWiderThanGrant(t *testing.T) {
	root := ownerRoot(t)
	isolatedGit(context.Background(), root, "init")
	sub := filepath.Join(root, "src")
	os.Mkdir(sub, 0700)
	_, err := prepareOwnerDirectories(context.Background(), ownerRoot(t), core.RuntimeTask{ID: "task"}, "attempt", core.Workspace{Path: sub}, ownerDirectoryPolicy(sub))
	if core.ErrorCode(err) != "denied" {
		t.Fatalf("whole repository exposed from subdir grant: %v", err)
	}
}
func TestOwnerToolRulesConfineEditsAndDoNotExposeShell(t *testing.T) {
	work := ownerRoot(t)
	input := ExecutionInput{WorkDir: work, DirectoryBounded: true, DirectoryWriteRoots: []string{filepath.Join(work, "work"), filepath.Join(work, "artifacts"), filepath.Join(work, "..", "escape")}}
	allowed := strings.Join(ownerDirectoryTools(input), ",")
	if allowed != "Read(./**),Edit(./work/**),Edit(./artifacts/**)" {
		t.Fatalf("unexpected rules %s", allowed)
	}
}

func TestExecutionWorkspacePathHidesIsolatedSourceDirectories(t *testing.T) {
	workspace := core.Workspace{Path: "/owner/project"}
	if got := workspacePathForExecution("direct", workspace, nil); got != workspace.Path {
		t.Fatalf("direct owner lost workspace path: %q", got)
	}
	if got := workspacePathForExecution("group_mention", workspace, nil); got != "" {
		t.Fatalf("group Agent received host workspace path: %q", got)
	}
	if got := workspacePathForExecution("proactive", workspace, &OwnerDirectoryWorkspace{}); got != "" {
		t.Fatalf("directory-bounded Agent received source path: %q", got)
	}
}

func TestOwnerDeclaredToolInvocationRefusesTestsAndProtectsGitControls(t *testing.T) {
	ctx := context.Background()
	preset, err := agent.Enable(ctx, ownerRoot(t), "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	work := ownerRoot(t)
	var args []string
	calls := 0
	model := &Claude{Run: func(_ context.Context, _ string, _ []byte, argv ...string) ([]byte, error) {
		calls++
		args = argv
		return claudeResult(t, core.RuntimeAttemptResult{Result: "done"}), nil
	}}
	input := ExecutionInput{WorkDir: work, Preset: preset, Capabilities: []string{"local_read", "local_write"}, DirectoryBounded: true, DirectoryWriteRoots: []string{work}}
	if _, err = model.Execute(ctx, input); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for i := 0; i < len(args)-1; i++ {
		if strings.HasPrefix(args[i], "--") {
			values[args[i]] = args[i+1]
		}
	}
	if values["--tools"] != "Read,Write,Edit" || values["--allowedTools"] != "Read(./**),Edit(./**)" || values["--setting-sources"] != "" || !strings.Contains(values["--disallowedTools"], "Read(./.git)") {
		t.Fatalf("unsafe tools %+v", values)
	}
	input.Capabilities = append(input.Capabilities, "local_test")
	if _, err = model.Execute(ctx, input); core.ErrorCode(err) != "unavailable" || calls != 1 {
		t.Fatalf("unsupported tests invoked model: calls %d %v", calls, err)
	}
}

type ownerDirectoryExecutor struct {
	execute func(ExecutionInput) (core.RuntimeAttemptResult, error)
}

func (e ownerDirectoryExecutor) Execute(_ context.Context, in ExecutionInput) (core.RuntimeAttemptResult, error) {
	return e.execute(in)
}
func TestOwnerDeclaredServiceRejectsResultAfterConfigurationEpochChanges(t *testing.T) {
	service, cfg, preset, _, adapter := setupService(t)
	ctx := context.Background()
	root := ownerRoot(t)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("input"), 0600)
	policy := ownerDirectoryPolicy(root)
	policy.Preset = preset.Name
	policy.ExternalActions = "owner_confirmation"
	declaration := map[string]any{"agents": map[string]core.RuntimeAgentPolicy{"owner": policy}, "applications": map[string]any{"proactive": map[string]any{"enabled": true, "agent": "owner"}}}
	objects := []core.ManagedConfigObject{{Kind: "application", Name: "proactive", ObjectType: "runtime", ObjectID: cfg.ID}}
	err := service.mutate(ctx, "global", "test.apply", func(tx *core.Tx) (any, error) {
		return tx.CommitAppliedConfig(ctx, 0, 1, []byte(core.JSON(declaration)), objects)
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := core.ReadChannel(ctx, service.Store.DB, cfg.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	err = service.mutate(ctx, "global", "test.message", func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, c.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "directory-request", ConversationID: "watch", Tenant: c.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "owner"}, Body: "edit notes", SentAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		t.Fatal(err)
	}
	executed := false
	service.Executor = ownerDirectoryExecutor{execute: func(in ExecutionInput) (core.RuntimeAttemptResult, error) {
		executed = true
		if !in.DirectoryBounded || len(in.DirectorySnapshots) != 1 || len(in.DirectoryWriteRoots) != 2 {
			t.Fatalf("unbounded input %+v", in)
		}
		os.WriteFile(filepath.Join(in.WorkDir, "work", "001", "notes.txt"), []byte("task output"), 0600)
		declaration["epoch_marker"] = "changed while executing"
		err := service.mutate(ctx, "global", "test.policy.change", func(tx *core.Tx) (any, error) {
			return tx.CommitAppliedConfig(ctx, 1, 1, []byte(core.JSON(declaration)), objects)
		})
		if err != nil {
			t.Fatal(err)
		}
		return core.RuntimeAttemptResult{Result: "obsolete", Artifacts: []string{"work/001/notes.txt"}}, nil
	}}
	service.tick(ctx, cfg, preset)
	if !executed {
		t.Fatal("declared-directory task was not executed")
	}
	if adapter.sends != 0 {
		t.Fatal("obsolete result was delivered")
	}
	body, _ := os.ReadFile(filepath.Join(root, "notes.txt"))
	if string(body) != "input" {
		t.Fatal("owner source was modified")
	}
}

func TestOwnerDeclaredCodeRejectsTrackedSymlinkInsideHiddenDirectory(t *testing.T) {
	ctx := context.Background()
	source := ownerRoot(t)
	outside := ownerRoot(t)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("host secret"), 0600)
	if _, err := isolatedGit(ctx, source, "init"); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(source, ".hidden"), 0700)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(source, ".hidden", "escape"))
	if _, err := isolatedGit(ctx, source, "add", "--all"); err != nil {
		t.Fatal(err)
	}
	if _, err := isolatedGit(ctx, source, "commit", "-m", "tracked link"); err != nil {
		t.Fatal(err)
	}
	_, err := prepareOwnerDirectories(ctx, ownerRoot(t), core.RuntimeTask{ID: "task"}, "attempt", core.Workspace{Path: source}, ownerDirectoryPolicy(source))
	if core.ErrorCode(err) != "denied" {
		t.Fatalf("tracked hidden symlink reached model workspace: %v", err)
	}
}
