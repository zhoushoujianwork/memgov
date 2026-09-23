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

func TestPrepareClaudeSkillsReplacesRepositorySkillLinksWithoutEditingSources(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "repository", true: "external"}[external], func(t *testing.T) {
			workdir := t.TempDir()
			source := filepath.Join(workdir, ".agents", "skills")
			linkTarget := "../.agents/skills"
			if external {
				source = t.TempDir()
				linkTarget = source
			}
			allowed := runtimeTestSkill(t, source, "issue-helper")
			runtimeTestSkill(t, source, "memgov-memory")
			runtimeTestSkill(t, source, "undeclared-helper")
			before, err := runtimeSkillDigest(source)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := runtimeSkillDigest(allowed)
			if err != nil {
				t.Fatal(err)
			}
			claude := filepath.Join(workdir, ".claude")
			if err = os.Mkdir(claude, 0700); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(claude, "skills")
			if err = os.Symlink(linkTarget, root); err != nil {
				t.Fatal(err)
			}
			in := ExecutionInput{WorkDir: workdir, Skills: core.RuntimeSkillPolicy{Inherit: "none", Paths: []string{allowed}, Resolved: []core.RuntimeSkill{{Name: "issue-helper", Path: allowed, Digest: digest}}}}
			if err = prepareClaudeSkills(&in); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("staging root was not replaced with a task-local directory: %v %v", info, err)
			}
			if target, err := os.Readlink(filepath.Join(root, "issue-helper")); err != nil || target != allowed {
				t.Fatalf("resolved skill was not staged: target=%q error=%v", target, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != "issue-helper" {
				t.Fatalf("undeclared project skills were staged: %v %v", entries, err)
			}
			after, err := runtimeSkillDigest(source)
			if err != nil || after != before {
				t.Fatalf("shared skill source was modified: before=%q after=%q error=%v", before, after, err)
			}
			manifest, err := os.ReadFile(filepath.Join(claude, ".memgov-agent-skills.json"))
			if err != nil || string(manifest) != `["issue-helper"]` {
				t.Fatalf("staged manifest is incorrect: %s %v", manifest, err)
			}
		})
	}
}

func TestPrepareClaudeSkillsRemovesUndeclaredTaskSkillsWithoutFollowingLinks(t *testing.T) {
	in := ExecutionInput{WorkDir: t.TempDir(), Skills: core.RuntimeSkillPolicy{Inherit: "none"}}
	root := filepath.Join(in.WorkDir, ".claude", "skills")
	runtimeTestSkill(t, root, "undeclared-helper")
	runtimeTestSkill(t, root, "memgov-memory")
	external := runtimeTestSkill(t, t.TempDir(), "external-helper")
	before, err := runtimeSkillDigest(external)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(external, filepath.Join(root, "linked-helper")); err != nil {
		t.Fatal(err)
	}
	if err = prepareClaudeSkills(&in); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("undeclared task skills remain: %v %v", entries, err)
	}
	after, err := runtimeSkillDigest(external)
	if err != nil || before != after {
		t.Fatalf("external skill was modified: %v", err)
	}
}

func TestPrepareClaudeSkillsRejectsLinkedParentBeforeCreatingChildren(t *testing.T) {
	workdir, external := t.TempDir(), t.TempDir()
	if err := os.Symlink(external, filepath.Join(workdir, ".claude")); err != nil {
		t.Fatal(err)
	}
	in := ExecutionInput{WorkDir: workdir, Skills: core.RuntimeSkillPolicy{Inherit: "none"}}
	if err := prepareClaudeSkills(&in); core.ErrorCode(err) != "denied" {
		t.Fatalf("linked .claude parent was accepted: %v", err)
	}
	entries, err := os.ReadDir(external)
	if err != nil || len(entries) != 0 {
		t.Fatalf("linked parent target was modified: %v %v", entries, err)
	}
}

func TestPrepareClaudeSkillsRejectsUntrustedManifest(t *testing.T) {
	for _, test := range []string{"linked", "traversal", "invalid-json"} {
		t.Run(test, func(t *testing.T) {
			in := ExecutionInput{WorkDir: t.TempDir(), Skills: core.RuntimeSkillPolicy{Inherit: "none"}}
			claude := filepath.Join(in.WorkDir, ".claude")
			retained := runtimeTestSkill(t, filepath.Join(claude, "skills"), "retained")
			manifest := filepath.Join(claude, ".memgov-agent-skills.json")
			outside := filepath.Join(t.TempDir(), "outside.json")
			if err := os.WriteFile(outside, []byte("[]"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch test {
			case "linked":
				err = os.Symlink(outside, manifest)
			case "traversal":
				err = os.WriteFile(manifest, []byte(`["../../outside"]`), 0600)
			case "invalid-json":
				err = os.WriteFile(manifest, []byte(`{"skills":[]}`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = prepareClaudeSkills(&in); core.ErrorCode(err) != "denied" {
				t.Fatalf("untrusted manifest accepted: %v", err)
			}
			if _, err = os.Stat(filepath.Join(retained, "SKILL.md")); err != nil {
				t.Fatalf("staging changed before manifest validation: %v", err)
			}
			body, err := os.ReadFile(outside)
			if err != nil || string(body) != "[]" {
				t.Fatalf("external manifest target was modified: %q %v", body, err)
			}
		})
	}
}

func TestStageResolvedSkillsRejectsPathNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../outside", "/outside", "nested/skill", `nested\skill`} {
		in := ExecutionInput{WorkDir: t.TempDir(), Skills: core.RuntimeSkillPolicy{Resolved: []core.RuntimeSkill{{Name: name, Path: t.TempDir()}}}}
		if err := stageResolvedSkills(in); core.ErrorCode(err) != "denied" {
			t.Errorf("unsafe staged name %q accepted: %v", name, err)
		}
	}
}

func TestWorkspaceRejectsNativeAndGlobalKnowledgeSkills(t *testing.T) {
	for _, name := range []string{"touch-memory", "error-reflection"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := filepath.Join(home, ".claude", "skills")
			original := runtimeTestSkill(t, root, name)
			alias := filepath.Join(root, "renamed-knowledge")
			if err := os.Symlink(original, alias); err != nil {
				t.Fatal(err)
			}
			copied := runtimeTestSkill(t, root, "copied-knowledge")
			if err := os.WriteFile(filepath.Join(copied, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: native knowledge\n---\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runtimeTestSkill(t, root, "allowed-helper")
			found, err := discoverClaudeUserSkills()
			if err != nil || len(found) != 1 || found[0].Name != "allowed-helper" {
				t.Fatalf("global knowledge skill or alias inherited: %+v %v", found, err)
			}
			for _, path := range []string{original, alias, copied} {
				canonical, err := filepath.EvalSymlinks(path)
				if err != nil {
					t.Fatal(err)
				}
				digest, err := runtimeSkillDigest(canonical)
				if err != nil {
					t.Fatal(err)
				}
				in := ExecutionInput{WorkDir: t.TempDir(), Skills: core.RuntimeSkillPolicy{Inherit: "none", Paths: []string{path}, Resolved: []core.RuntimeSkill{{Name: filepath.Base(path), Path: canonical, Digest: digest}}}}
				if err = prepareClaudeSkills(&in); core.ErrorCode(err) != "conflict" {
					t.Fatalf("explicit global knowledge skill accepted: %s %v", path, err)
				}
			}
		})
	}
}
