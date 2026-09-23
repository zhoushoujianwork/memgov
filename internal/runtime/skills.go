package runtime

import (
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"gopkg.in/yaml.v3"
)

func runtimeSkillDigest(path string) (string, error) {
	parts := []string{}
	err := filepath.WalkDir(path, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return core.Fail("denied", "configured skill contains a symbolic link")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return core.Fail("denied", "configured skill contains a non-regular file")
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(path, name)
		parts = append(parts, filepath.ToSlash(rel)+"\x00"+info.Mode().String()+"\x00"+core.Hash(body))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(parts)
	return core.Digest(parts), nil
}

func runtimeSkillSummary(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "name:") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "description:"))
		return strings.Trim(line, "\"'")
	}
	return ""
}

// Check stable skill identities even when a directory or symlink is renamed.
func managedKnowledgeSkill(name, path string, body []byte) bool {
	managed := func(value string) bool { return value == "memgov-memory" || value == "memgov-workspace" }
	if managed(name) || managed(filepath.Base(filepath.Clean(path))) {
		return true
	}
	if canonical, err := filepath.EvalSymlinks(path); err == nil && managed(filepath.Base(canonical)) {
		return true
	}
	lines := strings.Split(strings.TrimPrefix(string(body), "\ufeff"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "---" {
			continue
		}
		var metadata struct {
			Name string `yaml:"name"`
		}
		if yaml.Unmarshal([]byte(strings.Join(lines[1:i], "\n")), &metadata) == nil {
			return managed(strings.TrimSpace(metadata.Name))
		}
		return false
	}
	return false
}

func discoverClaudeUserSkills() ([]core.RuntimeSkill, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, core.Fail("unavailable", "Claude user skill home is unavailable")
	}
	root := filepath.Join(home, ".claude", "skills")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return []core.RuntimeSkill{}, nil
	}
	if err != nil {
		return nil, core.Fail("unavailable", "Claude user skill directory is unavailable")
	}
	out := []core.RuntimeSkill{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "memgov-memory" || entry.Name() == "memgov-workspace" {
			continue
		}
		path, err := filepath.EvalSymlinks(filepath.Join(root, entry.Name()))
		if err != nil {
			// Executor-inherited skills are optional ambient capabilities. A
			// concurrently replaced or temporarily unavailable entry must not
			// prevent an otherwise independent Agent turn from starting.
			continue
		}
		skillFile := filepath.Join(path, "SKILL.md")
		if info, err := os.Stat(skillFile); err != nil || !info.Mode().IsRegular() {
			continue
		}
		body, err := os.ReadFile(skillFile)
		if err != nil || managedKnowledgeSkill(entry.Name(), path, body) {
			continue
		}
		digest, err := runtimeSkillDigest(path)
		if err != nil {
			continue
		}
		out = append(out, core.RuntimeSkill{Name: entry.Name(), Path: path, Digest: digest, Summary: runtimeSkillSummary(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func prepareClaudeSkills(in *ExecutionInput) error {
	resolved, err := resolveClaudeSkillPolicy(in.Skills)
	if err != nil {
		return err
	}
	in.Skills = resolved
	return stageResolvedSkills(*in)
}

func resolveClaudeSkillPolicy(policy core.RuntimeSkillPolicy) (core.RuntimeSkillPolicy, error) {
	if policy.Inherit == "" {
		policy.Inherit = "none"
	}
	explicitNames := map[string]bool{}
	for _, path := range policy.Paths {
		explicitNames[filepath.Base(filepath.Clean(path))] = true
	}
	for _, skill := range policy.Resolved {
		// Executor inheritance is rediscovered below and may disappear between
		// turns. Explicit policy entries must remain available and unchanged.
		if policy.Inherit == "executor" && !explicitNames[skill.Name] {
			continue
		}
		body, err := os.ReadFile(filepath.Join(skill.Path, "SKILL.md"))
		if err != nil {
			return policy, core.Fail("conflict", "configured Agent skill is unavailable: %s", skill.Name)
		}
		if managedKnowledgeSkill(skill.Name, skill.Path, body) {
			return policy, core.Fail("conflict", "workspace knowledge skill is runtime-managed; retired memory skill aliases are disabled")
		}
	}
	explicit := map[string]core.RuntimeSkill{}
	for _, skill := range policy.Resolved {
		if !explicitNames[skill.Name] {
			continue
		}
		digest, err := runtimeSkillDigest(skill.Path)
		if err != nil || digest != skill.Digest {
			return policy, core.Fail("conflict", "configured Agent skill changed or is unavailable: %s", skill.Name)
		}
		explicit[skill.Name] = skill
	}
	if len(explicit) != len(explicitNames) {
		return policy, core.Fail("conflict", "configured explicit Agent skill is unresolved")
	}
	resolved := []core.RuntimeSkill{}
	if policy.Inherit == "executor" {
		var err error
		resolved, err = discoverClaudeUserSkills()
		if err != nil {
			return policy, err
		}
	} else {
		for _, skill := range policy.Resolved {
			if explicitNames[skill.Name] {
				continue
			}
			digest, err := runtimeSkillDigest(skill.Path)
			if err != nil || digest != skill.Digest {
				return policy, core.Fail("conflict", "configured Agent skill changed or is unavailable: %s", skill.Name)
			}
			resolved = append(resolved, skill)
		}
	}
	byName := map[string]core.RuntimeSkill{}
	for _, skill := range resolved {
		byName[skill.Name] = skill
	}
	for name, skill := range explicit {
		byName[name] = skill
	}
	policy.Resolved = policy.Resolved[:0]
	for _, skill := range byName {
		policy.Resolved = append(policy.Resolved, skill)
	}
	sort.Slice(policy.Resolved, func(i, j int) bool { return policy.Resolved[i].Name < policy.Resolved[j].Name })
	return policy, nil
}

func stageResolvedSkills(in ExecutionInput) error {
	root := filepath.Join(in.WorkDir, ".claude", "skills")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Join(in.WorkDir, ".claude"), root} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return core.Fail("denied", "Agent skill staging directory is not a regular directory")
		}
	}
	manifest := filepath.Join(in.WorkDir, ".claude", ".memgov-agent-skills.json")
	var previous []string
	if info, err := os.Lstat(manifest); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return core.Fail("denied", "Agent skill manifest is not a regular file")
		}
		if raw, readErr := os.ReadFile(manifest); readErr == nil {
			_ = json.Unmarshal(raw, &previous)
		}
	}
	for _, name := range previous {
		if name != "memgov-workspace" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, "/\\\r\n\x00") {
			_ = os.RemoveAll(filepath.Join(root, name))
		}
	}
	// Stage only the resolved policy. Provider user settings stay disabled, so
	// inherited skill discovery cannot autoload the retired memory skill.
	if err := os.RemoveAll(filepath.Join(root, "memgov-memory")); err != nil {
		return err
	}
	staged := []string{}
	for _, skill := range in.Skills.Resolved {
		if skill.Name == "memgov-memory" || skill.Name == "memgov-workspace" {
			return core.Fail("conflict", "workspace knowledge skill is managed by the runtime; legacy memory skills are disabled")
		}
		target := filepath.Join(root, skill.Name)
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := os.Symlink(skill.Path, target); err != nil {
			return err
		}
		staged = append(staged, skill.Name)
	}
	raw, _ := json.Marshal(staged)
	temporary, err := os.CreateTemp(filepath.Dir(manifest), ".memgov-skills-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(0600); err == nil {
		var written int
		written, err = temporary.Write(raw)
		if err == nil && written != len(raw) {
			err = io.ErrShortWrite
		}
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporaryPath, manifest)
}

func skillAllowlist(policy core.RuntimeSkillPolicy) []string {
	out := []string{}
	for _, skill := range policy.Resolved {
		out = append(out, "Skill("+skill.Name+")")
	}
	return out
}

func directSettingSources(_ core.RuntimeSkillPolicy) string {
	return "project"
}
