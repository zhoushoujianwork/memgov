package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// Transform only retired knowledge settings. Keep routing, skills, permissions,
// presets and explicit empty capability lists exactly as declared.
func convertWorkspaceConfig(raw []byte) ([]byte, []string, bool, error) {
	var tree yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(raw))
	if err := d.Decode(&tree); err != nil {
		return nil, nil, false, core.Fail("invalid_input", "invalid migration YAML")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, nil, false, core.Fail("invalid_input", "configuration must contain a single YAML document")
	}
	var homes []string
	changed := false
	var visit func(*yaml.Node, []string) error
	visit = func(n *yaml.Node, path []string) error {
		if n.Kind == yaml.AliasNode {
			return core.Fail("invalid_input", "expand YAML aliases before Workspace migration")
		}
		if n.Kind == yaml.MappingNode {
			out := []*yaml.Node{}
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if seen[k.Value] {
					return core.Fail("invalid_input", "duplicate configuration key")
				}
				seen[k.Value] = true
				agentField := len(path) == 2 && path[0] == "agents"
				groupField := strings.Join(path, "/") == "applications/group_mention" || (len(path) == 4 && path[0] == "applications" && path[1] == "bots" && path[3] == "group_mention")
				schedulingField := strings.Join(path, "/") == "runtime_setup" || strings.Join(path, "/") == "applications/proactive"
				retired := (agentField && k.Value == "memory_scope") || (strings.Join(path, "/") == "channels/route" && k.Value == "memory_policy") || (groupField && (k.Value == "shared_memory_workspaces" || k.Value == "excluded_memory_categories")) || (schedulingField && k.Value == "review_timeout_seconds")
				if k.Value == "home" && agentField {
					if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
						return core.Fail("invalid_input", "legacy Agent home must be a path string")
					}
					homes = append(homes, v.Value)
					retired = true
				}
				if retired {
					changed = true
					continue
				}
				skillPaths := len(path) == 3 && path[0] == "agents" && path[2] == "skills" && k.Value == "paths"
				if v.Kind == yaml.SequenceNode && ((agentField && k.Value == "capabilities") || skillPaths) {
					items := []*yaml.Node{}
					for _, item := range v.Content {
						if (k.Value == "capabilities" && item.Value == "memory_read") || (k.Value == "paths" && filepath.Base(filepath.Clean(item.Value)) == "memgov-memory") {
							changed = true
							continue
						}
						items = append(items, item)
					}
					v.Content = items
				}
				if err := visit(v, append(append([]string{}, path...), k.Value)); err != nil {
					return err
				}
				out = append(out, k, v)
			}
			n.Content = out
		} else {
			for _, child := range n.Content {
				if err := visit(child, path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(&tree, nil); err != nil {
		return nil, nil, false, err
	}
	if !changed {
		return raw, homes, false, nil
	}
	body, err := yaml.Marshal(&tree)
	return body, homes, changed, err
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func writeVerifiedArchiveFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	check, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if core.Hash(body) != core.Hash(check) {
		return core.Fail("conflict", "configuration archive verification failed")
	}
	return syncDirectory(filepath.Dir(path))
}

func (a *app) migrateWorkspaceConfig() (any, error) {
	path, err := filepath.Abs(a.configPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, core.Fail("denied", "configuration must be a regular file, not a symlink")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, core.Fail("conflict", "configuration migration already running")
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	raw, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		return nil, err
	}
	if len(raw) == 8<<20 {
		return nil, core.Fail("invalid_input", "configuration exceeds 8 MiB")
	}
	body, homes, changed, err := convertWorkspaceConfig(raw)
	if err != nil {
		return nil, err
	}
	if err = (&app{configPath: path}).loadConfig(body); err != nil {
		return nil, err
	}
	if !changed {
		return map[string]any{"migrated": false, "config_path": path}, nil
	}
	archive := filepath.Join(a.home, "backups", "workspace-cutover", "config-"+core.NewID())
	if err = writeVerifiedArchiveFile(filepath.Join(archive, "config.yaml"), raw); err != nil {
		return nil, err
	}
	manifest := map[string]string{"config.yaml": core.Hash(raw)}
	canonicalArchive, err := filepath.EvalSymlinks(archive)
	if err != nil {
		return nil, err
	}
	homes = append(homes, filepath.Join(a.home, "agent-homes"))
	seen := map[string]bool{}
	for _, home := range homes {
		if home == "" {
			continue
		}
		if home == "~" || strings.HasPrefix(home, "~/") {
			userHome, e := os.UserHomeDir()
			if e != nil {
				return nil, e
			}
			home = filepath.Join(userHome, strings.TrimPrefix(strings.TrimPrefix(home, "~"), "/"))
		}
		if !filepath.IsAbs(home) {
			home = filepath.Join(filepath.Dir(path), home)
		}
		home = filepath.Clean(home)
		if info, e := os.Lstat(home); os.IsNotExist(e) {
			continue
		} else if e != nil {
			return nil, e
		} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, core.Fail("denied", "legacy Agent home must be a directory, not a symlink")
		}
		originalHome := home
		home, err = filepath.EvalSymlinks(home)
		if err != nil {
			return nil, err
		}
		if rel, e := filepath.Rel(home, canonicalArchive); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, core.Fail("invalid_input", "legacy Agent home contains the archive directory; choose a separate data home for conversion")
		}
		if seen[home] {
			continue
		}
		seen[home] = true
		prefix := filepath.Join("agent-homes", core.Hash([]byte(originalHome)))
		manifest[prefix+"/@source"] = originalHome
		if err = filepath.WalkDir(home, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return core.Fail("denied", "legacy notes contain a symlink; cannot create complete archive")
			}
			if entry.IsDir() {
				return nil
			}
			info, e := entry.Info()
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() {
				return core.Fail("denied", "legacy notes contain a non-regular file")
			}
			b, e := os.ReadFile(name)
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(home, name)
			if e != nil {
				return e
			}
			dest := filepath.Join(prefix, rel)
			if e = writeVerifiedArchiveFile(filepath.Join(archive, dest), b); e != nil {
				return e
			}
			current, e := os.ReadFile(name)
			if e != nil {
				return e
			}
			if core.Hash(current) != core.Hash(b) {
				return core.Fail("conflict", "legacy notes changed while archiving")
			}
			manifest[dest] = core.Hash(b)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	manifestBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err = writeVerifiedArchiveFile(filepath.Join(archive, "manifest.json"), manifestBody); err != nil {
		return nil, err
	}
	if err = syncDirectory(filepath.Dir(archive)); err != nil {
		return nil, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, currentInfo) || core.Hash(current) != core.Hash(raw) {
		return nil, core.Fail("conflict", "configuration changed during archive; retry migration")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".workspace-config-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(body); err != nil {
		return nil, err
	}
	if err = tmp.Sync(); err != nil {
		return nil, err
	}
	if err = tmp.Close(); err != nil {
		return nil, err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	if err = syncDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return map[string]any{"migrated": true, "config_path": path, "archive_path": archive, "legacy_notes_imported": false}, nil
}
