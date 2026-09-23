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

func TestPrepareClaudeSkillsStagesResolvedSkillsAndDetectsChanges(t *testing.T) {
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

func TestDiscoverClaudeUserSkillsSkipsUnavailableInheritedEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude", "skills")
	runtimeTestSkill(t, root, "working-skill")
	if err := os.Symlink(filepath.Join(home, "missing-skill"), filepath.Join(root, "changing-skill")); err != nil {
		t.Fatal(err)
	}
	unsafe := runtimeTestSkill(t, root, "unsafe-skill")
	if err := os.Symlink(filepath.Join(home, "missing-file"), filepath.Join(unsafe, "changing-file")); err != nil {
		t.Fatal(err)
	}

	resolved, err := discoverClaudeUserSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].Name != "working-skill" {
		t.Fatalf("unavailable inherited skills blocked discovery: %+v", resolved)
	}
}

func TestDiscoverClaudeUserSkillsClassifiesUnavailableRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverClaudeUserSkills(); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("unavailable skill root was not classified safely: %v", err)
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

func TestWorkspaceStagesInheritedSkillsWithoutAmbientMemory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude", "skills")
	allowed := runtimeTestSkill(t, root, "clawflow")
	allowed, err := filepath.EvalSymlinks(allowed)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTestSkill(t, root, "memgov-memory")
	runtimeTestSkill(t, root, "memgov-workspace")
	in := ExecutionInput{WorkDir: t.TempDir(), Skills: core.RuntimeSkillPolicy{Inherit: "executor"}}
	stagedRoot := filepath.Join(in.WorkDir, ".claude", "skills")
	runtimeTestSkill(t, stagedRoot, "memgov-memory") // A session directory from the retired runtime.
	if err := prepareClaudeSkills(&in); err != nil {
		t.Fatal(err)
	}
	if len(in.Skills.Resolved) != 1 || in.Skills.Resolved[0].Name != "clawflow" {
		t.Fatalf("retired knowledge skill inherited: %+v", in.Skills.Resolved)
	}
	if target, err := os.Readlink(filepath.Join(stagedRoot, "clawflow")); err != nil || target != allowed {
		t.Fatalf("allowed inheritance lost: target=%q error=%v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(stagedRoot, "memgov-memory")); !os.IsNotExist(err) {
		t.Fatalf("retired skill remains staged: %v", err)
	}
	if directSettingSources(in.Skills) != "project" {
		t.Fatal("provider can still autoload user settings and skills")
	}
	in.Skills = core.RuntimeSkillPolicy{Inherit: "none"}
	if err := prepareClaudeSkills(&in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(stagedRoot, "clawflow")); !os.IsNotExist(err) {
		t.Fatalf("revoked inherited skill remains staged: %v", err)
	}
}

func TestWorkspaceRejectsRenamedKnowledgeSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude", "skills")
	original := runtimeTestSkill(t, t.TempDir(), "memgov-memory")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "old-notes")
	if err := os.Symlink(original, alias); err != nil {
		t.Fatal(err)
	}
	copied := runtimeTestSkill(t, root, "copied-notes")
	if err := os.WriteFile(filepath.Join(copied, "SKILL.md"), []byte("---\nname: memgov-memory\ndescription: retired\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runtimeTestSkill(t, root, "allowed-helper")
	found, err := discoverClaudeUserSkills()
	if err != nil || len(found) != 1 || found[0].Name != "allowed-helper" {
		t.Fatalf("legacy alias discovered: %+v %v", found, err)
	}
	for _, path := range []string{alias, copied} {
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := runtimeSkillDigest(canonical)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(path)
		in := ExecutionInput{WorkDir: t.TempDir(), Skills: core.RuntimeSkillPolicy{Inherit: "none", Paths: []string{path}, Resolved: []core.RuntimeSkill{{Name: name, Path: canonical, Digest: digest}}}}
		if err := prepareClaudeSkills(&in); core.ErrorCode(err) != "conflict" {
			t.Fatalf("explicit legacy alias accepted: %s %v", name, err)
		}
	}
}
