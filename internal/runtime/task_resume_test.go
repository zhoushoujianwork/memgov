package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestDirectContinuationResumesNativeProcessAfterInterruption(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	in.Task.ID, in.Task.Version = core.NewID(), 1
	in.Task.Messages = []core.RuntimeMessage{{Body: "完成当前代码任务"}}
	in.NativeSessionID = core.NewID()
	var saved core.RuntimeAgentSession
	in.RecordSession = func(_ context.Context, session core.RuntimeAgentSession) error { saved = session; return nil }
	binary := filepath.Join(in.Home, "resume-claude")
	script := `#!/usr/bin/env python3
import json, os, sys, time
args = sys.argv[1:]
resumed = '--resume' in args
if resumed:
    session = args[args.index('--resume') + 1]
    with open('native-session', encoding='utf-8') as f:
        assert f.read() == session
    with open('partial.txt', encoding='utf-8') as f:
        assert f.read() == 'already completed step one'
else:
    session = args[args.index('--session-id') + 1]
    assert '--no-session-persistence' not in args
    with open('native-session', 'w', encoding='utf-8') as f:
        f.write(session)
for line in sys.stdin:
    text = json.loads(line)['message']['content']
    if not resumed:
        with open('partial.txt', 'w', encoding='utf-8') as f:
            f.write('already completed step one')
        while True:
            time.sleep(0.1)
    assert '继续' in text
    print(json.dumps({'type':'result','subtype':'success','result':'从已完成的第一步继续，任务完成。'}), flush=True)
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c.Binary = binary
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Execute(ctx, in); done <- err }()
	defer cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		// Existence precedes the Python writer's flush/close. Cancelling in
		// that window leaves an empty file and tests filesystem timing, not
		// continuation. Wait for the durable progress this fixture resumes.
		if b, err := os.ReadFile(filepath.Join(in.WorkDir, "partial.txt")); err == nil && string(b) == "already completed step one" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial process did not write progress")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; core.ErrorCode(err) != "unavailable" {
		t.Fatalf("interrupted process did not stop: %v", err)
	}
	c.CloseDirectSessions()
	if saved.ID != in.NativeSessionID || saved.PolicyDigest == "" || saved.ContextDigest == "" {
		t.Fatal("native session was not saved before interruption")
	}
	previous := core.RuntimeAttempt{ID: core.NewID(), TaskID: in.Task.ID, TaskVersion: 1, Status: "failed", WorkspaceDir: in.WorkDir, AgentSession: saved}
	in.Task.Version = 2
	in.Task.Resume = &core.RuntimeTaskResume{TaskVersion: 2, FromAttemptID: previous.ID, Prompt: "继续", Mode: "native"}
	in.Task.Attempts = []core.RuntimeAttempt{previous}
	in.NativeSessionID = core.NewID()
	restarted := NewClaude(c.AnalysisModel, c.ExecutionModel)
	restarted.Binary = binary
	defer restarted.CloseDirectSessions()
	result, err := restarted.Execute(context.Background(), in)
	if err != nil || result.Result != "从已完成的第一步继续，任务完成。" || saved.ID != previous.AgentSession.ID {
		t.Fatalf("native continuation failed: result=%s error=%v", result.Result, err)
	}
}

func TestDirectLegacyContinuationSuppliesOriginalRequestAndExistingProgress(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	in.Task.ID, in.Task.Version = core.NewID(), 2
	in.Task.Messages = []core.RuntimeMessage{{Body: "原来的需求"}}
	previous := core.RuntimeAttempt{ID: core.NewID(), TaskID: in.Task.ID, TaskVersion: 1, Status: "failed", WorkspaceDir: in.WorkDir}
	in.Task.Attempts = []core.RuntimeAttempt{previous}
	in.Task.Resume = &core.RuntimeTaskResume{TaskVersion: 2, FromAttemptID: previous.ID, Prompt: "继续", Mode: "replay"}
	in.NativeSessionID = core.NewID()
	c.Run = func(_ context.Context, dir string, input []byte, args ...string) ([]byte, error) {
		if dir != previous.WorkspaceDir || !strings.Contains(string(input), "原来的需求") || !strings.Contains(string(input), "继续") || !strings.Contains(string(input), "旧调用没有") {
			t.Fatal("legacy continuation lost request, progress location or missing-history disclosure")
		}
		if strings.Contains(strings.Join(args, " "), "--resume") {
			t.Fatal("legacy continuation attempted to resume an unsaved session")
		}
		return []byte(`{"type":"result","subtype":"success","result":"检查进度后继续完成。"}`), nil
	}
	result, err := c.Execute(context.Background(), in)
	if err != nil || result.Result != "检查进度后继续完成。" {
		t.Fatalf("legacy continuation failed: %v", err)
	}
}

func TestContinuationRefusesChangedPolicyOrContextBeforeModelCall(t *testing.T) {
	for _, change := range []string{"permissions", "history", "directories"} {
		t.Run(change, func(t *testing.T) {
			c, in, _ := directAgentFixture(t)
			in.Task.ID, in.Task.Version = core.NewID(), 1
			in.NativeSessionID = core.NewID()
			previous := core.RuntimeAttempt{ID: core.NewID(), TaskID: in.Task.ID, TaskVersion: 1, Status: "failed", WorkspaceDir: in.WorkDir, AgentSession: agentSessionState(in, in.NativeSessionID)}
			in.Task.Version = 2
			in.Task.Attempts = []core.RuntimeAttempt{previous}
			in.Task.Resume = &core.RuntimeTaskResume{TaskVersion: 2, FromAttemptID: previous.ID, Prompt: "继续", Mode: "native"}
			switch change {
			case "permissions":
				in.BashEnabled = !in.BashEnabled
			case "history":
				in.ConversationContext = []core.RuntimeMessage{{Body: "新增上下文"}}
			case "directories":
				in.DirectoryPolicy = []string{"/changed"}
			}
			c.Run = func(context.Context, string, []byte, ...string) ([]byte, error) {
				t.Fatal("model was called with a changed continuation boundary")
				return nil, nil
			}
			if _, err := c.Execute(context.Background(), in); core.ErrorCode(err) != "conflict" {
				t.Fatalf("changed continuation was not refused: %v", err)
			}
		})
	}
}

func TestWorktreeContinuationReusesPartialChangesAndOriginalBase(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git failed: %v: %s", err, output)
		}
	}
	git("init")
	git("config", "user.email", "test@example.test")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "work.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "work.txt")
	git("commit", "-m", "base")
	task := core.RuntimeTask{ID: core.NewID(), Version: 1}
	dir, branch, base, err := PrepareWorkspace(context.Background(), home, task, core.NewID(), core.Workspace{Path: repo})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("partial progress"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := core.RuntimeAttempt{ID: core.NewID(), TaskID: task.ID, TaskVersion: 1, Status: "failed", WorkspaceDir: dir}
	task.Version, task.Attempts = 2, []core.RuntimeAttempt{previous}
	task.Resume = &core.RuntimeTaskResume{TaskVersion: 2, FromAttemptID: previous.ID, Prompt: "继续", Mode: "replay"}
	a, recoveredBranch, recoveredBase, err := resumeWorkspace(context.Background(), home, task, core.Workspace{Path: repo})
	if err != nil || a.WorkspaceDir != dir || recoveredBranch != branch || recoveredBase != base {
		t.Fatalf("original worktree was not reused: branch=%s base=%s error=%v", recoveredBranch, recoveredBase, err)
	}
	partial, _ := os.ReadFile(filepath.Join(a.WorkspaceDir, "work.txt"))
	if string(partial) != "partial progress" {
		t.Fatal("partial edits were overwritten")
	}
}

// Ensure the callback is omitted from debug/model JSON while durable state is JSON.
func TestContinuationInputKeepsRecorderOutOfJSON(t *testing.T) {
	in := ExecutionInput{RecordSession: func(context.Context, core.RuntimeAgentSession) error { return nil }}
	if _, err := json.Marshal(in); err != nil {
		t.Fatal(err)
	}
}

func TestAssignedTaskContinuationSavesAndResumesSession(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	in.ApplicationMode = "proactive"
	in.Task.ID, in.Task.Version = core.NewID(), 1
	in.NativeSessionID = core.NewID()
	var saved core.RuntimeAgentSession
	in.RecordSession = func(_ context.Context, session core.RuntimeAgentSession) error { saved = session; return nil }
	c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
		if !strings.Contains(strings.Join(args, " "), "--session-id "+in.NativeSessionID) || saved.ID == "" {
			t.Fatal("session identity was not saved before starting the assigned task")
		}
		return nil, core.Fail("unavailable", "interrupted")
	}
	if _, err := c.Execute(context.Background(), in); err == nil {
		t.Fatal("interrupted call returned success")
	}
	previous := core.RuntimeAttempt{ID: core.NewID(), TaskID: in.Task.ID, TaskVersion: 1, Status: "failed", WorkspaceDir: in.WorkDir, AgentSession: saved}
	in.Task.Version, in.Task.Attempts = 2, []core.RuntimeAttempt{previous}
	in.Task.Resume = &core.RuntimeTaskResume{TaskVersion: 2, FromAttemptID: previous.ID, Prompt: "继续", Mode: "native"}
	in.NativeSessionID = core.NewID()
	c.Run = func(_ context.Context, dir string, input []byte, args ...string) ([]byte, error) {
		if dir != previous.WorkspaceDir || !strings.Contains(strings.Join(args, " "), "--resume "+previous.AgentSession.ID) || strings.Contains(strings.Join(args, " "), "--no-session-persistence") {
			t.Fatal("assigned task did not resume the exact saved session in its old directory")
		}
		var payload map[string]any
		if err := json.Unmarshal(input, &payload); err != nil || !strings.Contains(payload["continuation_instruction"].(string), "继续") {
			t.Fatal("assigned task lost the continuation instruction")
		}
		return claudeResult(t, core.RuntimeAttemptResult{Result: "已继续完成。"}), nil
	}
	if result, err := c.Execute(context.Background(), in); err != nil || result.Result != "已继续完成。" {
		t.Fatalf("assigned continuation: result=%s error=%v", result.Result, err)
	}
}
