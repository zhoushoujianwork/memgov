package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEffectiveAgentHomeUsesDistinctDefaultsAndConfiguredPath(t *testing.T) {
	memgovHome := t.TempDir()
	got := effectiveAgentHome(memgovHome, "", "private-agent")
	want := filepath.Join(memgovHome, "agent-homes", "private-agent")
	if got != want {
		t.Fatalf("default Agent home = %q, want %q", got, want)
	}
	if other := effectiveAgentHome(memgovHome, "", "group-agent"); other == got {
		t.Fatalf("different Agents share default home: %q", got)
	}
	configured := filepath.Join(t.TempDir(), "custom-agent")
	if got := effectiveAgentHome(memgovHome, configured, "private-agent"); got != configured {
		t.Fatalf("configured Agent home = %q, want %q", got, configured)
	}
}

func TestPrepareAgentHomeCreatesSecureClaudeNotes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "agent-home")
	if err := prepareAgentHome(home); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("home mode = %o, want 700", info.Mode().Perm())
	}
	claudePath := filepath.Join(home, "CLAUDE.md")
	info, err = os.Stat(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
		t.Fatalf("CLAUDE.md mode/type = %o/%v", info.Mode().Perm(), info.Mode().IsRegular())
	}
	body, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "non-secret") {
		t.Fatalf("default CLAUDE.md does not state the persistence boundary: %q", body)
	}
	if err := os.WriteFile(claudePath, []byte("# durable note\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareAgentHome(home); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("existing CLAUDE.md was not secured: %o", info.Mode().Perm())
	}
}

func TestPrepareAgentHomeRejectsNonRegularClaudeFile(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "CLAUDE.md"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareAgentHome(home); err == nil {
		t.Fatal("directory CLAUDE.md was accepted")
	}
}
