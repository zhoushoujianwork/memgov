package core

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

type knowledgeArchiveEntry struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}
type knowledgeArchiveManifest struct {
	Format         string                   `json:"format"`
	DatabaseSHA256 string                   `json:"database_sha256"`
	Sources        []knowledgeArchiveSource `json:"sources"`
	Entries        []knowledgeArchiveEntry  `json:"entries"`
}

type knowledgeArchiveSource struct {
	Path  string `json:"path"`
	Entry string `json:"entry"`
}

// archiveLegacyKnowledge preserves files beside the database snapshot. Archives
// are never an Agent Workspace or a runtime mount. Refuse incomplete snapshots
// rather than follow links into data outside a configured knowledge directory.
func (s *Store) archiveLegacyKnowledge(ctx context.Context, backup *BackupInfo) error {
	home := filepath.Dir(s.Path)
	sources := []knowledgeArchiveSource{}
	custom := map[string]bool{}
	addHomes := func(value map[string]any) error {
		agents, _ := value["agents"].(map[string]any)
		for _, v := range agents {
			agent, _ := v.(map[string]any)
			path, _ := agent["home"].(string)
			if path == "" {
				continue
			}
			if path == "~" || strings.HasPrefix(path, "~/") {
				user, e := os.UserHomeDir()
				if e != nil {
					return e
				}
				path = filepath.Join(user, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(home, path)
			}
			path = filepath.Clean(path)
			canonical, e := filepath.EvalSymlinks(path)
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil {
				return e
			}
			for _, protected := range []string{home, filepath.Dir(backup.Path)} {
				protected, e = filepath.EvalSymlinks(protected)
				if e != nil {
					return e
				}
				rel, e := filepath.Rel(canonical, protected)
				if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return Fail("invalid_input", "legacy Agent home contains the state or archive directory and cannot be archived independently")
				}
			}
			custom[path] = true
		}
		return nil
	}
	for _, name := range []string{"config.yaml", "config.dual.yaml"} {
		path := filepath.Join(home, name)
		st, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return Fail("invalid_input", "legacy configuration archive requires a regular file: %s", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var config map[string]any
		if err = yaml.Unmarshal(raw, &config); err != nil {
			return fmt.Errorf("archive legacy configuration: %w", err)
		}
		if err = addHomes(config); err != nil {
			return err
		}
		sources = append(sources, knowledgeArchiveSource{Path: path, Entry: name})
	}
	// Old declarations retain custom Agent homes even when the current YAML was
	// moved or replaced. Archive every explicitly configured home before removal.
	var hasApplied int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='applied_configs'").Scan(&hasApplied); err != nil {
		return err
	}
	if hasApplied != 0 {
		rows, err := s.DB.QueryContext(ctx, "SELECT declaration FROM applied_configs")
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw string
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			var declaration map[string]any
			// Invalid JSON is rejected by the transaction hook. Preserve the database
			// snapshot first so the original invalid bytes remain recoverable.
			if json.Unmarshal([]byte(raw), &declaration) == nil {
				if err = addHomes(declaration); err != nil {
					rows.Close()
					return err
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	defaultHomes := filepath.Join(home, "agent-homes")
	if _, err := os.Lstat(defaultHomes); err == nil {
		sources = append(sources, knowledgeArchiveSource{Path: defaultHomes, Entry: "agent-homes"})
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	paths := make([]string, 0, len(custom))
	for path := range custom {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if path == defaultHomes || strings.HasPrefix(path, defaultHomes+string(filepath.Separator)) {
			continue
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		sources = append(sources, knowledgeArchiveSource{Path: path, Entry: "custom-agent-homes/" + Hash([]byte(path))})
	}
	destination := backup.Path + ".knowledge.tar"
	temp := destination + ".tmp-" + NewID()
	defer os.Remove(temp)
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(file)
	archive := knowledgeArchiveManifest{Format: "memgov.legacy-knowledge", DatabaseSHA256: backup.SHA256, Sources: sources, Entries: []knowledgeArchiveEntry{}}
	failed := func(err error) error { tw.Close(); file.Close(); return err }
	for _, source := range sources {
		if err = ctx.Err(); err != nil {
			return failed(err)
		}
		st, err := os.Lstat(source.Path)
		if err != nil {
			return failed(err)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return failed(Fail("invalid_input", "cannot archive a symlink Agent home or configuration: %s", source.Path))
		}
		parent, err := os.OpenRoot(filepath.Dir(source.Path))
		if err != nil {
			return failed(err)
		}
		base := filepath.Base(source.Path)
		err = fs.WalkDir(parent.FS(), base, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if e := ctx.Err(); e != nil {
				return e
			}
			info, e := d.Info()
			if e != nil {
				return e
			}
			if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
				return Fail("invalid_input", "legacy knowledge contains a link or special file: %s", filepath.Join(filepath.Dir(source.Path), path))
			}
			header, e := tar.FileInfoHeader(info, "")
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(base, path)
			if e != nil {
				return e
			}
			header.Name = source.Entry
			if rel != "." {
				header.Name += "/" + filepath.ToSlash(rel)
			}
			if info.IsDir() {
				header.Mode = 0700
			} else {
				header.Mode = 0600
			}
			if e = tw.WriteHeader(header); e != nil {
				return e
			}
			if info.IsDir() {
				archive.Entries = append(archive.Entries, knowledgeArchiveEntry{Name: header.Name, Type: "directory"})
				return nil
			}
			input, e := parent.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
			if e != nil {
				return e
			}
			actual, e := input.Stat()
			if e != nil {
				input.Close()
				return e
			}
			if !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
				input.Close()
				return Fail("conflict", "legacy knowledge changed during archive")
			}
			hash := sha256.New()
			_, copyErr := io.CopyN(io.MultiWriter(tw, hash), input, info.Size())
			after, statErr := input.Stat()
			closeErr := input.Close()
			if e = errors.Join(copyErr, statErr, closeErr); e != nil {
				return e
			}
			if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
				return Fail("conflict", "legacy knowledge changed during archive")
			}
			archive.Entries = append(archive.Entries, knowledgeArchiveEntry{Name: header.Name, Type: "file", Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))})
			return nil
		})
		parent.Close()
		if err != nil {
			return failed(err)
		}
	}
	manifest, _ := json.Marshal(archive)
	if err = tw.WriteHeader(&tar.Header{Name: "archive-manifest.json", Mode: 0600, Size: int64(len(manifest))}); err != nil {
		return failed(err)
	}
	if _, err = tw.Write(manifest); err != nil {
		return failed(err)
	}
	if err = tw.Close(); err != nil {
		file.Close()
		return err
	}
	if err = errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	currentDBHash, _, err := fileHash(backup.Path)
	if err != nil {
		return err
	}
	if currentDBHash != backup.SHA256 {
		return Fail("conflict", "database archive changed while preserving legacy knowledge")
	}
	if err = verifyLegacyKnowledgeArchive(ctx, temp, backup.SHA256, &archive); err != nil {
		return err
	}
	sha, _, err := fileHash(temp)
	if err != nil {
		return err
	}
	if err = os.Link(temp, destination); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(destination)); err != nil {
		return err
	}
	backup.KnowledgePath, backup.KnowledgeSHA256 = destination, sha
	return writeArchiveMetadata(*backup)
}
