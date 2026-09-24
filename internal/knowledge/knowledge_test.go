package knowledge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportRelayerCopiesPrivateCredentialsWithoutChangingSource(t *testing.T) {
	home, legacy := t.TempDir(), filepath.Join(t.TempDir(), "external-knowledge.sqlite")
	db, err := sql.Open("sqlite", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE connections(provider TEXT PRIMARY KEY, body TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ provider, body string }{
		{"dokki", `{"revision":1,"credentials":{"apiKey":"dokki-private"}}`},
		{"confluence", `{"revision":1,"credentials":{"dockToken":"dock-private","pat":"pat-private"}}`},
	} {
		if _, err = db.Exec("INSERT INTO connections VALUES(?,?)", row.provider, row.body); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(legacy, 0600); err != nil {
		t.Fatal(err)
	}
	status, err := ImportRelayer(home, legacy)
	if err != nil || !status["dokki"] || !status["confluence"] {
		t.Fatalf("import: %v %v", status, err)
	}
	info, err := os.Stat(file(home))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private file: %v %v", info, err)
	}
	data, err := Load(home)
	if err != nil || data.Dokki.APIKey != "dokki-private" || data.Confluence.PAT != "pat-private" {
		t.Fatalf("copied credentials: %v", err)
	}
	if _, err = ImportRelayer(home, legacy); err != nil {
		t.Fatalf("retry should be idempotent: %v", err)
	}
	if _, err = os.Stat(legacy); err != nil {
		t.Fatalf("source removed: %v", err)
	}
	if strings.Contains(strings.Join([]string{statusString(status)}, ""), "private") {
		t.Fatal("status leaked a credential")
	}
}

func TestConfigureReplacesCredentialsWithoutExposingThemInStatus(t *testing.T) {
	home := t.TempDir()
	first := []byte(`{"dokki":{"api_key":"d-one"},"confluence":{"dock_token":"t-one","pat":"p-one"}}`)
	status, err := Configure(home, first)
	if err != nil || !status["dokki"] || !status["confluence"] {
		t.Fatalf("configure: %v %v", status, err)
	}
	second := []byte(`{"dokki":{"api_key":"d-two"},"confluence":{"dock_token":"t-two","pat":"p-two"}}`)
	if _, err := Configure(home, second); err != nil {
		t.Fatal(err)
	}
	c, err := Load(home)
	if err != nil || c.Dokki.APIKey != "d-two" || c.Confluence.PAT != "p-two" {
		t.Fatalf("replacement failed: %v", err)
	}
	if _, err := Configure(home, []byte(`{"dokki":{"api_key":"x"},"extra":true}`)); err == nil {
		t.Fatal("incomplete or unknown credential fields accepted")
	}
	info, err := os.Stat(file(home))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential mode: %v %v", info, err)
	}
}

func statusString(value any) string { raw, _ := json.Marshal(value); return string(raw) }

func TestMCPExposesOnlySelectedToolsAndRejectsUnavailableSource(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_confluence","arguments":{"query":"x"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_dokki_workspaces","arguments":{}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := ServeMCP(context.Background(), t.TempDir(), []string{"dokki"}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("responses: %s", output.String())
	}
	var list struct {
		Result struct {
			Tools []mcpTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Result.Tools) != 3 {
		t.Fatalf("wrong tools: %v", list.Result.Tools)
	}
	for _, tool := range list.Result.Tools {
		if _, ok := tool.InputSchema["required"].([]any); !ok {
			t.Fatalf("MCP tool %s has a non-array required schema", tool.Name)
		}
	}
	if !strings.Contains(lines[2], "tool is not enabled") || !strings.Contains(lines[3], "Dokki is not configured") {
		t.Fatalf("boundary failure: %s", output.String())
	}
}

func TestDirectClientsValidateIdentityAndOnlyRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dokki/search":
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer dokki-private" {
				t.Errorf("Dokki auth or method")
			}
			_, _ = w.Write([]byte(`{"results":[{"resource_id":"abc","title":"Title","type":"document","highlight":"dokki-private"}],"total":1,"mode":"hybrid","page":{"returned":1,"limit":10,"has_more":false}}`))
		case "/dokki/resources/abc":
			_, _ = w.Write([]byte(`{"resource":{"id":"abc","type":"document"}}`))
		case "/dokki/resources/abc/content":
			_, _ = w.Write([]byte(`{"resource":{"id":"abc","type":"document"},"content":{"type":"document","document":{"content":"Body"}}}`))
		case "/confluence":
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer dock-private" || r.Header.Get("x-confluence-pat") != "pat-private" {
				t.Errorf("Confluence auth or method")
			}
			var req struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Method == "tools/list" {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"confluence_content_get","inputSchema":{"type":"object"}},{"name":"confluence_content_create","inputSchema":{"type":"object"}}]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"data":{"results":[{"title":"Page","_links":{"base":"https://confluence.example/wiki","webui":"/display/ABC/Page"}}]}}}}`))
		case "/rest/content/search":
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer pat-private" || !strings.Contains(r.URL.Query().Get("cql"), `text ~ "query"`) {
				t.Errorf("Confluence REST search request: %s", r.URL.String())
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"42","type":"page","title":"Page","_links":{"webui":"/display/ABC/Page"}}],"totalSize":1,"_links":{"base":"https://confluence.example/wiki"}}`))
		case "/rest/content/42":
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer pat-private" || r.URL.Query().Get("expand") != "body.storage,version,space" {
				t.Errorf("Confluence REST read request: %s", r.URL.String())
			}
			_, _ = w.Write([]byte(`{"id":"42","type":"page","title":"Page","body":{"storage":{"value":"<p>Body</p>"}},"_links":{"base":"https://confluence.example/wiki","webui":"/display/ABC/Page"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var creds Credentials
	creds.Dokki.APIKey = "dokki-private"
	creds.Confluence.DockToken = "dock-private"
	creds.Confluence.PAT = "pat-private"
	c := NewClient(creds)
	c.DokkiURL = server.URL + "/dokki"
	c.ConfluenceURL = server.URL + "/confluence"
	c.ConfluenceRESTURL = server.URL + "/rest"
	search, err := c.SearchDokki(context.Background(), "query", "hybrid", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(search)
	if strings.Contains(string(raw), "dokki-private") || !strings.Contains(string(raw), "https://dokki.one/doc/abc") {
		t.Fatalf("Dokki redaction/link: %s", raw)
	}
	if _, err = c.ReadDokkiResource(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	tools, err := c.ListConfluenceTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	toolRaw, _ := json.Marshal(tools)
	if strings.Contains(string(toolRaw), "content_create") || !strings.Contains(string(toolRaw), "content_get") {
		t.Fatalf("tool allowlist: %s", toolRaw)
	}
	if _, err = c.QueryConfluence(context.Background(), "confluence_content_create", nil); err == nil {
		t.Fatal("write tool admitted")
	}
	if _, err = c.QueryConfluence(context.Background(), "confluence_search", nil); err == nil {
		t.Fatal("broken advanced search admitted")
	}
	page, err := c.SearchConfluence(context.Background(), "query", "keyword", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	pageRaw, _ := json.Marshal(page)
	if !strings.Contains(string(pageRaw), "https://confluence.example/wiki/display/ABC/Page") {
		t.Fatalf("missing source URL: %s", pageRaw)
	}
	read, err := c.ReadConfluencePage(context.Background(), "42")
	if err != nil {
		t.Fatal(err)
	}
	readMap, _ := object(read)
	body, _ := object(readMap["body"])
	storage, _ := object(body["storage"])
	if stringAt(storage, "value") != "<p>Body</p>" || stringAt(readMap, "source_url") != "https://confluence.example/wiki/display/ABC/Page" {
		t.Fatalf("Confluence exact read missing content or URL")
	}
	if _, err = c.ReadConfluencePage(context.Background(), "../42"); err == nil {
		t.Fatal("unsafe page ID accepted")
	}
}
