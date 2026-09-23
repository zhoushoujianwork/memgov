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
)

// Exercise the supplied binary against real task-bound file tools and a fake
// transport. The test never discovers an installed data home or model account.
func TestInstalledWorkspaceAcceptance(t *testing.T) {
	binary := os.Getenv("MEMGOV_ACCEPTANCE_BINARY")
	if binary == "" {
		t.Skip("run scripts/runtime-offline-acceptance.sh with a built or installed binary")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("acceptance binary must have an explicit absolute path")
	}
	f := runDirectReplyScenario(t, "begin offline workspace acceptance", false, nil, false)
	binDir := filepath.Join(f.service.Home, "bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binary, filepath.Join(binDir, "memgov")); err != nil {
		t.Fatal(err)
	}
	run := func(tool string, success bool, input string, args ...string) map[string]json.RawMessage {
		t.Helper()
		cmd := exec.Command(tool, args...)
		cmd.Env = []string{"HOME=" + f.service.Home, "PATH=/usr/bin:/bin", "LANG=C", "TZ=UTC"}
		if input != "" {
			cmd.Stdin = strings.NewReader(input)
		}
		raw, err := cmd.CombinedOutput()
		if success != (err == nil) {
			t.Fatalf("workspace acceptance %v: expected success=%t, got %v: %s", args, success, err, raw)
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(raw, &envelope) != nil {
			t.Fatalf("workspace CLI returned malformed JSON: %s", raw)
		}
		if string(envelope["ok"]) != map[bool]string{true: "true", false: "false"}[success] {
			t.Fatalf("CLI exit/envelope mismatch: %s", raw)
		}
		return envelope
	}
	var tool, workspaceID string
	f.executor.onExecute = func(in ExecutionInput) {
		workspaceID = in.AgentWorkspaceID
		var err error
		tool, err = prepareWorkspaceTool(in)
		if err != nil {
			t.Fatal(err)
		}
		run(tool, true, "", "list")
		write := `{"path":"notes/acceptance.md","content":"Verified offline knowledge. Source: acceptance task.","expected_digest":""}`
		run(tool, true, write, "write")
		read := run(tool, true, "", "read", "notes/acceptance.md")
		var doc agentworkspace.Document
		if err := json.Unmarshal(read["data"], &doc); err != nil || !strings.Contains(doc.Content, "Verified offline knowledge") || doc.Digest == "" {
			t.Fatal("stored knowledge could not be read back", err)
		}
		// Create-only writes cannot erase the existing file; path escape cannot
		// read the runtime database or another audience's files.
		run(tool, false, write, "write")
		run(tool, false, "", "read", "../state.db")
		run(tool, true, "", "history", "notes/acceptance.md")
		// Stdin supports a maximum-sized topic file without the operating
		// system's argv length limit. HTML escaping expands JSON sixfold; the
		// read response must still be complete and valid.
		large, _ := json.Marshal(map[string]string{"path": "notes/large.md", "content": strings.Repeat("<", agentworkspace.MaxFileBytes), "expected_digest": ""})
		run(tool, true, string(large), "write")
		read = run(tool, true, "", "read", "notes/large.md")
		if err := json.Unmarshal(read["data"], &doc); err != nil || len(doc.Content) != agentworkspace.MaxFileBytes {
			t.Fatal("large knowledge read was truncated or malformed", err)
		}
	}
	ctx := context.Background()
	intakeDirectTest(t, f, "workspace-write", "record a verified note", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	if tool == "" {
		t.Fatal("workspace acceptance never reached the executing Agent")
	}
	// A completed attempt loses both read and write access through its old tool.
	run(tool, false, "", "read", "notes/acceptance.md")
	f.executor.onExecute = nil
	intakeDirectTest(t, f, "workspace-clear", "/clear", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	readAfterClear := false
	f.executor.onExecute = func(in ExecutionInput) {
		if in.AgentWorkspaceID != workspaceID {
			t.Fatal("session clear changed the Owner's knowledge root")
		}
		fresh, err := prepareWorkspaceTool(in)
		if err != nil {
			t.Fatal(err)
		}
		run(fresh, true, "", "read", "notes/acceptance.md")
		readAfterClear = true
	}
	intakeDirectTest(t, f, "workspace-reuse", "reuse the saved note", "owner")
	f.service.tick(ctx, f.cfg, f.preset)
	if !readAfterClear {
		t.Fatal("fresh session did not reuse durable workspace knowledge")
	}
	legacy := exec.Command(binary, "--home", f.service.Home, "recall", "old memory")
	legacy.Env = []string{"HOME=" + f.service.Home, "PATH=/usr/bin:/bin"}
	if output, err := legacy.CombinedOutput(); err == nil {
		t.Fatalf("retired memory CLI remained active: %s", output)
	}
}
