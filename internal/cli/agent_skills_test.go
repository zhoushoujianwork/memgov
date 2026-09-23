package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func writeTestSkill(t *testing.T, parent, name, description string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveClaudeSkillsExplicitOverridesInheritedAndReportsSummary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalRoot := filepath.Join(home, ".claude", "skills")
	writeTestSkill(t, globalRoot, "clawflow", "global version")
	explicit := writeTestSkill(t, filepath.Join(t.TempDir(), "explicit"), "clawflow", "project version")
	explicitResolved, err := filepath.EvalSymlinks(explicit)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveClaudeSkills(core.RuntimeSkillPolicy{Inherit: "executor", Paths: []string{explicit}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].Name != "clawflow" || resolved[0].Path != explicitResolved || resolved[0].Summary != "project version" || resolved[0].Digest == "" {
		t.Fatalf("unexpected resolved skill: %+v", resolved)
	}
}

func TestResolveClaudeSkillsRejectsDuplicateExplicitNamesAndManagedSkill(t *testing.T) {
	one := writeTestSkill(t, filepath.Join(t.TempDir(), "one"), "same", "one")
	two := writeTestSkill(t, filepath.Join(t.TempDir(), "two"), "same", "two")
	if _, err := resolveClaudeSkills(core.RuntimeSkillPolicy{Inherit: "none", Paths: []string{one, two}}); core.ErrorCode(err) != "conflict" {
		t.Fatalf("duplicate explicit skill accepted: %v", err)
	}
	managed := writeTestSkill(t, t.TempDir(), "memgov-memory", "managed")
	if _, err := resolveClaudeSkills(core.RuntimeSkillPolicy{Inherit: "none", Paths: []string{managed}}); core.ErrorCode(err) != "conflict" {
		t.Fatalf("managed skill override accepted: %v", err)
	}
}

func TestInspectSkillAcceptsTopLevelSymlinkAndRejectsNestedSymlink(t *testing.T) {
	real := writeTestSkill(t, t.TempDir(), "real", "linked")
	link := filepath.Join(t.TempDir(), "linked-skill")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	skill, err := inspectSkill(link)
	if err != nil || skill.Name != "linked-skill" || skill.Path == link {
		t.Fatalf("top-level skill symlink was not resolved: %+v %v", skill, err)
	}
	if err = os.Symlink(filepath.Join(real, "SKILL.md"), filepath.Join(real, "reference-link")); err != nil {
		t.Fatal(err)
	}
	if _, err = inspectSkill(link); core.ErrorCode(err) != "denied" {
		t.Fatalf("nested skill symlink was accepted: %v", err)
	}
}

func TestRetiredMemorySkillAliasesCannotBeInheritedOrConfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude", "skills")
	managed := writeTestSkill(t, root, "memgov-memory", "retired")
	alias := filepath.Join(root, "old-notes")
	if err := os.Symlink(managed, alias); err != nil {
		t.Fatal(err)
	}
	copy := writeTestSkill(t, root, "renamed-notes", "retired copy")
	if err := os.WriteFile(filepath.Join(copy, "SKILL.md"), []byte("---\nname: memgov-memory\n---\nOld governance instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveClaudeSkills(core.RuntimeSkillPolicy{Inherit: "executor"})
	if err != nil || len(resolved) != 0 {
		t.Fatal(resolved, err)
	}
	for _, path := range []string{alias, copy} {
		if _, err := resolveClaudeSkills(core.RuntimeSkillPolicy{Inherit: "none", Paths: []string{path}}); core.ErrorCode(err) != "conflict" {
			t.Fatal(path, err)
		}
	}
}

func TestGlobalMemorySkillAliasesAreSkippedWhenInheritedAndRejectedWhenExplicit(t *testing.T) {
	for _, name := range []string{"touch-memory", "error-reflection"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := filepath.Join(home, ".claude", "skills")
			original := writeTestSkill(t, root, name, "global memory writer")
			alias := filepath.Join(root, "renamed-knowledge")
			if err := os.Symlink(original, alias); err != nil {
				t.Fatal(err)
			}
			paths := []string{original, alias}
			for _, format := range []struct{ name, frontmatter string }{
				{"lf", "---\nname: " + name + "\n---\n"},
				{"bom-crlf", "\ufeff---\r\nname: \" " + name + " \"\r\n---\r\n"},
			} {
				copied := writeTestSkill(t, root, "copied-"+format.name, "renamed copy")
				if err := os.WriteFile(filepath.Join(copied, "SKILL.md"), []byte(format.frontmatter+"Native knowledge instructions"), 0600); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, copied)
			}
			writeTestSkill(t, root, "allowed-helper", "useful operational skill")
			resolved, err := resolveClaudeSkills(core.RuntimeSkillPolicy{Inherit: "executor"})
			if err != nil || len(resolved) != 1 || resolved[0].Name != "allowed-helper" {
				t.Fatalf("inherited global-memory skill reached applied policy: %+v %v", resolved, err)
			}
			for _, path := range paths {
				for _, inherit := range []string{"none", "executor"} {
					if _, err := resolveClaudeSkills(core.RuntimeSkillPolicy{Inherit: inherit, Paths: []string{path}}); core.ErrorCode(err) != "conflict" {
						t.Fatalf("explicit incompatible skill accepted during configuration: %s inherit=%s error=%v", path, inherit, err)
					}
				}
			}
		})
	}
}

func TestLoadConfigResolvesSkillPathsFromYAMLDirectoryAndHome(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	a := &app{configPath: filepath.Join(root, "config.local.yaml")}
	if err := a.loadConfig([]byte("agents:\n  helper:\n    skills:\n      inherit: none\n      paths: [./skills/issue-helper, ~/.claude/skills/clawflow, \"~\"]\n")); err != nil {
		t.Fatal(err)
	}
	paths := a.cfg.Agents["helper"].Skills.Paths
	if len(paths) != 3 || paths[0] != filepath.Join(root, "skills", "issue-helper") || paths[1] != filepath.Join(home, ".claude", "skills", "clawflow") || paths[2] != home {
		t.Fatalf("skill paths resolved incorrectly: %v", paths)
	}
}

func TestSkillInheritanceDefaultsOnlyForOwnerPrivateAgent(t *testing.T) {
	cfg := Config{Agents: map[string]AgentDeclaration{
		"owner":  {},
		"helper": {},
	}, Applications: ApplicationDeclarations{OwnerPrivate: &OwnerPrivateApplication{Agent: "owner"}}}
	normalized, err := NormalizeDualModeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Declaration.Agents["owner"].Skills.Inherit != "executor" || normalized.Declaration.Agents["helper"].Skills.Inherit != "none" {
		t.Fatalf("unexpected skill defaults: owner=%+v helper=%+v", normalized.Declaration.Agents["owner"].Skills, normalized.Declaration.Agents["helper"].Skills)
	}
}
