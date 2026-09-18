package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func runtimeTestSkill(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\ndescription: test skill\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrepareClaudeSkillsStagesOnlyExplicitSkillsAndDetectsChanges(t *testing.T) {
	skillPath := runtimeTestSkill(t, t.TempDir(), "issue-helper")
	digest, err := runtimeSkillDigest(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	in := ExecutionInput{WorkDir: t.TempDir(), Skills: core.RuntimeSkillPolicy{Inherit: "none", Paths: []string{skillPath}, Resolved: []core.RuntimeSkill{{Name: "issue-helper", Path: skillPath, Digest: digest}}}}
	if err = prepareClaudeSkills(&in); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(in.WorkDir, ".claude", "skills", "issue-helper")
	if target, err := os.Readlink(link); err != nil || target != skillPath {
		t.Fatalf("explicit skill was not staged: target=%q error=%v", target, err)
	}
	if err = os.WriteFile(filepath.Join(skillPath, "extra.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = prepareClaudeSkills(&in); core.ErrorCode(err) != "conflict" {
		t.Fatalf("changed skill was accepted: %v", err)
	}
}

func TestDiscoverClaudeUserSkillsFindsClawflow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runtimeTestSkill(t, filepath.Join(home, ".claude", "skills"), "clawflow")
	resolved, err := discoverClaudeUserSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].Name != "clawflow" || resolved[0].Summary != "test skill" {
		t.Fatalf("global Claude skill was not discovered: %+v", resolved)
	}
}

func TestPrepareClaudeSkillsRefreshesInheritedSkillOnNextTurn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := runtimeTestSkill(t, filepath.Join(home, ".claude", "skills"), "clawflow")
	in := ExecutionInput{WorkDir: t.TempDir(), Skills: core.RuntimeSkillPolicy{Inherit: "executor"}}
	if err := prepareClaudeSkills(&in); err != nil {
		t.Fatal(err)
	}
	first := in.Skills.Resolved[0].Digest
	if err := os.WriteFile(filepath.Join(path, "version.txt"), []byte("v2"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudeSkills(&in); err != nil {
		t.Fatalf("inherited skill change should refresh on the next turn: %v", err)
	}
	if in.Skills.Resolved[0].Digest == first {
		t.Fatal("inherited skill digest did not refresh")
	}
}
