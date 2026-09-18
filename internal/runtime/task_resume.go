package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func continuationPrompt(mode string) string {
	prompt := "继续。上次任务因停止或失败而中断；先核对已经完成的步骤、已有文件和外部操作状态，保留已有成果，在原请求和当前权限内继续。结果未知的发送、发布或其他外部操作不得盲目重复。"
	if mode == "replay" {
		prompt += "旧调用没有可恢复的原生会话，当前提供原请求、已接受的上下文和原工作目录。请检查实际进度，不要声称记得未保存的推理或工具输出。"
	}
	return prompt
}

func persistentClaudeArgs(args []string, sessionID, resumedID string) []string {
	kept := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--no-session-persistence":
			continue
		case "--session-id", "--resume":
			i++
			continue
		}
		kept = append(kept, args[i])
	}
	if resumedID != "" {
		return append(kept, "--resume", resumedID)
	}
	return append(kept, "--session-id", sessionID)
}

func agentSessionState(in ExecutionInput, id string) core.RuntimeAgentSession {
	return core.RuntimeAgentSession{ID: id, PolicyDigest: continuationPolicyDigest(in), ContextDigest: continuationContextDigest(in),
		Branch: in.WorkspaceBranch, Base: in.WorkspaceBase, Workspace: in.WorkspaceState}
}

func continuationPolicyDigest(in ExecutionInput) string {
	return core.Digest(map[string]any{"policy": directPolicyDigest(in, in.ClaudeProfile, in.ExecutionModel),
		"effective_agent_policy": in.AgentPolicyDigest,
		"mode":                   in.ApplicationMode, "channel": in.ChannelID, "conversation": in.ConversationID,
		"bounded": in.DirectoryBounded, "write_roots": in.DirectoryWriteRoots, "directories": in.DirectoryPolicy})
}

func continuationContextDigest(in ExecutionInput) string {
	return core.Digest(map[string]any{"messages": in.Task.Messages, "instructions": in.Task.Instructions,
		"history": in.ConversationContext, "memory": in.MemoryContext, "snapshots": in.DirectorySnapshots})
}

func previousResumeAttempt(task core.RuntimeTask) (core.RuntimeAttempt, error) {
	if task.Resume == nil || task.Resume.TaskVersion != task.Version {
		return core.RuntimeAttempt{}, core.Fail("conflict", "continuation request no longer matches the task")
	}
	for _, a := range task.Attempts {
		if a.ID == task.Resume.FromAttemptID && a.TaskID == task.ID && a.Status == "failed" && a.TaskVersion == task.Version-1 {
			return a, nil
		}
	}
	return core.RuntimeAttempt{}, core.Fail("conflict", "previous continuation attempt is unavailable")
}

func prepareContinuation(in *ExecutionInput) error {
	if in.Task.Resume == nil {
		return nil
	}
	a, err := previousResumeAttempt(in.Task)
	if err != nil {
		return err
	}
	if a.WorkspaceDir != in.WorkDir {
		return core.Fail("conflict", "continuation requires the original working directory")
	}
	if a.AgentSession.PolicyDigest != "" && a.AgentSession.PolicyDigest != continuationPolicyDigest(*in) {
		return core.Fail("conflict", "Agent policy changed; native task context cannot be reused")
	}
	if a.AgentSession.ContextDigest != "" && a.AgentSession.ContextDigest != continuationContextDigest(*in) {
		return core.Fail("conflict", "task context changed; previous Agent context cannot be reused")
	}
	if in.Task.Resume.Mode == "native" {
		if _, err := uuid.Parse(a.AgentSession.ID); err != nil || a.AgentSession.PolicyDigest == "" || a.AgentSession.ContextDigest == "" {
			return core.Fail("conflict", "previous native session is unavailable; retry the task with fresh context")
		}
		in.ResumeSessionID, in.NativeSessionID = a.AgentSession.ID, a.AgentSession.ID
	}
	return nil
}

func (s *Service) sessionRecorder(task core.RuntimeTask, attempt core.RuntimeAttempt) func(context.Context, core.RuntimeAgentSession) error {
	return func(ctx context.Context, session core.RuntimeAgentSession) error {
		return s.mutate(ctx, "global", "runtime.attempt.session", func(tx *core.Tx) (any, error) {
			return nil, tx.RecordRuntimeAgentSession(ctx, task.ID, task.Version, attempt.ID, session)
		})
	}
}

func resumeWorkspace(ctx context.Context, home string, task core.RuntimeTask, workspace core.Workspace) (core.RuntimeAttempt, string, string, error) {
	a, err := previousResumeAttempt(task)
	if err != nil {
		return a, "", "", err
	}
	path, err := filepath.EvalSymlinks(a.WorkspaceDir)
	if err != nil {
		return a, "", "", core.Fail("not_found", "previous task working directory is unavailable")
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return a, "", "", err
	}
	owned := pathInRoot(path, filepath.Join(realHome, "runtime", "tasks", task.ID)) || pathInRoot(path, filepath.Join(realHome, "runtime", "worktrees", task.ID))
	if !owned && workspace.Path != "" {
		configured, e := filepath.EvalSymlinks(workspace.Path)
		owned = e == nil && configured == path
	}
	if !owned {
		return a, "", "", core.Fail("denied", "previous task directory is outside the configured task scope")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return a, "", "", core.Fail("not_found", "previous task working directory is unavailable")
	}
	branch, base := a.AgentSession.Branch, a.AgentSession.Base
	if branch == "" {
		// Legacy attempts did not persist their worktree base; the branch reflog
		// retains its creation commit. Use it without creating a new checkout.
		cmd := exec.CommandContext(ctx, "git", "-C", path, "symbolic-ref", "--short", "HEAD")
		if raw, err := cmd.Output(); err == nil && strings.HasPrefix(strings.TrimSpace(string(raw)), "codex/runtime-") {
			branch = strings.TrimSpace(string(raw))
			cmd = exec.CommandContext(ctx, "git", "-C", path, "reflog", "show", "--format=%H", branch)
			if raw, err := cmd.Output(); err == nil {
				commits := strings.Fields(string(raw))
				if len(commits) > 0 {
					base = commits[len(commits)-1]
				}
			}
		}
	}
	if branch != "" {
		cmd := exec.CommandContext(ctx, "git", "-C", path, "symbolic-ref", "--short", "HEAD")
		raw, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(raw)) != branch || base == "" {
			return a, "", "", core.Fail("conflict", "previous task branch or base changed; cannot continue")
		}
	}
	return a, branch, base, nil
}

func restoredOwnerWorkspace(a core.RuntimeAttempt) (*OwnerDirectoryWorkspace, error) {
	if len(a.AgentSession.Workspace) == 0 {
		return nil, core.Fail("conflict", "previous bounded directory state is unavailable; retry with fresh inputs")
	}
	var prepared OwnerDirectoryWorkspace
	if err := json.Unmarshal(a.AgentSession.Workspace, &prepared); err != nil || prepared.WorkDir != a.WorkspaceDir {
		return nil, core.Fail("conflict", "previous bounded directory state is invalid")
	}
	return &prepared, nil
}
