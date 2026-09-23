package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/agentworkspace"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/sysprompt"
)

func TestOwnerWorkspaceSharedByPrivateAndBackgroundAndExcludesLegacyHome(t *testing.T) {
	f := runDirectReplyScenario(t, "workspace bootstrap", false, nil, false)
	s := f.service
	legacy := filepath.Join(s.Home, "agent-homes", "owner", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("LEGACY_PRIVATE_SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	private := f.cfg
	first := f.executor.inputs[0]
	if err := s.bindAgentWorkspace(ctx, private, &first); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.WorkspaceBootstrap.Content, "LEGACY_PRIVATE_SENTINEL") || first.AgentWorkspaceID == "" {
		t.Fatal("new knowledge root inherited legacy notes")
	}
	updated, err := agentworkspace.Write(s.Home, first.AgentWorkspaceID, "MEMORY.md", "# Index\n\nShared owner preference.\n", first.WorkspaceBootstrap.Digest, "test", "update-index")
	if err != nil {
		t.Fatal(err)
	}
	background, err := core.ReadRuntime(ctx, s.Store.DB, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	next := ExecutionInput{Task: core.RuntimeTask{ID: "background-task", RuntimeID: background.ID, RouteID: background.RouteIDs[0]}, AttemptID: "background-attempt"}
	if err := s.bindAgentWorkspace(ctx, background, &next); err != nil {
		t.Fatal(err)
	}
	if next.AgentWorkspaceID != first.AgentWorkspaceID || next.WorkspaceBootstrap.Digest != updated.Digest || !strings.Contains(next.WorkspaceBootstrap.Content, "Shared owner preference") {
		t.Fatal("private and background tasks did not share durable owner knowledge")
	}
	if strings.Join(next.WorkspaceCommand, " ") == strings.Join(first.WorkspaceCommand, " ") || !strings.Contains(strings.Join(next.WorkspaceCommand, " "), "--attempt background-attempt") {
		t.Fatal("harness command did not bind the current task attempt")
	}
	if _, err := s.Store.DB.Exec("UPDATE identity_aliases SET verified=0 WHERE principal_id=?", private.OwnerPrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.agentWorkspace(ctx, private, first.Task); core.ErrorCode(err) != "denied" {
		t.Fatal("revoked Owner identity still received bootstrap knowledge", err)
	}
}

func TestGroupWorkspaceUsesExactConversationRegardlessOfAgent(t *testing.T) {
	s, cfg, _, _, _ := setupService(t)
	ctx := context.Background()
	cfg.ApplicationMode, cfg.ExternalActions = "group_mention", "owner_confirmation"
	if _, err := s.Store.DB.Exec("UPDATE runtime_configs SET application_mode='group_mention',external_actions='owner_confirmation' WHERE id=?", cfg.ID); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.agentWorkspace(ctx, cfg, core.RuntimeTask{RuntimeID: cfg.ID, RouteID: cfg.RouteIDs[0]})
	if err != nil {
		t.Fatal(err)
	}
	var other core.Route
	_, err = s.Store.Mutate(ctx, core.Request{Scope: "global", Command: "test.group"}, func(tx *core.Tx) (any, error) {
		var e error
		other, e = tx.AddRoute(ctx, cfg.ChannelID, core.RouteInput{ConversationID: "another-group", ConversationType: "group"})
		return other, e
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.agentWorkspace(ctx, cfg, core.RuntimeTask{RuntimeID: cfg.ID, RouteID: other.ID}); core.ErrorCode(err) != "denied" {
		t.Fatal("unadmitted group received a workspace", err)
	}
	cfg.RouteIDs = append(cfg.RouteIDs, other.ID)
	if _, err := s.Store.DB.Exec("UPDATE runtime_configs SET route_ids=? WHERE id=?", core.JSON(cfg.RouteIDs), cfg.ID); err != nil {
		t.Fatal(err)
	}
	second, _, err := s.agentWorkspace(ctx, cfg, core.RuntimeTask{RuntimeID: cfg.ID, RouteID: other.ID})
	if err != nil || first.ID == second.ID {
		t.Fatal("two groups shared their workspace", err)
	}
	cfg.AgentPreset = "another-agent"
	again, _, err := s.agentWorkspace(ctx, cfg, core.RuntimeTask{RuntimeID: cfg.ID, RouteID: cfg.RouteIDs[0]})
	if err != nil || again.ID != first.ID {
		t.Fatal("changing the Agent binding discarded group knowledge", err)
	}
	owner, err := agentworkspace.OwnerRef(cfg.OwnerPrincipalID)
	if err != nil || owner.ID == first.ID || owner.ID == second.ID {
		t.Fatal("group inherited its owner's private workspace", err)
	}
}

func TestWorkspaceWrapperBindsAttemptAndAcceptsOnlyFileOperations(t *testing.T) {
	_, in, _ := directAgentFixture(t)
	log := filepath.Join(in.Home, "workspace-calls")
	script := "#!/usr/bin/env python3\nimport json,sys\nwith open(" + pythonString(log) + ", 'a') as f: f.write(json.dumps({'argv':sys.argv[1:],'input':sys.stdin.read() if 'write' in sys.argv else ''})+'\\n')\nprint('{}')\n"
	if err := os.WriteFile(filepath.Join(in.Home, "bin", "memgov"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	tool, err := prepareWorkspaceTool(in)
	if err != nil {
		t.Fatal(err)
	}
	write := `{"path":"notes/result.md","content":"verified result","expected_digest":""}`
	for _, args := range [][]string{{"list"}, {"read", "MEMORY.md"}, {"search", "literal; $(no-command)"}, {"history", "notes/result.md"}, {"write", write}} {
		if output, err := exec.Command(tool, args...).CombinedOutput(); err != nil {
			t.Fatalf("operation %v failed: %s %v", args, output, err)
		}
	}
	stdinWrite := exec.Command(tool, "write")
	stdinWrite.Stdin = strings.NewReader(write)
	if output, err := stdinWrite.CombinedOutput(); err != nil {
		t.Fatalf("stdin write failed: %s %v", output, err)
	}
	for _, args := range [][]string{{"list", "--workspace-id=other"}, {"read", "--home=other"}, {"write", `{"path":"x","content":"x","expected_digest":"","workspace_id":"other"}`}, {"memory", "show"}, {"read", "MEMORY.md", "--task=other"}} {
		if output, err := exec.Command(tool, args...).CombinedOutput(); err == nil {
			t.Fatalf("wrapper admitted %v: %s", args, output)
		}
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 6 {
		t.Fatalf("denied commands reached the CLI: %s", raw)
	}
	for _, line := range lines {
		var call struct {
			Argv  []string `json:"argv"`
			Input string   `json:"input"`
		}
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(call.Argv, " ")
		if !strings.Contains(joined, "--home "+in.Home+" agent workspace") || !strings.Contains(joined, "--task "+in.Task.ID+" --attempt "+in.AttemptID) {
			t.Fatal("wrapper scope could be replaced", joined)
		}
		if strings.Contains(joined, "workspace write") && !strings.Contains(call.Input, "verified result") {
			t.Fatal("write JSON was not sent on stdin")
		}
	}
}

func pythonString(value string) string { raw, _ := json.Marshal(value); return string(raw) }

func TestGroupKnowledgeWritesDoNotGrantHostFileOrShellTools(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	in.ApplicationMode, in.Capabilities = "group_mention", nil
	in.WorkspaceBootstrap.Content = "UNTRUSTED_INDEX_SENTINEL"
	c.Run = func(_ context.Context, _ string, raw []byte, args ...string) ([]byte, error) {
		values := map[string]string{}
		for i := 0; i+1 < len(args); i++ {
			if strings.HasPrefix(args[i], "--") {
				values[args[i]] = args[i+1]
			}
		}
		if values["--tools"] != "Skill,Bash" || strings.Contains(values["--allowedTools"], ",Bash,") || values["--add-dir"] != "" {
			t.Fatal("workspace widened group host capabilities", values)
		}
		if !strings.Contains(values["--allowedTools"], "memgov-workspace *)") || !strings.Contains(string(raw), "UNTRUSTED_INDEX_SENTINEL") || strings.Contains(values["--append-system-prompt"], "UNTRUSTED_INDEX_SENTINEL") {
			t.Fatal("knowledge tool missing or workspace data promoted into system instructions")
		}
		return claudeResult(t, map[string]any{"result": "done", "summary": "done"}), nil
	}
	if _, err := c.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceIndexChangeRestartsNativeSessionWithFreshData(t *testing.T) {
	c, in, starts := directAgentFixture(t)
	defer c.CloseDirectSessions()
	in.WorkspaceBootstrap.Content, in.WorkspaceBootstrap.Digest = "old workspace index", "old-digest"
	first, err := c.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.ConversationContext = []core.RuntimeMessage{{Body: in.Task.Messages[0].Body}, {Body: first.Result, SelfAuthored: true}}
	in.Task.Messages = []core.RuntimeMessage{{Body: "next request"}}
	in.WorkspaceBootstrap.Content, in.WorkspaceBootstrap.Digest = "updated workspace index", "new-digest"
	if _, err = c.Execute(context.Background(), in); err != nil || directStartCount(t, starts) != 2 {
		t.Fatal("workspace change reused stale native context", err)
	}
	raw, err := os.ReadFile(filepath.Join(in.Home, "inputs"))
	if err != nil || !strings.Contains(string(raw), "updated workspace index") {
		t.Fatal("fresh workspace data missing", err)
	}
}

func TestIntakeDoesNotProduceKnowledgeExtractionTasks(t *testing.T) {
	if strings.Contains(analysisSchema, `"memory"`) || strings.Contains(executionSchema, `"candidate"`) || strings.Contains(sysprompt.Text("analysis"), "Classify durable reusable knowledge") {
		t.Fatal("retired memory extraction pipeline remains available")
	}
}

func TestWorkspaceKnowledgeChangeDoesNotChangeContinuationAuthority(t *testing.T) {
	_, in, _ := directAgentFixture(t)
	in.WorkspaceBootstrap.Content, in.WorkspaceBootstrap.Digest = "old index", "old-digest"
	policy, contextDigest := continuationPolicyDigest(in), continuationContextDigest(in)
	in.WorkspaceBootstrap.Content, in.WorkspaceBootstrap.Digest = "new index", "new-digest"
	if continuationPolicyDigest(in) != policy || continuationContextDigest(in) != contextDigest {
		t.Fatal("a knowledge update was treated as a task or authorization change")
	}
	in.AgentWorkspaceID = "owner/different"
	if continuationPolicyDigest(in) == policy {
		t.Fatal("workspace identity changes were omitted from continuation authority")
	}
}

func TestWorkspaceDisablesNativeKnowledgeForOwnerProcesses(t *testing.T) {
	_, in, _ := directAgentFixture(t)
	for _, bash := range []bool{false, true} {
		in.BashEnabled = bash
		env := ownerAgentEnvironment(in, []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY=0", "CLAUDE_CODE_DISABLE_CLAUDE_MDS=0"}, "/runtime/bin/memgov")
		for _, key := range []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY", "CLAUDE_CODE_DISABLE_CLAUDE_MDS"} {
			count := 0
			for _, value := range env {
				if strings.HasPrefix(value, key+"=") {
					count++
					if value != key+"=1" {
						t.Fatalf("profile reenabled native knowledge: %s", value)
					}
				}
			}
			if count != 1 {
				t.Fatalf("native knowledge disable missing or duplicated: %s", key)
			}
		}
	}
}
