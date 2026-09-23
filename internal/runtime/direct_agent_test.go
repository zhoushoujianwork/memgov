package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func directAgentFixture(t *testing.T) (*Claude, ExecutionInput, string) {
	t.Helper()
	home, workdir := t.TempDir(), t.TempDir()
	preset, err := agent.Enable(context.Background(), home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "memgov"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	starts := filepath.Join(home, "starts")
	t.Setenv("DIRECT_TEST_STARTS", starts)
	t.Setenv("DIRECT_TEST_ARGS", filepath.Join(home, "args"))
	t.Setenv("DIRECT_TEST_PATHS", filepath.Join(home, "paths"))
	t.Setenv("DIRECT_TEST_INPUTS", filepath.Join(home, "inputs"))
	claudeBinary := filepath.Join(home, "fake-claude")
	script := `#!/usr/bin/env python3
import json, os, sys
with open(os.environ["DIRECT_TEST_STARTS"], "a", encoding="utf-8") as starts:
    starts.write("start\n")
with open(os.environ["DIRECT_TEST_ARGS"], "a", encoding="utf-8") as args:
    args.write(json.dumps(sys.argv[1:]) + "\n")
with open(os.environ["DIRECT_TEST_PATHS"], "a", encoding="utf-8") as paths:
    paths.write(os.environ["PATH"] + "\n")
if "--resume" in sys.argv[1:] and os.environ.get("DIRECT_TEST_RESUME_MISSING") == "1":
    print(json.dumps({"type":"result","subtype":"error_during_execution","is_error":True,"result":"No conversation found with session ID"}), flush=True)
    sys.exit(0)
for line in sys.stdin:
    with open(os.environ["DIRECT_TEST_INPUTS"], "a", encoding="utf-8") as inputs:
        inputs.write(line)
    incoming = json.loads(line)
    text = incoming["message"]["content"]
    if isinstance(text, list):
        text = text[-1]["text"]
    print(json.dumps({"type":"result","subtype":"success","result":text,
        "usage":{"input_tokens":7,"output_tokens":3},
        "modelUsage":{"claude-sonnet-live":{"costUSD":0.01}},"total_cost_usd":0.01}), flush=True)
`
	if err := os.WriteFile(claudeBinary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	in := ExecutionInput{SessionID: uuid.NewString(), Home: home, WorkspaceID: "global", WorkDir: workdir,
		Preset: preset, ApplicationMode: "direct", AgentWorkspaceID: "owner/fixture", AttemptID: core.NewID(), Capabilities: []string{"local_read", "local_write"},
		Task: core.RuntimeTask{ID: core.NewID(), Messages: []core.RuntimeMessage{{Body: "你好，原文不变。"}}}}
	return &Claude{Binary: claudeBinary}, in, starts
}

func TestDirectAgentReplaysWhenNativeResumeSessionIsMissing(t *testing.T) {
	c, in, starts := directAgentFixture(t)
	defer c.CloseDirectSessions()
	t.Setenv("DIRECT_TEST_RESUME_MISSING", "1")
	resumeID := uuid.NewString()
	in.NativeSessionID, in.ResumeSessionID = resumeID, resumeID
	in.ConversationContext = []core.RuntimeMessage{{Body: "上一轮问题"}, {Body: "上一轮回答", SelfAuthored: true}}
	in.Task.Messages = []core.RuntimeMessage{{Body: "本轮问题"}}
	result, err := c.Execute(context.Background(), in)
	if err != nil || result.Result != "本轮问题" {
		t.Fatalf("missing native session was not replayed: result=%+v error=%v", result, err)
	}
	if got := directStartCount(t, starts); got != 2 {
		t.Fatalf("resume fallback started %d processes, want 2", got)
	}
	args, err := os.ReadFile(filepath.Join(in.Home, "args"))
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(rows) != 2 {
		t.Fatalf("resume fallback started with unexpected arguments: %q", string(args))
	}
	var firstArgs, secondArgs []string
	if err := json.Unmarshal([]byte(rows[0]), &firstArgs); err != nil {
		t.Fatalf("decode first native arguments: %v", err)
	}
	if err := json.Unmarshal([]byte(rows[1]), &secondArgs); err != nil {
		t.Fatalf("decode second native arguments: %v", err)
	}
	findArg := func(args []string, name string) (string, bool) {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == name {
				return args[i+1], true
			}
		}
		return "", false
	}
	gotResume, ok := findArg(firstArgs, "--resume")
	if !ok || gotResume != resumeID {
		t.Fatalf("first native invocation did not resume %q: %v", resumeID, firstArgs)
	}
	if _, ok := findArg(secondArgs, "--resume"); ok {
		t.Fatalf("fallback native invocation unexpectedly resumed: %v", secondArgs)
	}
	newID, ok := findArg(secondArgs, "--session-id")
	if !ok || newID == "" || newID == resumeID {
		t.Fatalf("fallback native invocation did not use a fresh session: %v", secondArgs)
	}
	inputs, err := os.ReadFile(filepath.Join(in.Home, "inputs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inputs), "上一轮问题") || !strings.Contains(string(inputs), "上一轮回答") {
		t.Fatalf("resume fallback did not replay accepted history: %s", inputs)
	}
}

func TestDirectAgentKeepsProcessAcrossAcceptedTurnsAndResetsOnHistoryChange(t *testing.T) {
	c, in, starts := directAgentFixture(t)
	defer c.CloseDirectSessions()
	ctx := context.Background()
	first, err := c.Execute(ctx, in)
	if err != nil || first.Result != "你好，原文不变。" || first.Usage["model"] != "claude-sonnet-live" {
		t.Fatalf("first raw owner turn failed: result=%+v error=%v", first, err)
	}
	in.Task.Messages = []core.RuntimeMessage{{Body: "那上一条呢？"}}
	in.ConversationContext = []core.RuntimeMessage{{Body: first.Result}, {Body: first.Result, SelfAuthored: true}}
	second, err := c.Execute(ctx, in)
	if err != nil || second.Result != "那上一条呢？" {
		t.Fatalf("second turn failed: result=%+v error=%v", second, err)
	}
	if got := directStartCount(t, starts); got != 1 {
		t.Fatalf("direct conversation spawned %d processes for two accepted turns", got)
	}
	// Missing accepted bot delivery (or an edited/recalled owner turn) cannot
	// stay hidden in native Claude's unpersisted conversation state.
	in.Task.Messages = []core.RuntimeMessage{{Body: "重新确认"}}
	in.ConversationContext = []core.RuntimeMessage{{Body: first.Result}}
	third, err := c.Execute(ctx, in)
	if err != nil || third.Result != "重新确认" || directStartCount(t, starts) != 2 {
		t.Fatalf("stale native history was not discarded: result=%+v starts=%d error=%v", third, directStartCount(t, starts), err)
	}
	c.CloseDirectSession(in.SessionID)
	in.SessionID = uuid.NewString() // /clear uses a new internal session UUID
	in.Task.Messages = []core.RuntimeMessage{{Body: "新会话"}}
	in.ConversationContext = nil
	if _, err := c.Execute(ctx, in); err != nil || directStartCount(t, starts) != 3 {
		t.Fatalf("cleared session did not start fresh: starts=%d error=%v", directStartCount(t, starts), err)
	}
}

func TestOwnerDirectAgentInheritsClaudeGlobalClawflowSkill(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	defer c.CloseDirectSessions()
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	runtimeTestSkill(t, filepath.Join(userHome, ".claude", "skills"), "clawflow")
	runtimeTestSkill(t, filepath.Join(userHome, ".claude", "skills"), "memgov-memory")
	in.Skills = core.RuntimeSkillPolicy{Inherit: "executor"}
	if _, err := c.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(in.Home, "args"))
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err = json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &args); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			values[args[i]] = args[i+1]
		}
	}
	if values["--setting-sources"] != "project" || !strings.Contains(values["--allowedTools"], "Skill(clawflow)") {
		t.Fatalf("owner direct Agent did not inherit clawflow: %+v", values)
	}
	if _, err := os.Stat(filepath.Join(in.WorkDir, ".claude", "skills", "clawflow", "SKILL.md")); err != nil {
		t.Fatalf("allowed inherited skill not staged: %v", err)
	}
	if strings.Contains(values["--allowedTools"], "memgov-memory") {
		t.Fatal("retired memory skill allowed")
	}
	for _, allowed := range strings.Split(values["--allowedTools"], ",") {
		if allowed == "Bash" {
			t.Fatal("loading clawflow widened the Agent to unrestricted Bash")
		}
	}
}

func TestDirectAgentRestartsBeforeTurnWhenCapabilitiesOrPresetNarrow(t *testing.T) {
	c, in, starts := directAgentFixture(t)
	defer c.CloseDirectSessions()
	first, err := c.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.ConversationContext = []core.RuntimeMessage{{Body: in.Task.Messages[0].Body}, {Body: first.Result, SelfAuthored: true}}
	in.Task.Messages = []core.RuntimeMessage{{Body: "权限收缩后的新问题"}}
	in.Capabilities = []string{"local_read"}
	second, err := c.Execute(context.Background(), in)
	if err != nil || second.Result != "权限收缩后的新问题" || directStartCount(t, starts) != 2 {
		t.Fatalf("native process retained widened permissions or lost this turn: result=%+v starts=%d error=%v", second, directStartCount(t, starts), err)
	}
	if _, err := os.Stat(filepath.Join(in.WorkDir, ".claude", "tools", "memgov-workspace")); err != nil {
		t.Fatalf("workspace wrapper disappeared on host permission reduction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(in.WorkDir, ".claude", "tools", "memgov-hotword")); !os.IsNotExist(err) {
		t.Fatalf("revoked hotword wrapper remained available: %v", err)
	}
	args, err := os.ReadFile(filepath.Join(in.Home, "args"))
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(rows) != 2 || !strings.Contains(rows[0], "Skill(memgov-workspace)") || !strings.Contains(rows[1], "Skill(memgov-workspace)") || strings.Contains(rows[1], "Edit,Write") {
		t.Fatalf("new native process retained old Claude tool permissions: %q", rows)
	}
	in.Task.Messages = []core.RuntimeMessage{{Body: "受控preset升级"}}
	in.ConversationContext = append(in.ConversationContext, core.RuntimeMessage{Body: "权限收缩后的新问题"}, core.RuntimeMessage{Body: second.Result, SelfAuthored: true})
	in.Preset.Commit = "new-controlled-commit"
	if _, err := c.Execute(context.Background(), in); err != nil || directStartCount(t, starts) != 3 {
		t.Fatalf("preset commit change did not replace native process: starts=%d error=%v", directStartCount(t, starts), err)
	}
}

func TestDirectAgentBashPolicyUsesRealCLIAndRestartsWhenRevoked(t *testing.T) {
	c, in, starts := directAgentFixture(t)
	defer c.CloseDirectSessions()
	in.BashEnabled, in.ExternalActions = true, "owner_request"
	first, err := c.Execute(context.Background(), in)
	if err != nil || first.Result == "" || directStartCount(t, starts) != 1 {
		t.Fatalf("owner Bash turn failed: result=%+v starts=%d error=%v", first, directStartCount(t, starts), err)
	}
	readRows := func(path string) []string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	argsRows := readRows(filepath.Join(in.Home, "args"))
	paths := readRows(filepath.Join(in.Home, "paths"))
	var firstArgs []string
	if err := json.Unmarshal([]byte(argsRows[0]), &firstArgs); err != nil {
		t.Fatal(err)
	}
	argv := map[string]string{}
	for i := 0; i+1 < len(firstArgs); i++ {
		if strings.HasPrefix(firstArgs[i], "--") {
			argv[firstArgs[i]] = firstArgs[i+1]
		}
	}
	if !strings.Contains(argv["--allowedTools"], ",Bash") || !strings.Contains(argv["--tools"], "Bash") ||
		strings.Contains(strings.Join(firstArgs, " "), "dangerously-skip-permissions") ||
		!strings.Contains(argv["--append-system-prompt"], "do not ask for an additional confirmation token") ||
		strings.Contains(argv["--append-system-prompt"], "pending operation for owner confirmation") {
		t.Fatalf("full Bash owner request was not enabled or retained a confirmation gate: %q", firstArgs)
	}
	if firstPath := strings.Split(paths[0], string(os.PathListSeparator))[0]; firstPath != filepath.Join(in.Home, "bin") {
		t.Fatalf("full Bash PATH did not expose real memgov CLI: %q", firstPath)
	}
	if _, err := os.Stat(filepath.Join(in.WorkDir, ".claude", "tools", "memgov")); !os.IsNotExist(err) {
		t.Fatalf("controlled wrapper shadowed the real CLI: %v", err)
	}
	in.ConversationContext = []core.RuntimeMessage{{Body: in.Task.Messages[0].Body}, {Body: first.Result, SelfAuthored: true}}
	in.Task.Messages = []core.RuntimeMessage{{Body: "权限关闭后继续"}}
	in.BashEnabled, in.ExternalActions = false, "owner_confirmation"
	if _, err := c.Execute(context.Background(), in); err != nil || directStartCount(t, starts) != 2 {
		t.Fatalf("Bash revocation reused the old process: starts=%d error=%v", directStartCount(t, starts), err)
	}
	argsRows = readRows(filepath.Join(in.Home, "args"))
	paths = readRows(filepath.Join(in.Home, "paths"))
	var secondArgs []string
	if err := json.Unmarshal([]byte(argsRows[1]), &secondArgs); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(secondArgs, " "); !strings.Contains(joined, "Bash("+filepath.Join(in.WorkDir, ".claude", "tools", "memgov-workspace")+" *)") ||
		strings.Contains(joined, ",Bash,") || strings.Contains(joined, "do not ask for an additional confirmation token") {
		t.Fatalf("revoked process retained full Bash or owner_request: %q", secondArgs)
	}
	if firstPath := strings.Split(paths[1], string(os.PathListSeparator))[0]; firstPath != filepath.Join(in.WorkDir, ".claude", "tools") {
		t.Fatalf("restricted process PATH did not restore wrapper: %q", firstPath)
	}
	if _, err := os.Stat(filepath.Join(in.WorkDir, ".claude", "tools", "memgov-workspace")); err != nil {
		t.Fatalf("restricted memory wrapper missing after revocation: %v", err)
	}
}

func TestDirectSessionPolicyFingerprintIncludesBashAndExternalActions(t *testing.T) {
	_, in, _ := directAgentFixture(t)
	base := directPolicyDigest(in, "", "")
	in.BashEnabled = true
	withBash := directPolicyDigest(in, "", "")
	in.BashEnabled = false
	in.ExternalActions = "owner_request"
	withExternalPolicy := directPolicyDigest(in, "", "")
	if base == withBash || base == withExternalPolicy || withBash == withExternalPolicy {
		t.Fatal("a shell or external-action policy change would reuse the old native process")
	}
	in.ExternalActions = ""
	in.ChannelSystemPrompt = "new trusted channel behavior"
	if withChannelPrompt := directPolicyDigest(in, "", ""); base == withChannelPrompt {
		t.Fatal("a channel system prompt change would reuse the old native process")
	}
	in.ChannelSystemPrompt = ""
	in.WorkspacePath = "/tmp/project-a"
	withWorkspace := directPolicyDigest(in, "", "")
	in.WorkspacePath = "/tmp/project-b"
	if withWorkspace == directPolicyDigest(in, "", "") {
		t.Fatal("a configured workspace change would reuse the old native process")
	}
}

func TestDirectAgentAppendsTrustedChannelSystemPrompt(t *testing.T) {
	_, in, _ := directAgentFixture(t)
	in.ChannelSystemPrompt = "resolve the named contact and inspect the direct chat before broad memory recall"
	args := directClaudeArgs(in, "preset policy", "", filepath.Join(in.Home, "bin", "memgov"))
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			values[args[i]] = args[i+1]
		}
	}
	prompt := values["--append-system-prompt"]
	if !strings.Contains(prompt, "Channel-specific operating context") || !strings.Contains(prompt, in.ChannelSystemPrompt) {
		t.Fatalf("channel system prompt was not appended to the direct Agent policy: %q", prompt)
	}
}

func TestDirectAgentReceivesConfiguredWorkspacePath(t *testing.T) {
	_, in, _ := directAgentFixture(t)
	in.WorkspacePath = "/Users/mikas/github/memgov"
	args := directClaudeArgs(in, "", "", "")
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			values[args[i]] = args[i+1]
		}
	}
	prompt := values["--append-system-prompt"]
	if !strings.Contains(prompt, "Authorized project workspace: /Users/mikas/github/memgov") || !strings.Contains(prompt, "do not scan the host filesystem") || !strings.Contains(prompt, in.WorkDir) {
		t.Fatalf("workspace boundary was not explicit: %q", prompt)
	}
}

func TestDirectAgentCancelledTurnClosesStream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("direct process-group cleanup is Unix-specific")
	}
	c, in, _ := directAgentFixture(t)
	defer c.CloseDirectSessions()
	binary := filepath.Join(in.Home, "fake-claude")
	childPID := filepath.Join(in.Home, "child.pid")
	t.Setenv("DIRECT_TEST_CHILD_PID", childPID)
	script := "#!/usr/bin/env python3\nimport os, subprocess, sys, time\nchild = subprocess.Popen(['sleep', '60'])\nwith open(os.environ['DIRECT_TEST_CHILD_PID'], 'w', encoding='utf-8') as pid:\n pid.write(str(child.pid))\n pid.flush()\nfor line in sys.stdin:\n time.sleep(30)\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	result := make(chan error, 1)
	go func() {
		_, err := c.Execute(ctx, in)
		result <- err
	}()
	startupDeadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(childPID); err == nil {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("native Claude exited before recording child PID: %v", err)
		default:
		}
		if time.Now().After(startupDeadline) {
			t.Fatal("native Claude did not record child PID before startup deadline")
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	var executeErr error
	select {
	case executeErr = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled native Claude stream did not exit promptly")
	}
	if executeErr == nil || time.Since(start) > 6*time.Second {
		t.Fatalf("cancelled native Claude stream did not exit promptly: error=%v elapsed=%v", executeErr, time.Since(start))
	}
	rawPID, err := os.ReadFile(childPID)
	if err != nil {
		t.Fatalf("cancelled native Claude did not record child PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil || pid <= 1 {
		t.Fatalf("cancelled native Claude recorded invalid child PID %q: %v", rawPID, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(25 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("cancelled native Claude left child process %d alive", pid)
	}
}

func processAlive(pid int) bool {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(output))
	return state != "" && !strings.HasPrefix(state, "Z")
}

func TestDirectAgentTurnTimeoutAllowsThirtyMinutes(t *testing.T) {
	if directTurnTimeout < 30*time.Minute {
		t.Fatalf("direct Agent timeout is too short: %s", directTurnTimeout)
	}
}

func TestDirectModeCannotFallBackToClassifierOrBusinessJSONExecutor(t *testing.T) {
	c := &Claude{Run: func(context.Context, string, []byte, ...string) ([]byte, error) {
		t.Fatal("direct mode invoked classified/business-schema Claude command")
		return nil, nil
	}}
	if _, _, err := c.Analyze(context.Background(), core.RuntimeBatch{Mode: "direct"}); err == nil {
		t.Fatal("direct owner chat was admitted into Haiku intake")
	}
	if _, err := c.Execute(context.Background(), ExecutionInput{ApplicationMode: "direct"}); err == nil {
		t.Fatal("direct owner chat without a session fell back to JSON executor")
	}
}

func TestDirectStreamRecordsOnlySafeKnownToolCategories(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	const credential = "credential-should-never-be-a-tool-category"
	c.Run = func(_ context.Context, _ string, _ []byte, _ ...string) ([]byte, error) {
		return []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Skill","id":"external-stable-id","input":{"token":"` + credential + `"}},{"type":"tool_use","name":"Read","input":{"file":"private-message-body"}},{"type":"tool_use","name":"Unknown-` + credential + `","input":{"raw":"chat-original"}},{"type":"text","text":"private-model-output"}]}}` + "\n" +
			`{"type":"user","message":{"content":[{"type":"tool_result","content":"raw-stderr-` + credential + `"}]}}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Skill","input":{"again":"` + credential + `"}},{"type":"tool_use","name":"Bash","input":{"command":"curl secret"}}]}}` + "\n" +
			`{"type":"result","subtype":"success","result":"已答复"}` + "\n"), nil
	}
	result, err := c.Execute(context.Background(), in)
	if err != nil || result.Result != "已答复" {
		t.Fatalf("direct stream result failed: result=%+v error=%v", result, err)
	}
	if got := strings.Join(result.ToolKinds, ","); got != "skill,read,local_tool" || strings.Contains(got, credential) || strings.Contains(got, "external-stable-id") || strings.Contains(got, "curl") {
		t.Fatalf("tool categories copied raw stream contents or missed dedupe: %q", got)
	}
}

func directStartCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "start\n")
}

func TestDirectAgentStreamInputSkillAndToolPolicy(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	defer c.CloseDirectSessions()
	current := "请解释这段原文：hello？"
	in.Task.Messages = []core.RuntimeMessage{{Body: current}}
	in.ConversationContext = []core.RuntimeMessage{{Body: "之前的问题"}, {Body: "之前的回答", SelfAuthored: true}}
	in.WorkspaceBootstrap.Content = "untrusted workspace bootstrap"
	var seenArgs []string
	var seenInput []byte
	c.Run = func(_ context.Context, _ string, input []byte, args ...string) ([]byte, error) {
		seenInput, seenArgs = append([]byte{}, input...), append([]string{}, args...)
		return []byte(`{"type":"result","subtype":"success","result":"已答复","modelUsage":{"claude-sonnet-test":{"costUSD":0.02}},"usage":{"input_tokens":8,"output_tokens":4},"total_cost_usd":0.02}`), nil
	}
	result, err := c.Execute(context.Background(), in)
	if err != nil || result.Result != "已答复" || result.Usage["model"] != "claude-sonnet-test" {
		t.Fatalf("plain stream result did not decode: result=%+v error=%v", result, err)
	}
	var incoming struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(seenInput, &incoming); err != nil || incoming.Type != "user" || len(incoming.Message.Content) != 2 || incoming.Message.Content[1].Text != current {
		t.Fatalf("owner input was rewritten instead of forwarded raw: %+v error=%v", incoming, err)
	}
	values := map[string]string{}
	for i := 0; i+1 < len(seenArgs); i++ {
		if strings.HasPrefix(seenArgs[i], "--") {
			values[seenArgs[i]] = seenArgs[i+1]
		}
	}
	if values["--input-format"] != "stream-json" || values["--output-format"] != "stream-json" || values["--session-id"] != in.SessionID ||
		values["--setting-sources"] != "project" || values["--permission-mode"] != "dontAsk" || values["--json-schema"] != "" {
		t.Fatalf("direct session retained the business schema or lost streaming controls: %+v", values)
	}
	if strings.Contains(values["--append-system-prompt"], current) || strings.Contains(values["--append-system-prompt"], in.WorkspaceBootstrap.Content) ||
		strings.Contains(values["--append-system-prompt"], "之前的问题") || strings.Contains(values["--append-system-prompt"], "之前的回答") ||
		!strings.Contains(incoming.Message.Content[0].Text, "之前的问题") || !strings.Contains(incoming.Message.Content[0].Text, "之前的回答") {
		t.Fatal("system prompt rewrote the current question, forced recall, or lost recovery history")
	}
	allowed := values["--allowedTools"]
	if !strings.Contains(allowed, "Skill(memgov-workspace)") || !strings.Contains(allowed, "/.claude/tools/memgov-workspace *)") || !strings.Contains(allowed, "memgov-action") ||
		strings.Contains(allowed, "Bash(curl") || strings.Contains(allowed, "Bash(git") || strings.Contains(allowed, "Bash(make") || strings.Contains(allowed, ",Bash,") {
		t.Fatalf("direct tool boundary is too broad or memory skill is inaccessible: %q", allowed)
	}
	for _, rel := range []string{filepath.Join(".claude", "skills", "memgov-workspace", "SKILL.md"), filepath.Join(".claude", "tools", "memgov-workspace"), filepath.Join(".claude", "tools", "memgov-action")} {
		if _, err := os.Stat(filepath.Join(in.WorkDir, rel)); err != nil {
			t.Fatalf("controlled project skill/action tool was not installed: %s error=%v", rel, err)
		}
	}
}

func TestDirectActionWrapperBindsCurrentAttemptWithoutExternalWrite(t *testing.T) {
	_, in, _ := directAgentFixture(t)
	output := filepath.Join(in.Home, "proposals")
	bin := filepath.Join(in.Home, "bin", "memgov")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + output + "'\ncat \"${10}\" >> '" + output + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := installDirectActionTool(in, bin); err != nil {
		t.Fatal(err)
	}
	control, _ := json.Marshal(map[string]string{"task_id": "task-internal", "attempt_id": "attempt-internal", "workspace_id": in.WorkspaceID})
	if err := os.WriteFile(filepath.Join(in.WorkDir, ".memgov-turn.json"), control, 0600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(in.WorkDir, "proposal.json")
	if err := os.WriteFile(input, []byte(`{"kind":"send_message","target":"approved-route","payload":"prepared only"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(in.WorkDir, ".claude", "tools", "memgov-action"), input)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(output)
	if err != nil || !strings.Contains(string(b), "runtime task propose-action task-internal --input") || !strings.Contains(string(b), `"attempt_id": "attempt-internal"`) {
		t.Fatalf("wrapper did not bind a pending action to current task/attempt: output=%q error=%v", b, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"kind":"send_message","target":"other","payload":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(filepath.Join(in.WorkDir, ".claude", "tools", "memgov-action"), outside).Run(); err == nil {
		t.Fatal("action wrapper accepted an input outside the session directory")
	}
}

func TestDirectResultSkipsBackgroundNotifications(t *testing.T) {
	for _, notification := range []string{
		`{"type":"result","subtype":"success","is_error":false,"origin":{"kind":"task-notification"},"num_turns":0,"result":""}`,
		`{"type":"result","subtype":"success","origin":{"kind":"task-notification"},"result":"old background output"}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"origin":{"kind":"task-notification"},"result":"background failed"}`,
	} {
		t.Run(notification, func(t *testing.T) {
			stream := notification + "\n" + `{"type":"result","subtype":"success","result":"current answer"}` + "\n" + `{"type":"result","subtype":"success","result":"next answer"}`
			scanner := bufio.NewScanner(strings.NewReader(stream))
			result, err := readDirectResult(scanner)
			if err != nil || result.Result != "current answer" {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			next, err := readDirectResult(scanner)
			if err != nil || next.Result != "next answer" {
				t.Fatalf("consumed next turn: %+v %v", next, err)
			}
		})
	}
}

func TestDirectResultStillRejectsMissingOrFailedReply(t *testing.T) {
	for _, stream := range []string{
		`{"type":"result","origin":{"kind":"task-notification"},"result":""}`,
		`{"type":"result","subtype":"success","result":""}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"failed"}`,
	} {
		if _, err := readDirectResult(bufio.NewScanner(strings.NewReader(stream))); core.ErrorCode(err) != "unavailable" {
			t.Fatalf("stream=%s error=%v", stream, err)
		}
	}
}
