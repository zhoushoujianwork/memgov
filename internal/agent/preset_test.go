package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestPresetLifecycleIsCommittedAndRefusesDirtyPolicy(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	p, err := Enable(ctx, home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "enabled" || !p.Clean || p.Commit == "" {
		t.Fatalf("enabled preset: %+v", p)
	}
	again, err := Enable(ctx, home, "claude", "claude-default")
	if err != nil || again.Commit != p.Commit {
		t.Fatalf("idempotent enable: %+v %v", again, err)
	}
	if err = os.WriteFile(filepath.Join(p.Path, "policy", "memgov.md"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Disable(ctx, home, p.Name); core.ErrorCode(err) != "conflict" {
		t.Fatalf("dirty preset was disabled: %v", err)
	}
}

func TestPresetSyncCommitsPolicyAndRejectsCredentialLikeInput(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	p, err := Enable(ctx, home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err = os.WriteFile(source, []byte("Run the project checks before completing work.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := SyncClaudeMD(ctx, home, p.Name, source)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Clean || next.Commit == p.Commit {
		t.Fatalf("sync was not committed: %+v", next)
	}
	secret := filepath.Join(t.TempDir(), "secret.md")
	if err = os.WriteFile(secret, []byte("api_key=do-not-import"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = SyncClaudeMD(ctx, home, p.Name, secret); core.ErrorCode(err) != "denied" {
		t.Fatalf("credential-like policy was imported: %v", err)
	}
	disabled, err := Disable(ctx, home, p.Name)
	if err != nil || disabled.Status != "disabled" || !disabled.Clean {
		t.Fatalf("disable: %+v %v", disabled, err)
	}
}
