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
	"github.com/zhoushoujianwork/memgov/internal/agentworkspace"
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
	if !strings.Contains(joined, "--model claude-haiku-test") || !strings.Contains(joined, "--tools  ") || !strings.Contains(joined, "--strict-mcp-config") || !slices.Contains(args, "--disable-slash-commands") {
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
		ApplicationMode: "group_mention", Capabilities: []string{"conversation_history_read"}, ChannelSystemPrompt: channelPrompt})
	if err != nil {
		t.Fatal(err)
	}
	for i, arg := range args {
		if arg == "--allowedTools" && (i+1 >= len(args) || args[i+1] != "") {
			t.Fatalf("group Agent inherited tools: %q", args)
		}
	}
	if !strings.Contains(input, `"capabilities":["conversation_history_read"]`) {
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

func TestClaudeFullExecutionNativeToolsHonorFileCapabilities(t *testing.T) {
	for _, mode := range []string{"direct", "proactive", "group_mention"} {
		for _, tc := range []struct {
			name         string
			capabilities []string
			read, write  bool
		}{
			{"read-write", []string{"local_read", "local_write", "local_test", "artifact_create"}, true, true},
			{"read-only", []string{"local_read"}, true, false},
			{"write-only", []string{"local_write"}, false, true},
			{"artifact-only", []string{"artifact_create"}, false, false},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				c, in, _ := directAgentFixture(t)
				defer c.CloseDirectSessions()
				in.ApplicationMode, in.BashEnabled = mode, true
				in.Capabilities, in.ExternalActions = tc.capabilities, "owner_confirmation"
				c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
					values := claudeArgumentValues(args)
					for _, flag := range []string{"--tools", "--allowedTools"} {
						tools := strings.Split(values[flag], ",")
						for tool, want := range map[string]bool{
							"Read": tc.read, "Glob": tc.read, "Grep": tc.read,
							"Edit": tc.write, "Write": tc.write,
							"Bash": true, "WebSearch": true, "WebFetch": true,
						} {
							if slices.Contains(tools, tool) != want {
								t.Fatalf("%s native tool %s should be %v: %s", flag, tool, want, values[flag])
							}
						}
					}
					if strings.Contains(values["--allowedTools"], "Edit(./artifacts/**)") {
						t.Fatal("full execution retained the restricted group artifact override")
					}
					botMCP := mode == "group_mention" && strings.Contains(values["--mcp-config"], `"memgov_bot"`) && strings.Contains(values["--allowedTools"], "mcp__memgov_bot__forward_bot_message")
					if values["--permission-mode"] != "dontAsk" || slices.Contains(args, "--dangerously-skip-permissions") || mode == "group_mention" && !botMCP || mode != "group_mention" && values["--mcp-config"] != `{"mcpServers":{}}` {
						t.Fatal("native tools changed unrelated permission or MCP policy", args)
					}
					if !strings.Contains(values["--append-system-prompt"], "native WebSearch and WebFetch") {
						t.Fatal("prompt omitted the available native web tools")
					}
					if mode == "direct" {
						return []byte(`{"type":"result","subtype":"success","result":"done"}`), nil
					}
					return claudeResult(t, core.RuntimeAttemptResult{Result: "done"}), nil
				}
				if _, err := c.Execute(context.Background(), in); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestClaudeManagedWorkspaceSkillRemainsEnabledWithoutGeneralExecution(t *testing.T) {
	for _, mode := range []string{"direct", "proactive", "group_mention"} {
		t.Run(mode, func(t *testing.T) {
			c, in, _ := directAgentFixture(t)
			defer c.CloseDirectSessions()
			in.ApplicationMode = mode
			in.Capabilities = []string{"local_read", "local_write", "artifact_create"}
			c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
				values := claudeArgumentValues(args)
				if slices.Contains(args, "--disable-slash-commands") || values["--setting-sources"] != "project" ||
					!slices.Contains(strings.Split(values["--tools"], ","), "Skill") ||
					!slices.Contains(strings.Split(values["--allowedTools"], ","), "Skill(memgov-workspace)") {
					t.Fatal("managed workspace skill is disabled or unfiltered settings are enabled", args)
				}
				for _, flag := range []string{"--tools", "--allowedTools"} {
					tools := strings.Split(values[flag], ",")
					for _, tool := range []string{"WebSearch", "WebFetch"} {
						if slices.Contains(tools, tool) {
							t.Fatalf("restricted runtime gained %s through %s", tool, flag)
						}
					}
					if mode == "group_mention" {
						for _, tool := range []string{"Read", "Glob", "Grep"} {
							if slices.Contains(tools, tool) {
								t.Fatalf("restricted group gained host %s through %s", tool, flag)
							}
						}
					}
				}
				if slices.Contains(strings.Split(values["--allowedTools"], ","), "Bash") {
					t.Fatal("restricted runtime gained general Bash")
				}
				if mode == "direct" {
					return []byte(`{"type":"result","subtype":"success","result":"done"}`), nil
				}
				return claudeResult(t, core.RuntimeAttemptResult{Result: "done"}), nil
			}
			if _, err := c.Execute(context.Background(), in); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClaudeExecutionWithoutSkillsDisablesSlashCommands(t *testing.T) {
	for _, mode := range []string{"direct", "proactive", "group_mention"} {
		t.Run(mode, func(t *testing.T) {
			c, in, _ := directAgentFixture(t)
			defer c.CloseDirectSessions()
			in.ApplicationMode, in.AgentWorkspaceID = mode, ""
			c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
				if !slices.Contains(args, "--disable-slash-commands") {
					t.Fatal("skill-less execution enabled slash commands", args)
				}
				if mode == "direct" {
					return []byte(`{"type":"result","subtype":"success","result":"done"}`), nil
				}
				return claudeResult(t, core.RuntimeAttemptResult{Result: "done"}), nil
			}
			if _, err := c.Execute(context.Background(), in); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClaudeInheritedSkillRemainsEnabledWithoutWorkspace(t *testing.T) {
	for _, mode := range []string{"direct", "proactive", "group_mention"} {
		t.Run(mode, func(t *testing.T) {
			c, in, _ := directAgentFixture(t)
			defer c.CloseDirectSessions()
			in.ApplicationMode, in.AgentWorkspaceID = mode, ""
			userHome := t.TempDir()
			t.Setenv("HOME", userHome)
			runtimeTestSkill(t, filepath.Join(userHome, ".claude", "skills"), "issue-helper")
			in.Skills = core.RuntimeSkillPolicy{Inherit: "executor"}
			c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
				values := claudeArgumentValues(args)
				if slices.Contains(args, "--disable-slash-commands") || values["--setting-sources"] != "project" ||
					!slices.Contains(strings.Split(values["--allowedTools"], ","), "Skill(issue-helper)") {
					t.Fatal("configured skill is disabled without a managed workspace", args)
				}
				if mode == "direct" {
					return []byte(`{"type":"result","subtype":"success","result":"done"}`), nil
				}
				return claudeResult(t, core.RuntimeAttemptResult{Result: "done"}), nil
			}
			if _, err := c.Execute(context.Background(), in); err != nil {
				t.Fatal(err)
			}
		})
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
	result, err := c.ExecuteConfirmedAction(context.Background(), ActionExecutionInput{Task: core.RuntimeTask{ID: "task", Version: 2}, Action: core.RuntimePendingAction{ID: "action", TaskVersion: 2, Kind: "send_message", Target: "user", Payload: "hello"}, WorkspaceBootstrap: agentworkspace.Document{Content: "deployment reference"}, WorkDir: p.Path, Preset: p})
	if err != nil || result.Result != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(input, `"confirmed_action"`) || !strings.Contains(input, "deployment reference") || !strings.Contains(strings.Join(args, " "), "--allowedTools Read,Glob,Grep,Bash") {
		t.Fatalf("confirmed action contract missing: input=%s args=%q", input, args)
	}
}

func TestExecutionSchemasAllowOmittedEmptyCollections(t *testing.T) {
	for name, schema := range map[string]string{"execution": executionSchema, "action": actionExecutionSchema} {
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

func stagedTrackedSkillWorkspace(t *testing.T, regular ...bool) (ExecutionInput, string, string) {
	t.Helper()
	repo, home := t.TempDir(), t.TempDir()
	runtimeTestSkill(t, filepath.Join(repo, ".agents", "skills"), "project-helper")
	if err := os.Mkdir(filepath.Join(repo, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if len(regular) != 0 && regular[0] {
		runtimeTestSkill(t, filepath.Join(repo, ".claude", "skills"), "project-helper")
	} else if err := os.Symlink("../.agents/skills", filepath.Join(repo, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-b", "main")
	gitRun(t, repo, "add", ".agents/skills/project-helper/SKILL.md", ".claude/skills", ".claude/settings.json")
	gitRun(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "base")
	if err := os.Mkdir(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "memgov"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	task := core.RuntimeTask{ID: "12345678-1234-1234-1234-123456789abc", Version: 1}
	attempt := "abcdef12-1234"
	workdir, branch, base, err := PrepareWorkspace(context.Background(), home, task, attempt, core.Workspace{Path: repo})
	if err != nil {
		t.Fatal(err)
	}
	skill := runtimeTestSkill(t, t.TempDir(), "vetted-helper")
	digest, err := runtimeSkillDigest(skill)
	if err != nil {
		t.Fatal(err)
	}
	in := ExecutionInput{Home: home, WorkDir: workdir, Task: task, AttemptID: attempt, AgentWorkspaceID: "owner-test", BashEnabled: true,
		Skills: core.RuntimeSkillPolicy{Inherit: "none", Paths: []string{skill}, Resolved: []core.RuntimeSkill{{Name: "vetted-helper", Path: skill, Digest: digest}}}}
	if _, err := prepareOwnerAgentTools(&in); err != nil {
		t.Fatal(err)
	}
	return in, branch, base
}

func TestTaskGitWorkspaceRestoresRuntimeSkillRootBeforeVerification(t *testing.T) {
	for _, codeTask := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-op", true: "code-commit"}[codeTask], func(t *testing.T) {
			in, branch, base := stagedTrackedSkillWorkspace(t)
			// A reused session must keep provenance while refreshing tools.
			if _, err := prepareOwnerAgentTools(&in); err != nil {
				t.Fatal(err)
			}
			if codeTask {
				if err := os.WriteFile(filepath.Join(in.WorkDir, "result.txt"), []byte("business change"), 0600); err != nil {
					t.Fatal(err)
				}
				gitRun(t, in.WorkDir, "add", "result.txt")
				gitRun(t, in.WorkDir, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "business change")
			}
			commit, err := VerifyWorkspace(context.Background(), in.WorkDir, branch, base, codeTask)
			if err != nil || (commit != "") != codeTask {
				t.Fatalf("runtime-generated skill root blocked task: commit=%q err=%v", commit, err)
			}
			if target, err := os.Readlink(filepath.Join(in.WorkDir, ".claude", "skills")); err != nil || target != "../.agents/skills" {
				t.Fatalf("original skill root was not restored: %q %v", target, err)
			}
			if changes := gitRun(t, in.WorkDir, "diff", base, "HEAD", "--", ".claude"); changes != "" {
				t.Fatalf("runtime controls entered a business commit: %s", changes)
			}
			if _, err := VerifyWorkspace(context.Background(), in.WorkDir, branch, base, codeTask); err != nil {
				t.Fatalf("restoration was not idempotent: %v", err)
			}
		})
	}
}

func TestTaskGitWorkspaceSkillRestorationRejectsProjectAndGeneratedChanges(t *testing.T) {
	for _, change := range []string{"tracked-control", "staged-root", "committed-root", "generated-skill", "unknown-child", "forged-receipt"} {
		t.Run(change, func(t *testing.T) {
			in, branch, base := stagedTrackedSkillWorkspace(t)
			root := filepath.Join(in.WorkDir, ".claude", "skills")
			var err error
			switch change {
			case "tracked-control":
				err = os.WriteFile(filepath.Join(in.WorkDir, ".claude", "settings.json"), []byte(`{"changed":true}`), 0600)
			case "staged-root", "committed-root":
				gitRun(t, in.WorkDir, "add", ".claude/skills")
				if change == "committed-root" {
					gitRun(t, in.WorkDir, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "incorrect runtime changes")
				}
			case "generated-skill":
				err = os.WriteFile(filepath.Join(root, "memgov-workspace", "SKILL.md"), []byte("changed"), 0600)
			case "unknown-child":
				err = os.WriteFile(filepath.Join(root, "business.txt"), []byte("keep this business file"), 0600)
			case "forged-receipt":
				err = os.WriteFile(filepath.Join(in.WorkDir, ".claude", skillRootReceiptName), []byte(`{"original_target":"/outside","staged_targets":{}}`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = VerifyWorkspace(context.Background(), in.WorkDir, branch, base, false); core.ErrorCode(err) != "conflict" {
				t.Fatalf("changed project/runtime control accepted: %v", err)
			}
			if change == "unknown-child" {
				body, err := os.ReadFile(filepath.Join(root, "business.txt"))
				if err != nil || string(body) != "keep this business file" {
					t.Fatalf("unknown business content was discarded: %q %v", body, err)
				}
			}
		})
	}
}

func TestTaskGitWorkspacePreservesRegularProjectSkillDirectory(t *testing.T) {
	for _, codeTask := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-op", true: "code-commit"}[codeTask], func(t *testing.T) {
			in, branch, base := stagedTrackedSkillWorkspace(t, true)
			if _, err := os.Lstat(filepath.Join(in.WorkDir, ".claude", "skills", "project-helper")); !os.IsNotExist(err) {
				t.Fatalf("undeclared original project skill can still autoload: %v", err)
			}
			if _, err := prepareOwnerAgentTools(&in); err != nil {
				t.Fatalf("reused initialization failed: %v", err)
			}
			if codeTask {
				if err := os.WriteFile(filepath.Join(in.WorkDir, "result.txt"), []byte("business change"), 0600); err != nil {
					t.Fatal(err)
				}
				gitRun(t, in.WorkDir, "add", "result.txt")
				gitRun(t, in.WorkDir, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "business change")
			}
			commit, err := VerifyWorkspace(context.Background(), in.WorkDir, branch, base, codeTask)
			if err != nil || (commit != "") != codeTask {
				t.Fatalf("regular project skills blocked task: commit=%q err=%v", commit, err)
			}
			body, err := os.ReadFile(filepath.Join(in.WorkDir, ".claude", "skills", "project-helper", "SKILL.md"))
			if err != nil || string(body) != "---\ndescription: test skill\n---\n" {
				t.Fatalf("original project skill content changed: %q %v", body, err)
			}
			if changes := gitRun(t, in.WorkDir, "diff", base, "HEAD", "--", ".claude"); changes != "" {
				t.Fatalf("generated skill staging entered a commit: %s", changes)
			}
			if _, err := os.Lstat(filepath.Join(in.WorkDir, ".claude", originalSkillDirectory)); !os.IsNotExist(err) {
				t.Fatalf("original skill backup was not restored: %v", err)
			}
			if _, err := VerifyWorkspace(context.Background(), in.WorkDir, branch, base, codeTask); err != nil {
				t.Fatalf("regular skill restoration is not idempotent: %v", err)
			}
		})
	}
}

func TestTaskGitWorkspaceRejectsChangedPreservedSkillDirectory(t *testing.T) {
	in, branch, base := stagedTrackedSkillWorkspace(t, true)
	original := filepath.Join(in.WorkDir, ".claude", originalSkillDirectory, "project-helper", "SKILL.md")
	if err := os.WriteFile(original, []byte("changed original"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyWorkspace(context.Background(), in.WorkDir, branch, base, false); core.ErrorCode(err) != "conflict" {
		t.Fatalf("changed preserved project skill was accepted: %v", err)
	}
	if body, err := os.ReadFile(original); err != nil || string(body) != "changed original" {
		t.Fatalf("changed original content was discarded: %q %v", body, err)
	}
}

func TestTaskGitWorkspaceRecoversInterruptedSkillInitializationAndRestoration(t *testing.T) {
	for _, regular := range []bool{false, true} {
		for _, phase := range []string{"before-replace", "before-restore", "after-restore"} {
			t.Run(map[bool]string{false: "symlink", true: "directory"}[regular]+"/"+phase, func(t *testing.T) {
				in, branch, base := stagedTrackedSkillWorkspace(t, regular)
				claude := filepath.Join(in.WorkDir, ".claude")
				if err := os.RemoveAll(filepath.Join(claude, "skills")); err != nil {
					t.Fatal(err)
				}
				if phase != "before-restore" {
					var err error
					if regular {
						err = os.Rename(filepath.Join(claude, originalSkillDirectory), filepath.Join(claude, "skills"))
					} else {
						err = os.Symlink("../.agents/skills", filepath.Join(claude, "skills"))
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if phase == "before-replace" {
					if _, err := prepareOwnerAgentTools(&in); err != nil {
						t.Fatalf("interrupted initialization was not recoverable: %v", err)
					}
				}
				if _, err := VerifyWorkspace(context.Background(), in.WorkDir, branch, base, false); err != nil {
					t.Fatalf("interrupted staging/restoration did not recover: %v", err)
				}
			})
		}
	}
}
