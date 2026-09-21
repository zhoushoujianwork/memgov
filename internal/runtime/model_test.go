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

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func claudeResult(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"structured_output": json.RawMessage(body), "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}, "total_cost_usd": 0.001})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestClaudeAnalysisUsesHaikuWithoutToolsAndParsesUsage(t *testing.T) {
	var args []string
	c := &Claude{AnalysisModel: "claude-haiku-test", Run: func(_ context.Context, _ string, input []byte, argv ...string) ([]byte, error) {
		args = append([]string{}, argv...)
		if !strings.Contains(string(input), `"messages"`) {
			t.Fatalf("batch was not supplied: %s", input)
		}
		return claudeResult(t, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "context"}}}), nil
	}}
	analysis, usage, err := c.Analyze(context.Background(), core.RuntimeBatch{ID: "batch", Model: "fallback", Messages: []core.RuntimeMessage{{ID: "message", Body: "hello"}}})
	if err != nil || len(analysis.Decisions) != 1 || usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Fatalf("analysis=%+v usage=%+v err=%v", analysis, usage, err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--model claude-haiku-test") || !strings.Contains(joined, "--tools  ") || !strings.Contains(joined, "--strict-mcp-config") {
		t.Fatalf("unsafe analysis argv: %q", joined)
	}
}

func TestClaudeAnalysisRejectsVerifiedGroupMentions(t *testing.T) {
	c := &Claude{Run: func(context.Context, string, []byte, ...string) ([]byte, error) {
		t.Fatal("group mention invoked the work classifier")
		return nil, nil
	}}
	_, _, err := c.Analyze(context.Background(), core.RuntimeBatch{Mode: "group_mention", Messages: []core.RuntimeMessage{{ID: "greeting", Body: "在吗", Addressed: true}}})
	if core.ErrorCode(err) != "denied" {
		t.Fatalf("classification guard=%v", err)
	}
}

func TestAliasProfileParserKeepsOnlyClaudeEnvironment(t *testing.T) {
	alias := `export ANTHROPIC_BASE_URL="https://proxy.example/api" && export ANTHROPIC_AUTH_TOKEN='test-token' && export ANTHROPIC_MODEL=claude-test && echo ready && claude --dangerously-skip-permissions`
	env, err := parseAliasEnvironment(alias)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"ANTHROPIC_BASE_URL=https://proxy.example/api", "ANTHROPIC_AUTH_TOKEN=test-token", "ANTHROPIC_MODEL=claude-test"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in profile environment", want)
		}
	}
	if strings.Contains(joined, "dangerously") || strings.Contains(joined, "echo") {
		t.Fatalf("alias commands leaked into profile environment: %q", joined)
	}
}

func TestAliasProfileParserAcceptsCcswitchSemicolonSyntax(t *testing.T) {
	alias := `export ANTHROPIC_BASE_URL="https://proxy.example/api"; export ANTHROPIC_AUTH_TOKEN='test-token'; export ANTHROPIC_MODEL="claude-sonnet"; exec claude`
	env, err := parseAliasEnvironment(alias)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"ANTHROPIC_BASE_URL=https://proxy.example/api", "ANTHROPIC_AUTH_TOKEN=test-token", "ANTHROPIC_MODEL=claude-sonnet"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in semicolon-separated profile environment: %q", want, joined)
		}
	}
}

func TestReadShellAliasDefinitionDoesNotExecuteZshStartup(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(`alias cc='export ANTHROPIC_BASE_URL="https://proxy.example/api"; export ANTHROPIC_MODEL="claude-sonnet"; exec claude'`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", home)
	raw, ok := readShellAliasDefinition("cc")
	if !ok || !strings.Contains(raw, "ANTHROPIC_BASE_URL") || strings.Contains(raw, "alias cc=") {
		t.Fatalf("alias definition was not read safely: ok=%v raw=%q", ok, raw)
	}
	env, err := parseAliasEnvironment(raw)
	if err != nil || len(env) != 2 {
		t.Fatalf("alias definition did not produce the expected environment: env=%v err=%v", env, err)
	}
}

func TestProfileModelUsesAliasDefaultWithoutModelArgument(t *testing.T) {
	var args []string
	c := &Claude{Profile: "cc", AnalysisModel: "profile", Run: func(_ context.Context, _ string, _ []byte, argv ...string) ([]byte, error) {
		args = append([]string{}, argv...)
		return claudeResult(t, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "context"}}}), nil
	}}
	if _, _, err := c.Analyze(context.Background(), core.RuntimeBatch{Messages: []core.RuntimeMessage{{ID: "message"}}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "--model") {
		t.Fatalf("profile model was overridden: %q", args)
	}
}

func TestGroupAgentLoadsOnlyItsPersistentClaudeHome(t *testing.T) {
	home := t.TempDir()
	preset, err := agent.Enable(context.Background(), home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	agentHome := filepath.Join(t.TempDir(), "group-agent")
	otherHome := filepath.Join(t.TempDir(), "private-agent")
	var args []string
	c := &Claude{Run: func(_ context.Context, _ string, _ []byte, argv ...string) ([]byte, error) {
		args = append([]string{}, argv...)
		return claudeResult(t, map[string]any{"result": "ok", "summary": "", "artifacts": []string{}, "tool_kinds": []string{}, "pending_actions": []any{}}), nil
	}}
	_, err = c.Execute(context.Background(), ExecutionInput{WorkDir: t.TempDir(), AgentHome: agentHome, Preset: preset,
		ApplicationMode: "group_mention", Capabilities: []string{"local_write"}, PolicyResolved: true})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--add-dir "+agentHome) {
		t.Fatalf("group Agent did not load its home: %q", joined)
	}
	if strings.Contains(joined, otherHome) || !strings.Contains(joined, "Edit("+filepath.Join(agentHome, "CLAUDE.md")+")") {
		t.Fatalf("group Agent home boundary was not preserved: %q", joined)
	}
}

func TestGroupAgentExecutionDoesNotInheritOwnerTools(t *testing.T) {
	home := t.TempDir()
	preset, err := agent.Enable(context.Background(), home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	var input string
	var args []string
	c := &Claude{ExecutionModel: "test-model", Run: func(_ context.Context, _ string, raw []byte, argv ...string) ([]byte, error) {
		input, args = string(raw), append([]string{}, argv...)
		return claudeResult(t, map[string]any{"result": "answer", "summary": "answered", "artifacts": []string{}, "tool_kinds": []string{}, "pending_actions": []any{}}), nil
	}}
	channelPrompt := "The same-group conversation is primary; private chats are outside this route."
	_, err = c.Execute(context.Background(), ExecutionInput{Task: core.RuntimeTask{ID: "task"}, WorkDir: t.TempDir(), Preset: preset,
		ApplicationMode: "group_mention", Capabilities: []string{"conversation_history_read", "memory_read"}, ChannelSystemPrompt: channelPrompt})
	if err != nil {
		t.Fatal(err)
	}
	for i, arg := range args {
		if arg == "--allowedTools" && (i+1 >= len(args) || args[i+1] != "") {
			t.Fatalf("group Agent inherited tools: %q", args)
		}
	}
	if !strings.Contains(input, `"capabilities":["conversation_history_read","memory_read"]`) {
		t.Fatalf("fixed capabilities were not supplied: %s", input)
	}
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			values[args[i]] = args[i+1]
		}
	}
	if !strings.Contains(values["--append-system-prompt"], channelPrompt) {
		t.Fatalf("group Agent did not receive its channel system prompt: %q", values["--append-system-prompt"])
	}
}

func TestClaudeBashIsGatedByEffectiveConversationPolicy(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "memgov"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	preset, err := agent.Enable(context.Background(), home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	c := &Claude{Run: func(_ context.Context, _ string, _ []byte, argv ...string) ([]byte, error) {
		args = append([]string{}, argv...)
		return claudeResult(t, map[string]any{"result": "answer", "summary": "answered", "artifacts": []string{}, "tool_kinds": []string{}, "pending_actions": []any{}}), nil
	}}
	for _, mode := range []string{"proactive", "group_mention"} {
		for _, bash := range []bool{false, true} {
			in := ExecutionInput{Task: core.RuntimeTask{ID: "task"}, Home: home, WorkDir: t.TempDir(), Preset: preset,
				ApplicationMode: mode, Capabilities: []string{"local_read", "local_write", "local_test", "artifact_create"},
				BashEnabled: bash, ExternalActions: "owner_confirmation"}
			if _, err := c.Execute(context.Background(), in); err != nil {
				t.Fatalf("mode=%s bash=%v: %v", mode, bash, err)
			}
			values := map[string]string{}
			for i := 0; i+1 < len(args); i++ {
				if strings.HasPrefix(args[i], "--") {
					values[args[i]] = args[i+1]
				}
			}
			if strings.Contains(strings.Join(args, " "), "dangerously-skip-permissions") ||
				(strings.Contains(values["--allowedTools"], "Bash") != bash) ||
				(strings.Contains(values["--tools"], "Bash") != bash) {
				t.Fatalf("mode=%s bash=%v exposed wrong shell tools: %q", mode, bash, args)
			}
			if mode == "group_mention" && strings.Contains(values["--append-system-prompt"], "owner request may execute directly") {
				t.Fatal("group policy inherited owner-private external action authorization")
			}
		}
	}
	if _, err := c.Execute(context.Background(), ExecutionInput{Task: core.RuntimeTask{ID: "task"}, WorkDir: t.TempDir(),
		Preset: preset, ApplicationMode: "proactive", BashEnabled: true, DirectoryBounded: true}); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("unrestricted Bash was silently placed in declared-directory snapshot mode: %v", err)
	}
}

func TestClaudeUsageReportsTheModelThatActuallyRan(t *testing.T) {
	raw := []byte(`{"structured_output":{"decisions":[]},"usage":{"input_tokens":10,"output_tokens":5},"total_cost_usd":0.2,"modelUsage":{"claude-haiku-test":{"costUSD":0.01},"claude-sonnet-test":{"costUSD":0.19}}}`)
	var analysis core.RuntimeAnalysis
	usage, err := decodeClaude(raw, &analysis)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Model != "claude-sonnet-test" {
		t.Fatalf("actual model = %q", usage.Model)
	}
}

func TestConfirmedActionHasSeparateOneActionPrompt(t *testing.T) {
	home := t.TempDir()
	p, err := agent.Enable(context.Background(), home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	var input string
	var args []string
	c := &Claude{ExecutionModel: "exec-model", Run: func(_ context.Context, _ string, raw []byte, argv ...string) ([]byte, error) {
		input, args = string(raw), append([]string{}, argv...)
		return claudeResult(t, map[string]any{"result": "done", "summary": "sent", "artifacts": []string{}, "tool_kinds": []string{"dws"}}), nil
	}}
	result, err := c.ExecuteConfirmedAction(context.Background(), ActionExecutionInput{Task: core.RuntimeTask{ID: "task", Version: 2}, Action: core.RuntimePendingAction{ID: "action", TaskVersion: 2, Kind: "send_message", Target: "user", Payload: "hello"}, MemoryContext: "kubeconfig /Users/mikas/.kube/tencent-bj-prod.conf; context cls-94le9mxr-100019171796-context-default", WorkDir: p.Path, Preset: p})
	if err != nil || result.Result != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(input, `"confirmed_action"`) || !strings.Contains(input, "tencent-bj-prod.conf") || !strings.Contains(strings.Join(args, " "), "--allowedTools Read,Glob,Grep,Bash") || !strings.Contains(strings.Join(args, " "), "explicit kubeconfig") || !strings.Contains(strings.Join(args, " "), "bounded task workspace inputs") {
		t.Fatalf("confirmed action contract missing: input=%s args=%q", input, args)
	}
}

func TestExecutionSchemasAllowOmittedEmptyCollections(t *testing.T) {
	for name, schema := range map[string]string{"execution": executionSchema, "memory": memoryExecutionSchema, "action": actionExecutionSchema} {
		var value struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal([]byte(schema), &value); err != nil {
			t.Fatalf("%s schema is invalid: %v", name, err)
		}
		for _, field := range []string{"artifacts", "tool_kinds", "pending_actions"} {
			if slices.Contains(value.Required, field) {
				t.Fatalf("%s schema still requires optional collection %q: %v", name, field, value.Required)
			}
		}
	}
	var result core.RuntimeAttemptResult
	normalizeAttemptResult(&result)
	if result.Artifacts == nil || result.ToolKinds == nil || result.Actions == nil {
		t.Fatalf("omitted collections were not normalized: %+v", result)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestTaskGitWorkspaceMustBeCleanAndReturnsLocalCommit(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "README.md")
	gitRun(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "base")
	task := core.RuntimeTask{ID: "12345678-1234-1234-1234-123456789abc", Version: 1}
	workdir, branch, base, err := PrepareWorkspace(context.Background(), t.TempDir(), task, "abcdef12-1234", core.Workspace{Path: repo})
	if err != nil || branch == "" || workdir == repo {
		t.Fatalf("workspace=%q branch=%q base=%q err=%v", workdir, branch, base, err)
	}
	if _, err = VerifyWorkspace(context.Background(), workdir, branch, base, true); core.ErrorCode(err) != "conflict" {
		t.Fatalf("unchanged worktree passed commit verification: %v", err)
	}
	if commit, noOpErr := VerifyWorkspace(context.Background(), workdir, branch, base, false); noOpErr != nil || commit != "" {
		t.Fatalf("clean read-only worktree was not accepted: commit=%q err=%v", commit, noOpErr)
	}
	controls := filepath.Join(workdir, ".claude")
	if err = os.MkdirAll(controls, 0700); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(controls, ".memgov-agent-skills.json")
	if err = os.WriteFile(control, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyWorkspace(context.Background(), workdir, branch, base, false); err != nil {
		t.Fatalf("runtime support file forced a no-op commit: %v", err)
	}
	business := filepath.Join(controls, "settings.json")
	if err = os.WriteFile(business, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyWorkspace(context.Background(), workdir, branch, base, false); core.ErrorCode(err) != "conflict" {
		t.Fatalf("untracked business configuration was exempted: %v", err)
	}
	if err = os.Remove(business); err != nil {
		t.Fatal(err)
	}
	gitRun(t, workdir, "add", ".claude/.memgov-agent-skills.json")
	gitRun(t, workdir, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "track existing project control")
	if err = os.WriteFile(control, []byte("[\"changed\"]"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyWorkspace(context.Background(), workdir, branch, base, false); core.ErrorCode(err) != "conflict" {
		t.Fatalf("tracked control-file change escaped commit verification: %v", err)
	}
	gitRun(t, workdir, "restore", ".claude/.memgov-agent-skills.json")
	if err = os.WriteFile(filepath.Join(workdir, "result.txt"), []byte("done\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyWorkspace(context.Background(), workdir, branch, base, false); core.ErrorCode(err) != "conflict" {
		t.Fatalf("dirty worktree passed verification: %v", err)
	}
	gitRun(t, workdir, "add", "result.txt")
	gitRun(t, workdir, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "complete task")
	commit, err := VerifyWorkspace(context.Background(), workdir, branch, base, true)
	if err != nil || commit == "" || commit != gitRun(t, workdir, "rev-parse", "HEAD") {
		t.Fatalf("commit=%q err=%v", commit, err)
	}
}
