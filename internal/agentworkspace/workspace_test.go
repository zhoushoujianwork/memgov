package agentworkspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) (string, Info) {
	t.Helper()
	home := t.TempDir()
	ref, err := OwnerRef("owner-one")
	if err != nil {
		t.Fatal(err)
	}
	info, err := Ensure(home, ref)
	if err != nil {
		t.Fatal(err)
	}
	return home, info
}
func TestOwnerContinuityAndGroupIsolation(t *testing.T) {
	home, owner := fixture(t)
	doc, err := Write(home, owner.ID, "notes/project.md", "Observed 2026-09-23. Source: task:test.\nUse staging first.", "", "agent", "request1")
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := OwnerRef("owner-one")
	again, err := Ensure(home, ref)
	if err != nil || again.ID != owner.ID {
		t.Fatalf("owner workspace changed: %v", err)
	}
	got, err := Read(home, again.ID, doc.Path)
	if err != nil || got.Content != doc.Content {
		t.Fatalf("knowledge lost across sessions: %+v %v", got, err)
	}
	for _, conversation := range []string{"group-a", "group-b"} {
		ref, _ := GroupRef("channel", conversation)
		group, err := Ensure(home, ref)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Read(home, group.ID, doc.Path); !errors.Is(err, ErrNotFound) {
			t.Fatalf("owner file leaked into group: %v", err)
		}
		if _, err = Write(home, group.ID, "notes/group.md", conversation, "", "group", "request"); err != nil {
			t.Fatal(err)
		}
	}
	groups, err := List(home)
	if err != nil || len(groups) != 3 {
		t.Fatalf("workspace list %v %v", groups, err)
	}
	for _, g := range groups {
		if g.Kind == "group" {
			d, e := Read(home, g.ID, "notes/group.md")
			if e != nil || d.Content != g.ConversationID {
				t.Fatal("groups share files", e)
			}
		}
	}
}

func TestSearchReadsOnlyItsBudgetAndStopsAtLimit(t *testing.T) {
	home, info := fixture(t)
	root := filepath.Join(home, "agent-workspaces", info.ID, "notes")
	if err := os.WriteFile(filepath.Join(root, "first.md"), []byte("needle"), 0600); err != nil {
		t.Fatal(err)
	}
	// An unread invalid later document must not break a bounded one-hit search.
	if err := os.WriteFile(filepath.Join(root, "later.md"), []byte{0xff}, 0600); err != nil {
		t.Fatal(err)
	}
	if hits, err := Search(home, info.ID, "needle", 1); err != nil || len(hits) != 1 {
		t.Fatal(hits, err)
	}
	if err := os.Remove(filepath.Join(root, "later.md")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxSearchBytes/MaxFileBytes; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("budget-%03d.md", i)), []byte(strings.Repeat("x", MaxFileBytes)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Search(home, info.ID, "not-present", 1); !errors.Is(err, ErrTooLarge) {
		t.Fatal("search did not enforce aggregate budget", err)
	}
}
func TestConcurrentCASAndVersionHistory(t *testing.T) {
	home, info := fixture(t)
	first, err := Write(home, info.ID, "notes/concurrent.md", "original", "", "owner", "first")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, content := range []string{"one", "two"} {
		wg.Add(1)
		go func(content string) {
			defer wg.Done()
			<-start
			_, err := Write(home, info.ID, first.Path, content, first.Digest, "agent", content)
			results <- err
		}(content)
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("lost update: success=%d conflict=%d", success, conflict)
	}
	revisions, err := History(home, info.ID, first.Path)
	if err != nil || len(revisions) != 2 {
		t.Fatalf("history missing %v %v", revisions, err)
	}
	if revisions[0].PreviousDigest != first.Digest {
		t.Fatal("old version not linked")
	}
	old, err := os.ReadFile(filepath.Join(home, "agent-workspace-history", info.ID, first.Digest+".md"))
	if err != nil || string(old) != "original" {
		t.Fatal("old bytes missing", err)
	}
	files, err := Files(home, info.ID)
	if err != nil || len(files) != 2 {
		t.Fatalf("history entered file index %v %v", files, err)
	}
}
func TestPathAndBudgetBoundaries(t *testing.T) {
	home, info := fixture(t)
	for _, p := range []string{"../other.md", "/tmp/escape.md", "notes/../../escape.md", "notes/.hidden.md", "CLAUDE.md", "workspace.json", "notes/file.txt", "notes\\escape.md"} {
		if _, err := Write(home, info.ID, p, "bad", "", "agent", "req"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted %q: %v", p, err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "agent-workspaces", info.ID, "notes", "link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(home, info.ID, "notes/link.md"); !errors.Is(err, ErrInvalid) {
		t.Fatal("symlink read accepted", err)
	}
	if _, err := Write(home, info.ID, "notes/link.md", "overwrite", "", "agent", "req"); !errors.Is(err, ErrInvalid) {
		t.Fatal("symlink write accepted", err)
	}
	if _, err := Files(home, info.ID); !errors.Is(err, ErrInvalid) {
		t.Fatal("symlink indexed", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	index, _ := Bootstrap(home, info.ID)
	if _, err := Write(home, info.ID, "MEMORY.md", strings.Repeat("a", MaxIndexBytes+1), index.Digest, "agent", "req"); !errors.Is(err, ErrTooLarge) {
		t.Fatal("unbounded index", err)
	}
	if _, err := Write(home, info.ID, "notes/big.md", strings.Repeat("a", MaxFileBytes+1), "", "agent", "req"); !errors.Is(err, ErrTooLarge) {
		t.Fatal("unbounded file", err)
	}
	if _, err := WriteChecked(home, info.ID, "notes/denied.md", "denied", "", "agent", "req", func() error { return ErrConflict }); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := Read(home, info.ID, "notes/denied.md"); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked task wrote content", err)
	}
}
func TestFreshStartSearchAndLatestIndex(t *testing.T) {
	home, info := fixture(t)
	legacy := filepath.Join(home, "agent-homes", "old")
	os.MkdirAll(legacy, 0700)
	os.WriteFile(filepath.Join(legacy, "CLAUDE.md"), []byte("LEGACY SECRET"), 0600)
	index, err := Bootstrap(home, info.ID)
	if err != nil || strings.Contains(index.Content, "LEGACY") {
		t.Fatal("legacy content loaded", err)
	}
	_, err = Write(home, info.ID, "notes/中文.md", "观察时间：2026-09-23\n来源：task:test\n部署之前先验证环境。", "", "agent", "req")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Write(home, info.ID, "MEMORY.md", "# Index\n[Deployment](notes/中文.md)", index.Digest, "agent", "index")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := Bootstrap(home, info.ID)
	if err != nil || latest.Digest == index.Digest {
		t.Fatal("stale bootstrap", err)
	}
	matches, err := Search(home, info.ID, "验证", 10)
	if err != nil || len(matches) != 1 || !strings.Contains(matches[0].Snippet, "验证") {
		t.Fatalf("search %v %v", matches, err)
	}
}

func TestOwnerIdentityLengthAlwaysProducesUsableWorkspace(t *testing.T) {
	for _, n := range []int{1, 122, 123, 128} {
		ref, err := OwnerRef(strings.Repeat("a", n))
		if n > 122 {
			if !errors.Is(err, ErrInvalid) {
				t.Fatal("overlong identity accepted", n, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Ensure(t.TempDir(), ref); err != nil {
			t.Fatal("OwnerRef produced unusable reference", n, err)
		}
	}
}

func TestNextWriteRecoversPublishedPendingRevision(t *testing.T) {
	home, info := fixture(t)
	doc, err := Write(home, info.ID, "notes/nested/topic.md", "first", "", "agent", "first")
	if err != nil {
		t.Fatal(err)
	}
	h := filepath.Join(home, "agent-workspace-history", info.ID)
	markers, err := filepath.Glob(filepath.Join(h, "*.json"))
	if err != nil || len(markers) != 1 {
		t.Fatal(markers, err)
	}
	pending := strings.TrimSuffix(markers[0], ".json") + ".pending"
	if err = os.Rename(markers[0], pending); err != nil {
		t.Fatal(err)
	}
	// Simulate a process dying after publishing content but before finalizing its
	// history marker. The next writer must recover it before replacing content.
	if _, err = Write(home, info.ID, doc.Path, "second", doc.Digest, "agent", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending receipt not recovered", err)
	}
	history, err := History(home, info.ID, doc.Path)
	if err != nil || len(history) != 2 {
		t.Fatal(history, err)
	}
	if history[0].PreviousDigest != doc.Digest || history[1].Digest != doc.Digest {
		t.Fatal("crash lost a committed revision", history)
	}
}

func TestHistoryReturnsOnlyLatestHundred(t *testing.T) {
	home, info := fixture(t)
	doc, err := Write(home, info.ID, "notes/topic.md", "first", "", "agent", "first")
	if err != nil {
		t.Fatal(err)
	}
	// Historical metadata is sufficient to verify output bounding; ordinary write
	// durability and blob integrity are covered separately.
	h := filepath.Join(home, "agent-workspace-history", info.ID)
	for n := 0; n < MaxHistory+5; n++ {
		rev := Revision{Path: doc.Path, Digest: doc.Digest, CreatedAt: time.Date(2030, 1, 1, 0, 0, n, 0, time.UTC).Format(time.RFC3339Nano)}
		raw, _ := json.Marshal(rev)
		if err = os.WriteFile(filepath.Join(h, fmt.Sprintf("fixture-%d.json", n)), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	history, err := History(home, info.ID, doc.Path)
	if err != nil || len(history) != MaxHistory {
		t.Fatal(len(history), err)
	}
	if history[0].CreatedAt != time.Date(2030, 1, 1, 0, 0, MaxHistory+4, 0, time.UTC).Format(time.RFC3339Nano) {
		t.Fatal("history is not newest first", history[0])
	}
}

func TestCrossProcessCompareAndSwap(t *testing.T) {
	const marker = "MEMGOV_WORKSPACE_CAS_CHILD"
	if os.Getenv(marker) == "1" {
		var input struct{ Home, ID, Path, Digest, Content, Gate string }
		if err := json.Unmarshal([]byte(os.Getenv("MEMGOV_WORKSPACE_CAS_INPUT")), &input); err != nil {
			os.Exit(3)
		}
		for start := time.Now(); ; {
			if _, err := os.Stat(input.Gate); err == nil {
				break
			}
			if time.Since(start) > 10*time.Second {
				os.Exit(4)
			}
			time.Sleep(time.Millisecond)
		}
		_, err := Write(input.Home, input.ID, input.Path, input.Content, input.Digest, "child", input.Content)
		if err == nil {
			fmt.Print("ok")
		} else if errors.Is(err, ErrConflict) {
			fmt.Print("conflict")
		} else {
			fmt.Print(err)
			os.Exit(5)
		}
		os.Exit(0)
	}
	home, info := fixture(t)
	doc, err := Write(home, info.ID, "notes/process.md", "original", "", "agent", "initial")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "start")
	type child struct {
		cmd    *exec.Cmd
		output strings.Builder
	}
	children := []*child{}
	for _, content := range []string{"one", "two"} {
		raw, _ := json.Marshal(map[string]string{"Home": home, "ID": info.ID, "Path": doc.Path, "Digest": doc.Digest, "Content": content, "Gate": gate})
		c := &child{cmd: exec.Command(executable, "-test.run=^TestCrossProcessCompareAndSwap$")}
		c.cmd.Env = append(os.Environ(), marker+"=1", "MEMGOV_WORKSPACE_CAS_INPUT="+string(raw))
		c.cmd.Stdout = &c.output
		c.cmd.Stderr = &c.output
		if err = c.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, c)
	}
	if err = os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	results := map[string]int{}
	for _, c := range children {
		if err = c.cmd.Wait(); err != nil {
			t.Fatal(err, c.output.String())
		}
		results[c.output.String()]++
	}
	if results["ok"] != 1 || results["conflict"] != 1 {
		t.Fatal("cross-process lost update", results)
	}
}
