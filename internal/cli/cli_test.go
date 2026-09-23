package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func invoke(t *testing.T, home, input string, args ...string) (int, map[string]any) {
	t.Helper()
	var out, errOut bytes.Buffer
	args = append([]string{"--home", home}, args...)
	code := Run(context.Background(), args, bytes.NewBufferString(input), &out, &errOut)
	var value map[string]any
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatalf("invalid JSON: %v: %s stderr=%s", err, out.String(), errOut.String())
	}
	return code, value
}
func TestCLIInitializationAndMachineErrors(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	code, _ := invoke(t, home, "", "config", "show")
	if code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("config created home: %v", err)
	}
	code, _ = invoke(t, home, "", "doctor")
	if code != 4 {
		t.Fatal(code)
	}
	code, value := invoke(t, home, "", "init")
	if code != 0 || value["ok"] != true {
		t.Fatalf("init: %d %+v", code, value)
	}
	code, _ = invoke(t, home, "", "workspace", "add", "demo")
	if code != 0 {
		t.Fatal(code)
	}
	code, _ = invoke(t, home, "", "workspace", "add", "demo")
	if code != 3 {
		t.Fatal(code)
	}
	code, _ = invoke(t, home, "", "workspace", "add")
	if code != 2 {
		t.Fatal(code)
	}
}
func TestRetiredKnowledgeCommandsAreAbsent(t *testing.T) {
	for _, command := range []string{"source", "candidate", "memory", "audience", "review", "recall", "ingest"} {
		code, out := invoke(t, t.TempDir(), "", command, "list")
		if code != 2 {
			t.Fatalf("retired command remains: %s %+v", command, out)
		}
	}
}

func TestAgentPresetCommandsCreateCommittedRules(t *testing.T) {
	home := t.TempDir()
	code, value := invoke(t, home, "", "agent", "preset", "enable", "claude", "--name", "claude-default")
	if code != 0 || value["ok"] != true {
		t.Fatalf("enable: %d %+v", code, value)
	}
	code, value = invoke(t, home, "", "agent", "preset", "status", "claude-default")
	if code != 0 || value["data"].(map[string]any)["clean"] != true || value["data"].(map[string]any)["commit"] == "" {
		t.Fatalf("status: %d %+v", code, value)
	}
}

func TestRuntimeHarnessDiagnosticsDoNotRequireDatabase(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	code, value := invoke(t, home, "", "runtime", "harness")
	if code != 0 {
		t.Fatalf("harness diagnostics: %d %+v", code, value)
	}
	data, ok := value["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("unexpected diagnostics payload: %+v", value)
	}
	foundClaude := false
	for _, raw := range data {
		item, ok := raw.(map[string]any)
		if !ok || item["name"] != "claude" {
			continue
		}
		foundClaude = true
		if item["available"] != true || item["analyzer"] != true || item["executor"] != true {
			t.Fatalf("claude contract is not healthy: %+v", item)
		}
	}
	if !foundClaude {
		t.Fatalf("claude harness missing from diagnostics: %+v", data)
	}
	code, value = invoke(t, home, "", "runtime", "harness", "missing-harness")
	if code != 0 {
		t.Fatalf("unknown harness diagnostic should be inspectable: %d %+v", code, value)
	}
	if item, ok := value["data"].(map[string]any); !ok || item["available"] != false || item["error"] == "" {
		t.Fatalf("unknown harness result: %+v", value)
	}
}
