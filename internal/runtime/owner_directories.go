package runtime

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/processtree"
)

// OwnerDirectoryWorkspace isolates declared input roots from mutable task data.
// Code tasks use a private local clone and its worktree, retaining ancestry but
// never giving a model a checkout connected to the owner's original Git files.
type OwnerDirectoryWorkspace struct {
	WorkDir    string
	Branch     string
	Base       string
	Snapshots  []DirectorySnapshot
	ReadRoots  []string
	WriteRoots []string
}

func pathInRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func validateOwnerDirectoryPolicy(policy core.RuntimeAgentPolicy) error {
	if len(policy.Directories) == 0 {
		return nil
	}
	if !hasAgentCapability(policy.Capabilities, "local_read") {
		return core.Fail("denied", "declared owner directories require local_read")
	}
	// A command allowlist cannot contain filesystem effects of arbitrary tests.
	// Until a verified OS sandbox is available, do not silently run them outside
	// the copied filesystem boundary.
	if hasAgentCapability(policy.Capabilities, "local_test") {
		return core.Fail("unavailable", "declared-directory shell tests require a verified OS sandbox")
	}
	return nil
}
func prepareOwnerDirectories(ctx context.Context, home string, task core.RuntimeTask, attemptID string, w core.Workspace, policy core.RuntimeAgentPolicy) (OwnerDirectoryWorkspace, error) {
	var out OwnerDirectoryWorkspace
	if err := validateOwnerDirectoryPolicy(policy); err != nil {
		return out, err
	}
	roots := make([]string, 0, len(policy.Directories))
	for _, root := range policy.Directories {
		if !filepath.IsAbs(root) {
			return out, core.Fail("denied", "owner directory must be absolute")
		}
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			return out, core.Fail("unavailable", "owner directory does not exist")
		}
		if real != filepath.Clean(root) {
			return out, core.Fail("denied", "owner directory cannot use symbolic links")
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() {
			return out, core.Fail("denied", "owner input must be a directory")
		}
		roots = append(roots, real)
	}
	admitted := func(path string) bool {
		for _, r := range roots {
			if pathInRoot(path, r) {
				return true
			}
		}
		return false
	}
	var repository string
	if w.Path != "" {
		path, err := filepath.EvalSymlinks(w.Path)
		if err != nil {
			return out, err
		}
		if path != filepath.Clean(w.Path) || !admitted(path) {
			return out, core.Fail("denied", "workspace lies outside declared owner directories")
		}
		raw, err := isolatedGit(ctx, path, "rev-parse", "--show-toplevel")
		if err == nil {
			repository = strings.TrimSpace(string(raw))
			if !admitted(repository) {
				return out, core.Fail("denied", "repository root extends beyond declared owner directory")
			}
			gitDir, err := isolatedGit(ctx, path, "rev-parse", "--absolute-git-dir")
			if err != nil {
				return out, err
			}
			if !admitted(strings.TrimSpace(string(gitDir))) {
				return out, core.Fail("denied", "repository metadata lies outside declared owner directory")
			}
			if err := validateCloneMetadata(strings.TrimSpace(string(gitDir))); err != nil {
				return out, err
			}

		}
	}
	baseDir := filepath.Join(home, "runtime", "tasks", task.ID, attemptID)
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return out, err
	}
	baseDir, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return out, err
	}
	// This bounded rooted read is the only code that reads declared source roots.
	// It also rejects symlinks/special files before a clone or model is started.
	snapshots, err := stageGroupDirectories(ctx, baseDir, roots)
	if err != nil {
		return out, err
	}
	out.ReadRoots = []string{filepath.Join(baseDir, "inputs")}
	if repository == "" {
		out.WorkDir = baseDir
		for _, snapshot := range snapshots {
			// Mutable copies are distinct from the retained read-only evidence snapshot.
			rel := strings.TrimPrefix(snapshot.Path, "inputs/")
			path := filepath.Join(baseDir, "work", filepath.FromSlash(rel))
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return out, err
			}
			if err = os.WriteFile(path, []byte(snapshot.Content), 0600); err != nil {
				return out, err
			}
			out.Snapshots = append(out.Snapshots, DirectorySnapshot{Path: filepath.ToSlash(filepath.Join("work", rel)), Content: snapshot.Content})
		}
		if hasAgentCapability(policy.Capabilities, "local_write") {
			out.WriteRoots = append(out.WriteRoots, filepath.Join(baseDir, "work"))
		}
	} else {
		// Local cloning preserves source commit ancestry and does not inherit source
		// hooks or local Git configuration. Disable hardlinks and alternates so the
		// owner's objects remain read-only from the task's perspective.
		clone := filepath.Join(baseDir, "repository")
		if _, err = isolatedGit(ctx, baseDir, "clone", "--no-hardlinks", "--no-checkout", "--", repository, clone); err != nil {
			return out, core.Fail("unavailable", "could not create isolated directory repository")
		}
		raw, err := isolatedGit(ctx, clone, "rev-parse", "HEAD")
		if err != nil {
			return out, err
		}
		out.Base = strings.TrimSpace(string(raw))
		out.Branch = "codex/declared-" + strings.ReplaceAll(attemptID, "-", "")
		out.WorkDir = filepath.Join(baseDir, "worktree")
		if _, err = isolatedGit(ctx, clone, "worktree", "add", "-b", out.Branch, out.WorkDir, "HEAD"); err != nil {
			return out, core.Fail("unavailable", "could not create declared-directory worktree")
		}
		if err := auditOwnerTree(out.WorkDir); err != nil {
			return out, err
		}
		// Read only files from the isolated checkout into model context. The .git
		// control file is hidden and is neither supplied nor writable by the model.
		checkoutSnapshots, err := stageGroupDirectories(ctx, filepath.Join(baseDir, "checkout-context"), []string{out.WorkDir})
		if err != nil {
			return out, err
		}
		for _, snapshot := range checkoutSnapshots {
			rel := strings.TrimPrefix(snapshot.Path, "inputs/001/")
			out.Snapshots = append(out.Snapshots, DirectorySnapshot{Path: rel, Content: snapshot.Content})
		}
		// Other admitted roots are read-only context, never mutable source mounts.
		for _, snapshot := range snapshots {
			parts := strings.SplitN(snapshot.Path, "/", 3)
			if len(parts) == 3 {
				index, _ := strconv.Atoi(parts[1])
				if index > 0 && index <= len(roots) && pathInRoot(filepath.Join(roots[index-1], filepath.FromSlash(parts[2])), repository) {
					continue
				}
			}
			out.Snapshots = append(out.Snapshots, DirectorySnapshot{Path: snapshot.Path, Content: snapshot.Content})
		}
		if hasAgentCapability(policy.Capabilities, "local_write") {
			out.WriteRoots = append(out.WriteRoots, out.WorkDir)
		}
	}
	totalSnapshotBytes := 0
	for _, snapshot := range out.Snapshots {
		totalSnapshotBytes += len(snapshot.Content)
	}
	if len(out.Snapshots) > snapshotMaxFiles || totalSnapshotBytes > snapshotMaxBytes {
		return out, core.Fail("denied", "combined owner directory snapshots exceed task limit")
	}
	if err = os.MkdirAll(filepath.Join(out.WorkDir, "artifacts"), 0700); err != nil {
		return out, err
	}
	if hasAgentCapability(policy.Capabilities, "artifact_create") {
		out.WriteRoots = append(out.WriteRoots, filepath.Join(out.WorkDir, "artifacts"))
	}
	return out, nil
}

// isolatedGit runs fixed backend operations against the private clone. Git's
// global/system config and inherited environment can redirect paths or execute
// helpers, so the isolated repository uses a minimal local execution context.
func isolatedGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=memgov", "GIT_AUTHOR_EMAIL=memgov@localhost", "GIT_COMMITTER_NAME=memgov", "GIT_COMMITTER_EMAIL=memgov@localhost"}
	return processtree.Output(ctx, cmd)
}
func finishOwnerDirectories(ctx context.Context, w OwnerDirectoryWorkspace, policy core.RuntimeAgentPolicy, artifacts []string) ([]string, string, error) {
	if err := auditOwnerTree(w.WorkDir); err != nil {
		return nil, "", err
	}
	products, err := validateOwnerArtifacts(w.WorkDir, w.WriteRoots, artifacts)
	if err != nil {
		return nil, "", err
	}
	if w.Branch == "" {
		return products, "", nil
	}
	if _, err = isolatedGit(ctx, w.WorkDir, "diff", "--check"); err != nil {
		return nil, "", core.Fail("conflict", "declared-directory changes failed Git diff validation")
	}
	raw, err := isolatedGit(ctx, w.WorkDir, "status", "--porcelain")
	if err != nil {
		return nil, "", err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return products, "", nil
	}
	if !hasAgentCapability(policy.Capabilities, "local_write") && !hasAgentCapability(policy.Capabilities, "artifact_create") {
		return nil, "", core.Fail("denied", "read-only directory task changed files")
	}
	if _, err = isolatedGit(ctx, w.WorkDir, "add", "--all"); err != nil {
		return nil, "", err
	}
	if _, err = isolatedGit(ctx, w.WorkDir, "commit", "-m", "Complete declared-directory task"); err != nil {
		return nil, "", core.Fail("unavailable", "could not commit isolated directory task")
	}
	raw, err = isolatedGit(ctx, w.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, "", err
	}
	commit := strings.TrimSpace(string(raw))
	if commit == w.Base {
		return nil, "", core.Fail("conflict", "directory task commit did not advance")
	}
	return products, commit, nil
}
func validateOwnerArtifacts(workdir string, roots, artifacts []string) ([]string, error) {
	out := []string{}
	for _, path := range artifacts {
		full := path
		if !filepath.IsAbs(full) {
			full = filepath.Join(workdir, path)
		}
		allowed := ""
		for _, root := range roots {
			if pathInRoot(full, root) {
				allowed = root
				break
			}
		}
		if allowed == "" {
			return nil, core.Fail("denied", "owner artifact lies outside task write roots")
		}
		rel, err := filepath.Rel(allowed, full)
		if err != nil || rel == "." {
			return nil, core.Fail("denied", "owner artifact must name a file")
		}
		root, err := os.OpenRoot(allowed)
		if err != nil {
			return nil, err
		}
		current := ""
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			current = filepath.Join(current, part)
			info, e := root.Lstat(current)
			if e != nil || info.Mode()&os.ModeSymlink != 0 {
				root.Close()
				return nil, core.Fail("denied", "owner artifact contains a symbolic link")
			}
		}
		f, err := root.Open(rel)
		if err != nil {
			root.Close()
			return nil, err
		}
		info, err := f.Stat()
		f.Close()
		root.Close()
		if err != nil || !info.Mode().IsRegular() {
			return nil, core.Fail("denied", "owner artifact must be a regular file")
		}
		out = append(out, full)
	}
	return out, nil
}
func ownerDirectoryTools(in ExecutionInput) []string {
	out := []string{"Read(./**)"}
	for _, root := range in.DirectoryWriteRoots {
		rel, err := filepath.Rel(in.WorkDir, root)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if rel == "." {
			out = append(out, "Edit(./**)")
		} else {
			out = append(out, fmt.Sprintf("Edit(./%s/**)", filepath.ToSlash(rel)))
		}
	}
	return out
}

// A local clone must not borrow object stores outside the declared repository.
func validateCloneMetadata(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	var total int64
	count := 0
	return fs.WalkDir(root.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return core.Fail("denied", "repository metadata cannot be inspected")
		}
		count++
		if count > 100000 {
			return core.Fail("denied", "repository metadata exceeds entry limit")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return core.Fail("denied", "repository metadata cannot use symbolic links")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return core.Fail("denied", "repository metadata requires regular files")
		}
		if (rel == "objects/info/alternates" || rel == "commondir") && info.Size() > 0 {
			return core.Fail("denied", "repository borrows metadata outside its declared root")
		}
		total += info.Size()
		if total > 256*1024*1024 {
			return core.Fail("denied", "repository metadata exceeds isolated clone limit")
		}
		return nil
	})
}

// Unlike prompt snapshot collection, this audit never skips hidden paths: a
// tracked link anywhere in a checkout must not turn a scoped Read into a host
// read, even when the linked file is absent from the initial model context.
func auditOwnerTree(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	count := 0
	return fs.WalkDir(root.FS(), ".", func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return core.Fail("denied", "isolated task tree cannot be inspected")
		}
		count++
		if count > 100000 {
			return core.Fail("denied", "isolated task tree exceeds entry limit")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return core.Fail("denied", "isolated task tree contains symbolic link")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return core.Fail("denied", "isolated task tree contains a special file")
		}
		return nil
	})
}
