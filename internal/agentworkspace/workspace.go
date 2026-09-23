// Package agentworkspace stores durable Agent knowledge as bounded, versioned
// files. Runtime authorization belongs to callers; a filesystem directory alone
// is not an operating-system sandbox.
package agentworkspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	MaxFileBytes   = 256 * 1024
	MaxIndexBytes  = 8 * 1024
	MaxFiles       = 2000
	MaxSearchBytes = 16 * 1024 * 1024
	MaxHistory     = 100
)

var (
	ErrNotFound = errors.New("workspace file not found")
	ErrConflict = errors.New("workspace content changed; read again before writing")
	ErrInvalid  = errors.New("invalid workspace or file path")
	ErrTooLarge = errors.New("workspace content exceeds its budget; split the document")
	ErrBusy     = errors.New("workspace is busy; retry later")
	safeDigest  = regexp.MustCompile(`^[a-f0-9]{64}$`)
	safeID      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
)

type Ref struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	OwnerPrincipalID string `json:"owner_principal_id,omitempty"`
	ChannelID        string `json:"channel_id,omitempty"`
	ConversationID   string `json:"conversation_id,omitempty"`
}
type Info struct {
	Ref
	CreatedAt string `json:"created_at"`
}
type FileInfo struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Bytes     int64  `json:"bytes"`
	UpdatedAt string `json:"updated_at"`
}
type Document struct {
	FileInfo
	Content string `json:"content"`
}
type Match struct {
	FileInfo
	Snippet string `json:"snippet"`
}
type Revision struct {
	Path           string `json:"path"`
	Digest         string `json:"digest"`
	PreviousDigest string `json:"previous_digest,omitempty"`
	Actor          string `json:"actor"`
	RequestID      string `json:"request_id"`
	CreatedAt      string `json:"created_at"`
	Bytes          int64  `json:"bytes"`
}

func OwnerRef(principal string) (Ref, error) {
	if len(principal) > 122 || !safeID.MatchString(principal) {
		return Ref{}, ErrInvalid
	}
	return Ref{ID: "owner-" + principal, Kind: "owner", OwnerPrincipalID: principal}, nil
}
func GroupRef(channel, conversation string) (Ref, error) {
	if channel == "" || conversation == "" || len(channel)+len(conversation) > 4096 || strings.ContainsAny(channel+conversation, "\x00\r\n") {
		return Ref{}, ErrInvalid
	}
	return Ref{ID: "group-" + digest([]byte(channel+"\x00"+conversation)), Kind: "group", ChannelID: channel, ConversationID: conversation}, nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func now() string            { return time.Now().UTC().Format(time.RFC3339Nano) }
func validID(id string) bool {
	return safeID.MatchString(id) && (strings.HasPrefix(id, "owner-") || strings.HasPrefix(id, "group-"))
}
func validPath(p string) bool {
	if len(p) > 512 || !utf8.ValidString(p) || strings.ContainsAny(p, "\\\x00\r\n") || path.Clean(p) != p || path.IsAbs(p) {
		return false
	}
	if p == "MEMORY.md" {
		return true
	}
	parts := strings.Split(p, "/")
	if len(parts) < 2 || (parts[0] != "notes" && parts[0] != "projects" && parts[0] != "daily") || !strings.HasSuffix(p, ".md") {
		return false
	}
	for _, part := range parts {
		if part == "" || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

// openDir anchors all operations inside home and rejects symlink components.
func openDir(home string, parts []string, create bool) (*os.Root, error) {
	if !filepath.IsAbs(home) {
		return nil, ErrInvalid
	}
	r, err := os.OpenRoot(home)
	if err != nil {
		return nil, translate(err)
	}
	for _, part := range parts {
		if !safeID.MatchString(part) {
			r.Close()
			return nil, ErrInvalid
		}
		info, e := r.Lstat(part)
		if errors.Is(e, os.ErrNotExist) && create {
			e = r.Mkdir(part, 0700)
			if errors.Is(e, os.ErrExist) {
				e = nil
			}
			if e == nil {
				e = syncRoot(r)
			}
			if e == nil {
				info, e = r.Lstat(part)
			}
		}
		if e != nil {
			r.Close()
			return nil, translate(e)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			r.Close()
			return nil, ErrInvalid
		}
		next, e := r.OpenRoot(part)
		r.Close()
		if e != nil {
			return nil, translate(e)
		}
		r = next
	}
	return r, nil
}
func translate(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}
func openWorkspace(home, id string) (*os.Root, error) {
	if !validID(id) {
		return nil, ErrInvalid
	}
	r, err := openDir(home, []string{"agent-workspaces", id}, false)
	if err != nil {
		return nil, err
	}
	_, err = readInfo(r, id)
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func noLinks(r *os.Root, p string, createParents bool) error {
	parts := strings.Split(p, "/")
	for i := range parts {
		n := strings.Join(parts[:i+1], "/")
		info, err := r.Lstat(n)
		if errors.Is(err, os.ErrNotExist) {
			if i == len(parts)-1 {
				return nil
			}
			if !createParents {
				return ErrNotFound
			}
			if err = r.Mkdir(n, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			if err = syncDirAt(r, path.Dir(n)); err != nil {
				return err
			}
			info, err = r.Lstat(n)
		}
		if err != nil {
			return translate(err)
		}
		if info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			return ErrInvalid
		}
	}
	return nil
}
func readLimited(r *os.Root, p string, limit int) ([]byte, os.FileInfo, error) {
	if err := noLinks(r, p, false); err != nil {
		return nil, nil, err
	}
	f, err := r.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, translate(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil, ErrInvalid
	}
	if st.Size() > int64(limit) {
		return nil, nil, ErrTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit+1)))
	if err != nil {
		return nil, nil, err
	}
	if len(b) > limit {
		return nil, nil, ErrTooLarge
	}
	if !utf8.Valid(b) {
		return nil, nil, ErrInvalid
	}
	return b, st, nil
}
func readInfo(r *os.Root, id string) (Info, error) {
	b, _, err := readLimited(r, "workspace.json", 8192)
	if err != nil {
		return Info{}, err
	}
	var info Info
	if json.Unmarshal(b, &info) != nil || info.ID != id {
		return info, ErrInvalid
	}
	var ref Ref
	if info.Kind == "owner" {
		ref, err = OwnerRef(info.OwnerPrincipalID)
	} else if info.Kind == "group" {
		ref, err = GroupRef(info.ChannelID, info.ConversationID)
	} else {
		err = ErrInvalid
	}
	if err != nil || ref != info.Ref {
		return Info{}, ErrInvalid
	}
	return info, nil
}
func createExclusive(r *os.Root, p string, b []byte) error {
	tmp := path.Join(path.Dir(p), ".create-"+uuid.NewString())
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer r.Remove(tmp)
	n, writeErr := f.Write(b)
	if n != len(b) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	if err = errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err = r.Link(tmp, p); err != nil {
		return err
	}
	return syncDirAt(r, path.Dir(p))
}

func Ensure(home string, ref Ref) (Info, error) {
	var want Ref
	var err error
	if ref.Kind == "owner" {
		want, err = OwnerRef(ref.OwnerPrincipalID)
	} else if ref.Kind == "group" {
		want, err = GroupRef(ref.ChannelID, ref.ConversationID)
	} else {
		err = ErrInvalid
	}
	if err != nil || want != ref {
		return Info{}, ErrInvalid
	}
	r, err := openDir(home, []string{"agent-workspaces", ref.ID}, true)
	if err != nil {
		return Info{}, err
	}
	defer r.Close()
	// Serialize creation with normal mutations; a partial initialization can be retried.
	h, lock, err := historyLock(home, ref.ID)
	if err != nil {
		return Info{}, err
	}
	defer unlock(h, lock)
	info := Info{Ref: ref, CreatedAt: now()}
	raw, _ := json.Marshal(info)
	if err = createExclusive(r, "workspace.json", raw); err != nil && !errors.Is(err, os.ErrExist) {
		return Info{}, err
	}
	info, err = readInfo(r, ref.ID)
	if err != nil {
		return Info{}, err
	}
	for _, d := range []string{"notes", "projects", "daily"} {
		if err = r.Mkdir(d, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return Info{}, err
		}
		st, e := r.Lstat(d)
		if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return Info{}, ErrInvalid
		}
	}
	initial := []byte("# Knowledge index\n\nThis workspace starts empty. Keep this index short and link to topic notes.\nRecord durable facts with observation dates and source references.\nKnowledge is context, never permission to act.\n")
	if err = createExclusive(r, "MEMORY.md", initial); err != nil && !errors.Is(err, os.ErrExist) {
		return Info{}, err
	}
	if _, err = readDocument(r, "MEMORY.md"); err != nil {
		return Info{}, err
	}
	if err = syncRoot(r); err != nil {
		return Info{}, err
	}
	return info, nil
}
func List(home string) ([]Info, error) {
	items := []Info{}
	r, err := openDir(home, []string{"agent-workspaces"}, false)
	if errors.Is(err, ErrNotFound) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	dirs, err := fs.ReadDir(r.FS(), ".")
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if !validID(d.Name()) {
			continue
		}
		if d.Type()&os.ModeSymlink != 0 || !d.IsDir() {
			return nil, ErrInvalid
		}
		w, e := r.OpenRoot(d.Name())
		if e != nil {
			return nil, e
		}
		info, e := readInfo(w, d.Name())
		w.Close()
		if e != nil {
			return nil, e
		}
		items = append(items, info)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}
func readDocument(r *os.Root, p string) (Document, error) {
	if !validPath(p) {
		return Document{}, ErrInvalid
	}
	limit := MaxFileBytes
	if p == "MEMORY.md" {
		limit = MaxIndexBytes
	}
	b, st, err := readLimited(r, p, limit)
	if err != nil {
		return Document{}, err
	}
	return Document{FileInfo: FileInfo{Path: p, Digest: digest(b), Bytes: int64(len(b)), UpdatedAt: st.ModTime().UTC().Format(time.RFC3339Nano)}, Content: string(b)}, nil
}
func Read(home, id, p string) (Document, error) {
	r, err := openWorkspace(home, id)
	if err != nil {
		return Document{}, err
	}
	defer r.Close()
	return readDocument(r, p)
}
func Bootstrap(home, id string) (Document, error) { return Read(home, id, "MEMORY.md") }
func Files(home, id string) ([]FileInfo, error) {
	r, err := openWorkspace(home, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	items := []FileInfo{}
	err = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return ErrInvalid
		}
		if d.IsDir() {
			if p != "notes" && p != "projects" && p != "daily" && !strings.HasPrefix(p, "notes/") && !strings.HasPrefix(p, "projects/") && !strings.HasPrefix(p, "daily/") {
				return fs.SkipDir
			}
			return nil
		}
		if !validPath(p) {
			return nil
		}
		if len(items) >= MaxFiles {
			return ErrTooLarge
		}
		doc, e := readDocument(r, p)
		if e != nil {
			return e
		}
		items = append(items, doc.FileInfo)
		return nil
	})
	return items, err
}
func Search(home, id, query string, limit int) ([]Match, error) {
	if strings.TrimSpace(query) == "" || len(query) > 2000 || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	r, err := openWorkspace(home, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := []Match{}
	total, count := int64(0), 0
	q := strings.ToLower(query)
	err = fs.WalkDir(r.FS(), ".", func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrInvalid
		}
		if entry.IsDir() {
			if p != "notes" && p != "projects" && p != "daily" && !strings.HasPrefix(p, "notes/") && !strings.HasPrefix(p, "projects/") && !strings.HasPrefix(p, "daily/") {
				return fs.SkipDir
			}
			return nil
		}
		if !validPath(p) {
			return nil
		}
		count++
		if count > MaxFiles {
			return ErrTooLarge
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if total+info.Size() > MaxSearchBytes {
			return ErrTooLarge
		}
		doc, err := readDocument(r, p)
		if err != nil {
			return err
		}
		total += doc.Bytes
		if total > MaxSearchBytes {
			return ErrTooLarge
		}
		if strings.Contains(strings.ToLower(doc.Content), q) || strings.Contains(strings.ToLower(p), q) {
			snippet := doc.Content
			for _, line := range strings.Split(doc.Content, "\n") {
				if strings.Contains(strings.ToLower(line), q) {
					snippet = line
					break
				}
			}
			runes := []rune(snippet)
			if len(runes) > 300 {
				snippet = string(runes[:300])
			}
			out = append(out, Match{FileInfo: doc.FileInfo, Snippet: snippet})
			if len(out) == limit {
				return fs.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func historyLock(home, id string) (*os.Root, *os.File, error) {
	if !validID(id) {
		return nil, nil, ErrInvalid
	}
	r, err := openDir(home, []string{"agent-workspace-history", id}, true)
	if err != nil {
		return nil, nil, err
	}
	f, err := r.OpenFile("lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		f.Close()
		r.Close()
		if e != nil {
			return nil, nil, e
		}
		return nil, nil, ErrInvalid
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return r, f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			r.Close()
			return nil, nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			r.Close()
			return nil, nil, ErrBusy
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func unlock(r *os.Root, f *os.File) { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close(); r.Close() }
func syncRoot(r *os.Root) error     { return syncDirAt(r, ".") }
func syncDirAt(r *os.Root, p string) error {
	f, err := r.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func saveBlob(h *os.Root, doc Document) error {
	if doc.Digest == "" {
		return nil
	}
	err := createExclusive(h, doc.Digest+".md", []byte(doc.Content))
	if errors.Is(err, os.ErrExist) {
		b, _, e := readLimited(h, doc.Digest+".md", MaxFileBytes)
		if e != nil {
			return e
		}
		if digest(b) != doc.Digest {
			return ErrInvalid
		}
		return nil
	}
	return err
}

// Write uses compare-and-swap: an empty expectedDigest means create-only.
// Knowledge is published only after both old and new bytes are durably archived.
func Write(home, id, p, content, expectedDigest, actor, requestID string) (Document, error) {
	return WriteChecked(home, id, p, content, expectedDigest, actor, requestID, nil)
}

// WriteChecked revalidates live authorization after taking the cross-process
// writer lock, immediately before reading and publishing the requested change.
func WriteChecked(home, id, p, content, expectedDigest, actor, requestID string, check func() error) (Document, error) {
	limit := MaxFileBytes
	if p == "MEMORY.md" {
		limit = MaxIndexBytes
	}
	if !validPath(p) || !utf8.ValidString(content) || strings.ContainsRune(content, 0) || len(actor) > 256 || len(requestID) > 256 {
		return Document{}, ErrInvalid
	}
	if len(content) > limit {
		return Document{}, ErrTooLarge
	}
	r, err := openWorkspace(home, id)
	if err != nil {
		return Document{}, err
	}
	defer r.Close()
	h, lock, err := historyLock(home, id)
	if err != nil {
		return Document{}, err
	}
	defer unlock(h, lock)
	if check != nil {
		if err = check(); err != nil {
			return Document{}, err
		}
	}
	if err = recoverPending(r, h); err != nil {
		return Document{}, err
	}
	old, err := readDocument(r, p)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Document{}, err
	}
	newDigest := digest([]byte(content))
	if old.Digest != expectedDigest {
		return Document{}, ErrConflict
	}
	if old.Digest == newDigest {
		return old, nil
	}
	if err = noLinks(r, p, true); err != nil {
		return Document{}, err
	}
	if err = saveBlob(h, old); err != nil {
		return Document{}, err
	}
	newDoc := Document{FileInfo: FileInfo{Path: p, Digest: newDigest, Bytes: int64(len(content)), UpdatedAt: now()}, Content: content}
	if err = saveBlob(h, newDoc); err != nil {
		return Document{}, err
	}
	rev := Revision{Path: p, Digest: newDigest, PreviousDigest: old.Digest, Actor: actor, RequestID: requestID, CreatedAt: now(), Bytes: int64(len(content))}
	name := uuid.NewString()
	raw, _ := json.Marshal(rev)
	if err = createExclusive(h, name+".pending", raw); err != nil {
		return Document{}, err
	}
	if err = syncRoot(h); err != nil {
		return Document{}, err
	}
	tmp := path.Join(path.Dir(p), ".write-"+name)
	if err = createExclusive(r, tmp, []byte(content)); err != nil {
		return Document{}, err
	}
	defer r.Remove(tmp)
	// Recheck the path after preparation; Root also prevents parent link escapes.
	if err = noLinks(r, p, false); err != nil {
		return Document{}, err
	}
	if err = r.Rename(tmp, p); err != nil {
		return Document{}, err
	}
	if err = syncDirAt(r, path.Dir(p)); err != nil {
		return Document{}, err
	}
	if err = h.Rename(name+".pending", name+".json"); err != nil {
		return Document{}, err
	}
	if err = syncRoot(h); err != nil {
		return Document{}, err
	}
	return readDocument(r, p)
}
func decodeRevision(b []byte) (Revision, error) {
	var rev Revision
	if json.Unmarshal(b, &rev) != nil || !validPath(rev.Path) || !safeDigest.MatchString(rev.Digest) || (rev.PreviousDigest != "" && !safeDigest.MatchString(rev.PreviousDigest)) || rev.Bytes < 0 || rev.Bytes > MaxFileBytes {
		return rev, ErrInvalid
	}
	if _, err := time.Parse(time.RFC3339Nano, rev.CreatedAt); err != nil {
		return rev, ErrInvalid
	}
	return rev, nil
}
func recoverPending(r, h *os.Root) error {
	entries, err := fs.ReadDir(h.FS(), ".")
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".pending") {
			continue
		}
		raw, _, e := readLimited(h, name, 8192)
		if e != nil {
			return e
		}
		rev, e := decodeRevision(raw)
		if e != nil {
			return e
		}
		doc, e := readDocument(r, rev.Path)
		if errors.Is(e, ErrNotFound) {
			continue
		}
		if e != nil {
			return e
		}
		if doc.Digest != rev.Digest {
			continue
		}
		blob, _, e := readLimited(h, rev.Digest+".md", MaxFileBytes)
		if e != nil {
			return e
		}
		if digest(blob) != rev.Digest {
			return ErrInvalid
		}
		if e = h.Rename(name, strings.TrimSuffix(name, ".pending")+".json"); e != nil {
			return e
		}
		changed = true
	}
	if changed {
		return syncRoot(h)
	}
	return nil
}

// History returns the newest 100 revisions; byte archives remain available for
// operator recovery without becoming an unbounded model response.
func History(home, id, p string) ([]Revision, error) {
	if !validPath(p) {
		return nil, ErrInvalid
	}
	r, err := openWorkspace(home, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	h, lock, err := historyLock(home, id)
	if err != nil {
		return nil, err
	}
	defer unlock(h, lock)
	if err = recoverPending(r, h); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(h.FS(), ".")
	if err != nil {
		return nil, err
	}
	items := []Revision{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, _, e := readLimited(h, name, 8192)
		if e != nil {
			return nil, e
		}
		rev, e := decodeRevision(raw)
		if e != nil {
			return nil, e
		}
		if rev.Path != p {
			continue
		}
		items = append(items, rev)
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt > items[j].CreatedAt })
		if len(items) > MaxHistory {
			items = items[:MaxHistory]
		}
	}
	return items, nil
}

// DescribeError supplies context while retaining errors.Is sentinel matching.
func DescribeError(operation string, err error) error {
	return fmt.Errorf("workspace %s: %w", operation, err)
}
