package runtime

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func (s *Service) resolveTaskAgent(ctx context.Context, cfg core.RuntimeConfig, task core.RuntimeTask) (core.RuntimeAgentPolicy, agent.Preset, error) {
	policy, err := core.ResolveRuntimeTaskAgent(ctx, s.Store.DB, cfg, task)
	if err != nil {
		return policy, agent.Preset{}, err
	}
	if cfg.ApplicationMode == "group_mention" {
		if policy.KnowledgeMCP != nil {
			return policy, agent.Preset{}, core.Fail("denied", "group Agents cannot use Owner knowledge sources")
		}
		if hasAgentCapability(policy.Capabilities, "local_test") && !policy.BashEnabled {
			return policy, agent.Preset{}, core.Fail("denied", "group Agents cannot execute shell tests")
		}
		if len(policy.Directories) > 0 && !hasAgentCapability(policy.Capabilities, "local_read") {
			return policy, agent.Preset{}, core.Fail("denied", "directory snapshots require local_read capability")
		}
	}
	if policy.BashEnabled && len(policy.Directories) > 0 {
		return policy, agent.Preset{}, core.Fail("denied", "full Bash cannot be combined with bounded directory snapshots")
	}
	if cfg.ApplicationMode != "group_mention" {
		if err := validateOwnerDirectoryPolicy(policy); err != nil {
			return policy, agent.Preset{}, err
		}
	}
	preset, err := agent.Status(ctx, s.Home, policy.Preset)
	if err != nil {
		return policy, preset, err
	}
	if preset.Status != "enabled" || !preset.Clean {
		return policy, preset, core.Fail("denied", "task Agent preset must be enabled and clean")
	}
	if err = s.checkPresetHarness(preset); err != nil {
		return policy, preset, err
	}
	return policy, preset, nil
}

func (s *Service) checkPresetHarness(preset agent.Preset) error {
	if s.HarnessName != "" && preset.Provider != s.HarnessName {
		return core.Fail("invalid_input", "Agent preset harness %q differs from runtime harness %q; use a runtime configured for the selected harness", preset.Provider, s.HarnessName)
	}
	return nil
}
func (s *Service) checkTaskAgent(ctx context.Context, cfg core.RuntimeConfig, task core.RuntimeTask, policy core.RuntimeAgentPolicy, preset agent.Preset) error {
	current, currentPreset, err := s.resolveTaskAgent(ctx, cfg, task)
	if err != nil {
		return err
	}
	if core.Digest(current) != core.Digest(policy) || currentPreset.Commit != preset.Commit {
		return core.Fail("conflict", "task Agent policy changed during execution")
	}
	return nil
}

// DirectorySnapshot contains only bounded UTF-8 regular files. Original host
// paths are not sent to the group model, and no source directory is mounted as
// an executable workspace. Copies are retained with the task for audit.
type DirectorySnapshot struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

const snapshotMaxFiles = 200
const snapshotMaxBytes = 2 * 1024 * 1024
const snapshotMaxFileBytes = 128 * 1024

func stageGroupDirectories(ctx context.Context, workdir string, roots []string) ([]DirectorySnapshot, error) {
	out := []DirectorySnapshot{}
	total := 0
	for i, path := range roots {
		if !filepath.IsAbs(path) {
			return nil, core.Fail("denied", "Agent directory must be absolute")
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, core.Fail("unavailable", "Agent input directory is unavailable")
		}
		if canonical != filepath.Clean(path) {
			return nil, core.Fail("denied", "Agent input directory cannot use symbolic links")
		}
		before, err := os.Lstat(path)
		if err != nil || !before.IsDir() {
			return nil, core.Fail("denied", "Agent input directory changed before opening")
		}
		root, err := os.OpenRoot(path)
		if err != nil {
			return nil, core.Fail("unavailable", "Agent input directory cannot be opened")
		}
		after, statErr := root.Stat(".")
		if statErr != nil || !os.SameFile(before, after) {
			root.Close()
			return nil, core.Fail("denied", "Agent input directory changed while opening")
		}
		err = fs.WalkDir(root.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return core.Fail("unavailable", "Agent directory entry cannot be read")
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return core.Fail("denied", "Agent input directory contains a symbolic link")
			}
			// Runtime/credential controls and dependency caches are not task documents.
			if rel != "." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules") {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			info, e := entry.Info()
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() {
				return core.Fail("denied", "Agent input must contain only regular files")
			}
			if info.Size() > snapshotMaxFileBytes {
				return core.Fail("denied", "Agent input file exceeds snapshot limit")
			}
			f, e := root.Open(rel)
			if e != nil {
				return core.Fail("denied", "Agent input escaped its declared root")
			}
			opened, statErr := f.Stat()
			if statErr != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
				f.Close()
				return core.Fail("denied", "Agent input file changed while opening")
			}
			content, e := io.ReadAll(io.LimitReader(f, snapshotMaxFileBytes+1))
			f.Close()
			if e != nil {
				return e
			}
			if len(content) > snapshotMaxFileBytes {
				return core.Fail("denied", "Agent input file exceeded snapshot limit")
			}
			if !utf8.Valid(content) {
				return nil
			}
			if len(out) >= snapshotMaxFiles || total+len(content) > snapshotMaxBytes {
				return core.Fail("denied", "Agent directory snapshot exceeds task limit")
			}
			destination := filepath.Join("inputs", fmt.Sprintf("%03d", i+1), rel)
			full := filepath.Join(workdir, destination)
			if e = os.MkdirAll(filepath.Dir(full), 0700); e != nil {
				return e
			}
			if e = os.WriteFile(full, content, 0400); e != nil {
				return e
			}
			total += len(content)
			out = append(out, DirectorySnapshot{Path: filepath.ToSlash(destination), Content: string(content)})
			return nil
		})
		root.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// validateGroupArtifacts refuses capability URLs and host paths masquerading as
// results. Group products must be regular files created under this task's output
// directory; rooted I/O prevents a raced symlink from reaching outside it.
func validateGroupArtifacts(workdir string, artifacts []string) ([]string, error) {
	out := []string{}
	if len(artifacts) == 0 {
		return out, nil
	}
	base := filepath.Join(workdir, "artifacts")
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, core.Fail("denied", "group artifact directory is unavailable")
	}
	defer root.Close()
	for _, path := range artifacts {
		full := path
		if !filepath.IsAbs(full) {
			full = filepath.Join(workdir, path)
		}
		rel, e := filepath.Rel(base, full)
		if e != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, core.Fail("denied", "group artifact is outside its output directory")
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		current := ""
		for _, part := range parts {
			current = filepath.Join(current, part)
			info, e := root.Lstat(current)
			if e != nil || info.Mode()&os.ModeSymlink != 0 {
				return nil, core.Fail("denied", "group artifact cannot contain symbolic links")
			}
		}
		f, e := root.Open(rel)
		if e != nil {
			return nil, core.Fail("denied", "group artifact escaped its output directory")
		}
		info, e := f.Stat()
		f.Close()
		if e != nil || !info.Mode().IsRegular() {
			return nil, core.Fail("denied", "group artifact must be a regular file")
		}
		out = append(out, filepath.Join(base, rel))
	}
	return out, nil
}

func hasAgentCapability(capabilities []string, capability string) bool {
	for _, c := range capabilities {
		if c == capability {
			return true
		}
	}
	return false
}
