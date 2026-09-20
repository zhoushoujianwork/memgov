package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPresetCLISelectsHarnessDefaultAndImportsGenericPolicy(t *testing.T) {
	home := t.TempDir()
	code, value := invoke(t, home, "", "agent", "preset", "enable", "codex")
	if code != 0 || data(t, value)["name"] != "codex-default" || data(t, value)["provider"] != "codex" {
		t.Fatalf("generic preset default: %d %+v", code, value)
	}
	source := filepath.Join(t.TempDir(), "policy.md")
	if err := os.WriteFile(source, []byte("Run relevant project checks.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, value = invoke(t, home, "", "agent", "preset", "sync", "codex-default", "--from-policy", source)
	if code != 0 || data(t, value)["clean"] != true {
		t.Fatalf("generic policy import: %d %+v", code, value)
	}
	code, value = invoke(t, home, "", "agent", "preset", "sync", "codex-default", "--from-policy", source, "--from-claude-md", source)
	if code == 0 {
		t.Fatalf("ambiguous policy import accepted: %+v", value)
	}
	code, value = invoke(t, home, "", "agent", "preset", "enable", "claude")
	if code != 0 || data(t, value)["name"] != "claude-default" {
		t.Fatalf("Claude compatibility default: %d %+v", code, value)
	}
}
