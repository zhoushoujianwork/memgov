package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func skillSummary(body []byte) string {
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

func skillDigest(path string) (string, error) {
	parts := []string{}
	err := filepath.WalkDir(path, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return core.Fail("denied", "skill contains a symbolic link: %s", name)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return core.Fail("denied", "skill contains a non-regular file: %s", name)
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

func inspectSkill(path string) (core.RuntimeSkill, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return core.RuntimeSkill{}, core.Fail("not_found", "Agent skill path is unavailable: %s", path)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return core.RuntimeSkill{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return core.RuntimeSkill{}, core.Fail("invalid_input", "Agent skill path must be a directory: %s", path)
	}
	skillFile := filepath.Join(resolved, "SKILL.md")
	if info, err = os.Stat(skillFile); err != nil || !info.Mode().IsRegular() {
		return core.RuntimeSkill{}, core.Fail("invalid_input", "Agent skill requires SKILL.md: %s", path)
	}
	body, err := os.ReadFile(skillFile)
	if err != nil {
		return core.RuntimeSkill{}, err
	}
	name := filepath.Base(filepath.Clean(path))
	if name == "." || name == string(filepath.Separator) || strings.ContainsAny(name, "\r\n\x00/") {
		return core.RuntimeSkill{}, core.Fail("invalid_input", "Agent skill has an invalid directory name")
	}
	digest, err := skillDigest(resolved)
	if err != nil {
		return core.RuntimeSkill{}, err
	}
	return core.RuntimeSkill{Name: name, Path: resolved, Digest: digest, Summary: skillSummary(body)}, nil
}

func resolveClaudeSkills(policy core.RuntimeSkillPolicy) ([]core.RuntimeSkill, error) {
	byName := map[string]core.RuntimeSkill{}
	if policy.Inherit == "executor" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(filepath.Join(home, ".claude", "skills"))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "memgov-memory" {
				continue
			}
			skill, inspectErr := inspectSkill(filepath.Join(home, ".claude", "skills", entry.Name()))
			if inspectErr != nil {
				return nil, inspectErr
			}
			byName[skill.Name] = skill
		}
	}
	explicit := map[string]bool{}
	for _, path := range policy.Paths {
		skill, err := inspectSkill(path)
		if err != nil {
			return nil, err
		}
		if skill.Name == "memgov-memory" {
			return nil, core.Fail("conflict", "memgov-memory is managed by the runtime")
		}
		if explicit[skill.Name] {
			return nil, core.Fail("conflict", "duplicate explicit Agent skill name: %s", skill.Name)
		}
		explicit[skill.Name] = true
		byName[skill.Name] = skill
	}
	out := make([]core.RuntimeSkill, 0, len(byName))
	for _, skill := range byName {
		out = append(out, skill)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
