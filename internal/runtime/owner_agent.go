package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// Owner private turns and independent observation tasks share installed skills,
// workspace access and the execution environment, but never their conversation.
func prepareOwnerAgentTools(in *ExecutionInput) (string, error) {
	if err := prepareClaudeSkills(in); err != nil {
		return "", err
	}
	binary, err := directMemgovBinary(in.Home)
	if err != nil {
		return "", err
	}
	if in.BashEnabled {
		_ = os.Remove(filepath.Join(in.WorkDir, ".claude", "tools", "memgov"))
		_ = os.Remove(filepath.Join(in.WorkDir, ".claude", "tools", "memgov-action"))
	}
	if _, err = prepareWorkspaceTool(*in); err != nil {
		return "", err
	}
	in.MemgovBinary = binary
	return binary, nil
}

func ownerAgentEnvironment(in ExecutionInput, profileEnv []string, binary string) []string {
	toolDir := filepath.Join(in.WorkDir, ".claude", "tools")
	if in.BashEnabled {
		toolDir = filepath.Dir(binary)
	}
	path := toolDir + string(os.PathListSeparator) + os.Getenv("PATH")
	return mergeEnvironment(os.Environ(), append(profileEnv,
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "MEMGOV_HOME="+in.Home, "MEMGOV_WORKSPACE="+in.WorkspaceID, "PATH="+path))
}

func prepareOwnerMessageTool(in ExecutionInput, binary string) (string, error) {
	if in.ApplicationMode != "proactive" || in.ExternalActions != "owner_delegated" {
		return "", nil
	}
	if in.Task.ID == "" || in.AttemptID == "" {
		return "", core.Fail("invalid_input", "owner message tool requires a task and attempt")
	}
	tools := filepath.Join(in.WorkDir, ".claude", "tools")
	if err := os.MkdirAll(tools, 0700); err != nil {
		return "", err
	}
	for _, dir := range []string{filepath.Join(in.WorkDir, ".claude"), tools} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", core.Fail("denied", "owner message tool directory is not a regular directory")
		}
	}
	config, _ := json.Marshal(map[string]string{"binary": binary, "home": in.Home, "workspace": in.WorkspaceID, "workdir": in.WorkDir, "task": in.Task.ID, "attempt": in.AttemptID})
	if err := writeDirectTool(tools, "memgov-message", []byte(fmt.Sprintf(ownerMessagePython, config))); err != nil {
		return "", err
	}
	return filepath.Join(tools, "memgov-message"), nil
}

const ownerMessagePython = `#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys, tempfile
CONFIG = %s
workdir = pathlib.Path(CONFIG["workdir"]).resolve()
if len(sys.argv) != 2:
    sys.exit(2)
candidate = pathlib.Path(sys.argv[1])
if not candidate.is_absolute(): candidate = workdir / candidate
candidate = candidate.resolve()
if os.path.commonpath([str(workdir), str(candidate)]) != str(workdir) or not candidate.is_file() or candidate.stat().st_size > 65536:
    sys.exit(2)
with candidate.open("r", encoding="utf-8") as source: action = json.load(source)
if not isinstance(action, dict) or set(action) != {"idempotency_key", "target_type", "target_id", "content", "reason", "evidence_message_ids"}:
    sys.exit(2)
action["attempt_id"] = CONFIG["attempt"]
fd, path = tempfile.mkstemp(prefix=".memgov-message-", suffix=".json", dir=str(workdir))
try:
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as output: json.dump(action, output, ensure_ascii=False)
    completed = subprocess.run([CONFIG["binary"], "--home", CONFIG["home"], "--workspace", CONFIG["workspace"],
        "runtime", "message", "send", CONFIG["task"], "--input", path], cwd=str(workdir),
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=90, check=False)
    sys.stdout.buffer.write((completed.stdout or completed.stderr)[:131072])
    sys.exit(completed.returncode)
finally:
    os.unlink(path)
`
