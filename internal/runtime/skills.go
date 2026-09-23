package runtime

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/processtree"
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
// Native/global memory writers would create a second knowledge authority and
// mix audiences; durable learning is provided by the managed workspace skill.
func managedKnowledgeSkill(name, path string, body []byte) bool {
	managed := func(value string) bool {
		switch value {
		case "memgov-memory", "memgov-workspace", "touch-memory", "error-reflection":
			return true
		}
		return false
	}
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
			return policy, core.Fail("conflict", "workspace knowledge skill is runtime-managed; retired and global memory skill aliases are disabled")
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

func validStagedSkillName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, "/\\\r\n\x00")
}

const skillRootReceiptName = ".memgov-skill-root.json"
const originalSkillDirectory = ".memgov-original-skills"

type skillRootReceipt struct {
	OriginalTarget    string            `json:"original_target,omitempty"`
	OriginalDirectory bool              `json:"original_directory,omitempty"`
	OriginalDigest    string            `json:"original_digest,omitempty"`
	StagedTargets     map[string]string `json:"staged_targets"`
}

func readSkillRootReceipt(claude *os.Root) (skillRootReceipt, error) {
	var receipt skillRootReceipt
	info, err := claude.Lstat(skillRootReceiptName)
	if os.IsNotExist(err) {
		return receipt, nil
	}
	if err != nil {
		return receipt, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return receipt, core.Fail("denied", "Agent skill staging receipt is not a regular file")
	}
	raw, err := claude.ReadFile(skillRootReceiptName)
	if err != nil {
		return receipt, err
	}
	if err = json.Unmarshal(raw, &receipt); err != nil || (receipt.OriginalTarget != "") == receipt.OriginalDirectory || (receipt.OriginalDirectory && receipt.OriginalDigest == "") {
		return receipt, core.Fail("denied", "Agent skill staging receipt is invalid")
	}
	for name := range receipt.StagedTargets {
		if !validStagedSkillName(name) {
			return receipt, core.Fail("denied", "Agent skill staging receipt contains an invalid directory entry")
		}
	}
	return receipt, nil
}

func stageResolvedSkills(in ExecutionInput) error {
	for _, skill := range in.Skills.Resolved {
		if !validStagedSkillName(skill.Name) {
			return core.Fail("denied", "Agent skill name is not a directory entry")
		}
		if skill.Name == "memgov-memory" || skill.Name == "memgov-workspace" {
			return core.Fail("conflict", "workspace knowledge skill is managed by the runtime; legacy memory skills are disabled")
		}
	}
	workdir, err := os.OpenRoot(in.WorkDir)
	if err != nil {
		return err
	}
	defer workdir.Close()
	// Check the parent before creating anything beneath it. Repository skill
	// links are supported, but a linked .claude directory is not a staging root.
	if err = workdir.Mkdir(".claude", 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := workdir.Lstat(".claude")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return core.Fail("denied", "Agent skill staging directory is not a regular directory")
	}
	claude, err := workdir.OpenRoot(".claude")
	if err != nil {
		return err
	}
	defer claude.Close()
	const manifest = ".memgov-agent-skills.json"
	managedManifest := false
	if info, err = claude.Lstat(manifest); err == nil {
		managedManifest = true
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return core.Fail("denied", "Agent skill manifest is not a regular file")
		}
		raw, err := claude.ReadFile(manifest)
		if err != nil {
			return err
		}
		var previous []string
		if err = json.Unmarshal(raw, &previous); err != nil {
			return core.Fail("denied", "Agent skill manifest is invalid")
		}
		for _, name := range previous {
			if !validStagedSkillName(name) {
				return core.Fail("denied", "Agent skill manifest contains an invalid directory entry")
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	receipt, err := readSkillRootReceipt(claude)
	if err != nil {
		return err
	}
	if info, err = claude.Lstat("skills"); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if receipt.OriginalDirectory {
				return core.Fail("denied", "Agent replaced the staged skill directory with a link")
			}
			target, err := claude.Readlink("skills")
			if err != nil {
				return err
			}
			if receipt.OriginalTarget != "" && receipt.OriginalTarget != target {
				return core.Fail("denied", "Agent changed the original skill root link")
			}
			receipt.OriginalTarget = target
			// Persist provenance before unlinking so a failed initialization can
			// resume without losing the repository's original control layout.
			raw, _ := json.Marshal(receipt)
			if err = writeSkillStagingMetadata(claude, skillRootReceiptName, raw); err != nil {
				return err
			}
			if err = claude.Remove("skills"); err != nil {
				return err
			}
		} else if !info.IsDir() {
			return core.Fail("denied", "Agent skill staging directory is not a regular directory")
		} else {
			if receipt.OriginalTarget == "" && !receipt.OriginalDirectory && (!managedManifest || trackedProjectSkills(in.WorkDir)) {
				if _, err := claude.Lstat(originalSkillDirectory); !os.IsNotExist(err) {
					return core.Fail("denied", "Agent skill preservation directory already exists")
				}
				receipt.OriginalDigest, err = skillDirectoryDigest(claude, "skills")
				if err != nil {
					return err
				}
				receipt.OriginalDirectory = true
				raw, _ := json.Marshal(receipt)
				if err = writeSkillStagingMetadata(claude, skillRootReceiptName, raw); err != nil {
					return err
				}
			}
			if receipt.OriginalDirectory {
				if _, err := claude.Lstat(originalSkillDirectory); os.IsNotExist(err) {
					digest, err := skillDirectoryDigest(claude, "skills")
					if err != nil || digest != receipt.OriginalDigest {
						return core.Fail("denied", "Agent original skill directory changed during initialization")
					}
					if err = claude.Rename("skills", originalSkillDirectory); err != nil {
						return err
					}
				} else if err != nil {
					return err
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = claude.Mkdir("skills", 0700); err != nil && !os.IsExist(err) {
		return err
	}
	root, err := claude.OpenRoot("skills")
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	// Stage only the resolved policy, including on a reused task directory.
	// Unlisted project skills must not autoload. The current managed workspace
	// skill is recreated by prepareWorkspaceTool after this step.
	for _, entry := range entries {
		if err = root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	staged := []string{}
	receipt.StagedTargets = map[string]string{}
	for _, skill := range in.Skills.Resolved {
		if err = root.Symlink(skill.Path, skill.Name); err != nil {
			return err
		}
		staged = append(staged, skill.Name)
		receipt.StagedTargets[skill.Name] = skill.Path
	}
	if receipt.OriginalTarget != "" || receipt.OriginalDirectory {
		raw, _ := json.Marshal(receipt)
		if err = writeSkillStagingMetadata(claude, skillRootReceiptName, raw); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(staged)
	return writeSkillStagingMetadata(claude, manifest, raw)
}

func writeSkillStagingMetadata(claude *os.Root, name string, raw []byte) error {
	temporaryPath := ".memgov-skills-" + core.NewID()
	temporary, err := claude.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer claude.Remove(temporaryPath)
	written, err := temporary.Write(raw)
	if err == nil && written != len(raw) {
		err = io.ErrShortWrite
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return claude.Rename(temporaryPath, name)
}

func trackedProjectSkills(workdir string) bool {
	command := exec.Command("git", "-C", workdir, "ls-files", "-z", "--", ".claude/skills")
	raw, err := command.Output()
	return err == nil && len(raw) != 0
}

// Digest preserved entries without following any skill symlinks.
func skillDirectoryDigest(claude *os.Root, name string) (string, error) {
	info, err := claude.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", core.Fail("denied", "Agent preserved skills are not a regular directory")
	}
	root, err := claude.OpenRoot(name)
	if err != nil {
		return "", err
	}
	defer root.Close()
	parts := []string{}
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := ""
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			value, err = root.Readlink(path)
		case info.IsDir():
		case info.Mode().IsRegular():
			var raw []byte
			raw, err = root.ReadFile(path)
			value = core.Hash(raw)
		default:
			return core.Fail("denied", "Agent skill directory contains an unsupported file")
		}
		if err != nil {
			return err
		}
		parts = append(parts, path+"\x00"+info.Mode().String()+"\x00"+value)
		return nil
	})
	return core.Digest(parts), err
}

// Restore runtime-only replacements before checking a task's business changes.
// Git's base, HEAD and index authenticate the original layout; the receipt alone
// cannot authorize restoring or removing arbitrary project files.
func restoreRuntimeSkillRoot(ctx context.Context, workdir, base string) error {
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(".claude")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return core.Fail("conflict", "Agent changed the runtime skill parent directory")
	}
	claude, err := root.OpenRoot(".claude")
	if err != nil {
		return err
	}
	defer claude.Close()
	receipt, err := readSkillRootReceipt(claude)
	if err != nil || (receipt.OriginalTarget == "" && !receipt.OriginalDirectory) {
		return err
	}
	gitOutput := func(args ...string) (string, error) {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...)
		raw, err := processtree.Output(ctx, command)
		return string(raw), err
	}
	baseEntry, err := gitOutput("ls-tree", "-z", base, "--", ".claude/skills")
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSuffix(baseEntry, "\x00"), "\t")
	if len(parts) != 2 || parts[1] != ".claude/skills" {
		// An untracked project link has no repository state to restore.
		return nil
	}
	fields := strings.Fields(parts[0])
	if len(fields) != 3 {
		return core.Fail("conflict", "Agent skill root has an invalid task base")
	}
	if receipt.OriginalDirectory {
		if fields[0] != "040000" || fields[1] != "tree" {
			return core.Fail("conflict", "Agent skill root did not originate from a tracked directory")
		}
	} else {
		if fields[0] != "120000" || fields[1] != "blob" {
			return core.Fail("conflict", "Agent skill root did not originate from a tracked link")
		}
		target, err := gitOutput("cat-file", "blob", fields[2])
		if err != nil || target != receipt.OriginalTarget {
			return core.Fail("conflict", "Agent skill root restoration does not match the task base")
		}
	}
	headEntry, err := gitOutput("ls-tree", "-z", "HEAD", "--", ".claude/skills")
	if err != nil || headEntry != baseEntry {
		return core.Fail("conflict", "Agent committed changes to the runtime skill root")
	}
	if _, err = gitOutput("diff", "--cached", "--quiet", base, "--", ".claude/skills"); err != nil {
		return core.Fail("conflict", "Agent staged changes to the runtime skill root")
	}
	if receipt.OriginalDirectory {
		digest, err := skillDirectoryDigest(claude, originalSkillDirectory)
		if err != nil {
			// Restoring the directory may have completed before a process exit.
			if _, statErr := claude.Lstat(originalSkillDirectory); os.IsNotExist(statErr) {
				digest, err = skillDirectoryDigest(claude, "skills")
				if err == nil && digest == receipt.OriginalDigest {
					return claude.Remove(skillRootReceiptName)
				}
			}
			return core.Fail("conflict", "Agent original skill directory is unavailable")
		}
		if digest != receipt.OriginalDigest {
			return core.Fail("conflict", "Agent changed the preserved skill directory")
		}
	}
	info, err = claude.Lstat("skills")
	if os.IsNotExist(err) {
		return finishSkillRootRestoration(claude, receipt)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := claude.Readlink("skills")
		if receipt.OriginalDirectory || err != nil || target != receipt.OriginalTarget {
			return core.Fail("conflict", "Agent changed the runtime skill root link")
		}
		return claude.Remove(skillRootReceiptName)
	}
	if !info.IsDir() {
		return core.Fail("conflict", "Agent changed the runtime skill root directory")
	}
	skills, err := claude.OpenRoot("skills")
	if err != nil {
		return err
	}
	defer skills.Close()
	entries, err := fs.ReadDir(skills.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if target, ok := receipt.StagedTargets[entry.Name()]; ok {
			actual, err := skills.Readlink(entry.Name())
			if err != nil || actual != target {
				return core.Fail("conflict", "Agent changed a staged runtime skill")
			}
			continue
		}
		if entry.Name() != "memgov-workspace" || !entry.IsDir() {
			return core.Fail("conflict", "Agent added files to the runtime skill directory")
		}
		files, err := fs.ReadDir(skills.FS(), "memgov-workspace")
		if err != nil || len(files) != 1 || files[0].Name() != "SKILL.md" || files[0].Type()&os.ModeSymlink != 0 {
			return core.Fail("conflict", "Agent changed the managed workspace skill")
		}
		actual, err := skills.ReadFile("memgov-workspace/SKILL.md")
		expected, readErr := workspaceSkill.ReadFile("workspace_skill/SKILL.md")
		if err != nil || readErr != nil || string(actual) != string(expected) {
			return core.Fail("conflict", "Agent changed the managed workspace skill")
		}
	}
	if err = claude.RemoveAll("skills"); err != nil {
		return err
	}
	return finishSkillRootRestoration(claude, receipt)
}

func finishSkillRootRestoration(claude *os.Root, receipt skillRootReceipt) error {
	var err error
	if receipt.OriginalDirectory {
		err = claude.Rename(originalSkillDirectory, "skills")
	} else {
		err = claude.Symlink(receipt.OriginalTarget, "skills")
	}
	if err != nil {
		return err
	}
	return claude.Remove(skillRootReceiptName)
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
