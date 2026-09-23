package console

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agentworkspace"
)

func workspaceData[T any](t *testing.T, s *Server, path string) T {
	t.Helper()
	w := request(t, s, "GET", path, "", nil)
	if w.Code != 200 {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("workspace response may be cached")
	}
	var body struct {
		OK   bool `json:"ok"`
		Data T    `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.OK {
		t.Fatalf("invalid workspace response: %s", w.Body.String())
	}
	return body.Data
}

func TestWorkspaceBrowserScopesSearchReadsAndHistory(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	owner, _ := agentworkspace.OwnerRef("owner-test")
	group, _ := agentworkspace.GroupRef("channel-test", "group-test")
	for _, ref := range []agentworkspace.Ref{owner, group} {
		if _, err := agentworkspace.Ensure(s.opts.Home, ref); err != nil {
			t.Fatal(err)
		}
	}
	first, err := agentworkspace.Write(s.opts.Home, owner.ID, "notes/release.md", "# Release\nOwner-only source: task-a\n", "", "owner", "write-1")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := agentworkspace.Write(s.opts.Home, owner.ID, first.Path, "# Release\nCurrent procedure 中文\nSource: task-b\n", first.Digest, "owner", "write-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentworkspace.Write(s.opts.Home, group.ID, "notes/group.md", "# Group\nGroup-only context", "", "group", "write-3"); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/agent-workspaces/" + owner.ID
	workspaces := workspaceData[[]agentworkspace.Info](t, s, "/api/v1/agent-workspaces")
	if len(workspaces) != 2 {
		t.Fatalf("workspaces: %+v", workspaces)
	}
	files := workspaceData[[]agentworkspace.FileInfo](t, s, base+"/files")
	if len(files) != 2 {
		t.Fatalf("files: %+v", files)
	}
	matches := workspaceData[[]agentworkspace.Match](t, s, base+"/files?query="+url.QueryEscape("procedure 中文"))
	if len(matches) != 1 || matches[0].Path != first.Path || matches[0].Digest != latest.Digest {
		t.Fatalf("current search: %+v", matches)
	}
	for _, query := range []string{"Owner-only", "Group-only"} {
		if matches := workspaceData[[]agentworkspace.Match](t, s, base+"/files?query="+url.QueryEscape(query)); len(matches) != 0 {
			t.Fatalf("search included another workspace or old revision: %+v", matches)
		}
	}
	doc := workspaceData[agentworkspace.Document](t, s, base+"/file?path=notes%2Frelease.md")
	if doc.Content != latest.Content || doc.Digest != latest.Digest {
		t.Fatalf("document: %+v", doc)
	}
	revisions := workspaceData[[]agentworkspace.Revision](t, s, base+"/history?path=notes%2Frelease.md")
	if len(revisions) != 2 {
		t.Fatalf("history: %+v", revisions)
	}
	if w := request(t, s, "GET", "/api/v1/agent-workspaces/"+group.ID+"/file?path=notes%2Frelease.md", "", nil); w.Code != 404 {
		t.Fatalf("cross-workspace read: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/v1/memories", "/api/v1/memory-workspaces", "/api/v1/memories/old-id/history"} {
		if w := request(t, s, "GET", path, "", nil); w.Code != 404 {
			t.Fatalf("legacy API %s: %d", path, w.Code)
		}
	}
}

func TestWorkspaceBrowserRejectsWritesAndPathEscapes(t *testing.T) {
	s, _, _ := fixture(t, time.Now())
	owner, _ := agentworkspace.OwnerRef("owner-test")
	if _, err := agentworkspace.Ensure(s.opts.Home, owner); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/agent-workspaces/" + owner.ID
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if w := request(t, s, method, base+"/file?path=MEMORY.md", `{ "content": "changed" }`, nil); w.Code != 405 {
			t.Fatalf("accepted %s: %d", method, w.Code)
		}
	}
	for _, path := range []string{"../state.db", "/etc/passwd", "notes/../../state.db", "workspace.json", "notes/.hidden.md"} {
		for _, endpoint := range []string{"file", "history"} {
			if w := request(t, s, "GET", base+"/"+endpoint+"?path="+url.QueryEscape(path), "", nil); w.Code != 400 {
				t.Fatalf("accepted %s %q: %d %s", endpoint, path, w.Code, w.Body.String())
			}
		}
	}
	if w := request(t, s, "GET", base+"/files?query="+strings.Repeat("x", 501), "", nil); w.Code != 400 {
		t.Fatal("accepted unbounded search")
	}
	if w := request(t, s, "GET", "/api/v1/agent-workspaces/owner-missing/file?path=MEMORY.md", "", nil); w.Code != 404 {
		t.Fatal("missing workspace did not return 404")
	}
	secret := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(secret, []byte("OUTSIDE-SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(s.opts.Home, "agent-workspaces", owner.ID, "notes", "link.md")); err != nil {
		t.Fatal(err)
	}
	if w := request(t, s, "GET", base+"/file?path=notes%2Flink.md", "", nil); w.Code != 400 || strings.Contains(w.Body.String(), "OUTSIDE-SECRET") {
		t.Fatalf("symlink exposed: %d %s", w.Code, w.Body.String())
	}
	if w := request(t, s, "GET", base+"/files?query=OUTSIDE-SECRET", "", nil); strings.Contains(w.Body.String(), "OUTSIDE-SECRET") {
		t.Fatal("search followed symlink")
	}
}
