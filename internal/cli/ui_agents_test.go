package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

const editorYAML = `# keep this comment
logging:
  retention: 720h
agents:
  helper:
    skills:
      inherit: none
  free:
    skills:
      inherit: none
applications:
  owner_private:
    enabled: false
    runtime: owner-private
    agent: helper
`

func TestUIAgentPendingComparisonIgnoresSetOrder(t *testing.T) {
	a := AgentDeclaration{Capabilities: []string{"local_read"}, Directories: []string{"/b", "/a"}, Skills: core.RuntimeSkillPolicy{Paths: []string{"/skill-b", "/skill-a"}}}
	b := AgentDeclaration{Capabilities: []string{"local_read"}, Directories: []string{"/a", "/b"}, Skills: core.RuntimeSkillPolicy{Paths: []string{"/skill-a", "/skill-b"}, Resolved: []core.RuntimeSkill{{Name: "ignored-disk-digest"}}}}
	if agentDigest(a) != agentDigest(b) {
		t.Fatal("equivalent Agent declarations marked pending")
	}
}

func editorFixture(t *testing.T) (*agentConfigEditor, *core.Store) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte(editorYAML), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := core.Open(context.Background(), filepath.Join(home, "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &agentConfigEditor{home: home, path: path}, s
}
func editorDraft(t *testing.T, e *agentConfigEditor, name, operation string) agentEdit {
	t.Helper()
	raw, a, _, err := e.read()
	if err != nil {
		t.Fatal(err)
	}
	n, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return agentEdit{Revision: core.Hash(raw), Name: name, Operation: operation, Agent: n.Declaration.Agents[name]}
}
func editJSON(t *testing.T, d agentEdit) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUIAgentEditPreviewSaveAndStaleRevision(t *testing.T) {
	e, s := editorFixture(t)
	ctx := context.Background()
	if _, err := e.preview(ctx, s.DB, json.RawMessage(`{"revision":"x","name":"free","operation":"update","file_path":"/tmp/other"}`)); core.ErrorCode(err) != "invalid_input" {
		t.Fatal("unknown edit field accepted", err)
	}
	d := editorDraft(t, e, "free", "update")
	d.Agent.ExecutionModel = "sonnet"
	initial, _ := os.ReadFile(e.path)
	before, err := core.ReadAppliedConfig(ctx, s.DB, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.preview(ctx, s.DB, editJSON(t, d)); err != nil {
		t.Fatal(err)
	}
	if current, _ := os.ReadFile(e.path); !bytes.Equal(current, initial) {
		t.Fatal("preview wrote config")
	}
	if _, err := e.save(ctx, s.DB, editJSON(t, d)); err != nil {
		t.Fatal(err)
	}
	current, a, _, err := e.read()
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.Agents["free"].ExecutionModel != "sonnet" || a.cfg.Logging.Retention != "720h" || !strings.Contains(string(current), "# keep this comment") {
		t.Fatal(string(current))
	}
	info, _ := os.Stat(e.path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("config permissions changed")
	}
	if _, err := e.save(ctx, s.DB, editJSON(t, d)); core.ErrorCode(err) != "conflict" {
		t.Fatal("stale edit accepted", err)
	}
	if body, _ := os.ReadFile(e.path); !bytes.Equal(body, current) {
		t.Fatal("stale save changed file")
	}
	after, err := core.ReadAppliedConfig(ctx, s.DB, 0)
	if err != nil || before.Version != after.Version {
		t.Fatal("file edit applied runtime", after, err)
	}
	value, err := e.list(ctx, s.DB)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(value)
	if !strings.Contains(string(b), `"pending":true`) {
		t.Fatal(string(b))
	}
}

func TestUIAgentDeleteReferencesValidationAndSymlink(t *testing.T) {
	e, s := editorFixture(t)
	ctx := context.Background()
	d := editorDraft(t, e, "helper", "delete")
	if _, err := e.save(ctx, s.DB, editJSON(t, d)); core.ErrorCode(err) != "conflict" {
		t.Fatal("referenced Agent deleted", err)
	}
	d = editorDraft(t, e, "free", "update")
	d.Agent.Capabilities = []string{"unsupported"}
	if _, err := e.save(ctx, s.DB, editJSON(t, d)); core.ErrorCode(err) != "invalid_input" {
		t.Fatal(err)
	}
	d = editorDraft(t, e, "free", "update")
	d.Agent.Skills.Paths = []string{"./not-a-skill"}
	if _, err := e.save(ctx, s.DB, editJSON(t, d)); err == nil {
		t.Fatal("missing skill saved")
	}
	d = editorDraft(t, e, "free", "update")
	d.Agent.Skills.Resolved = []core.RuntimeSkill{{Name: "injected"}}
	if _, err := e.save(ctx, s.DB, editJSON(t, d)); core.ErrorCode(err) != "invalid_input" {
		t.Fatal(err)
	}
	link := filepath.Join(e.home, "linked.yaml")
	if err := os.Symlink(e.path, link); err != nil {
		t.Fatal(err)
	}
	e.path = link
	d = editorDraft(t, e, "free", "delete")
	if _, err := e.save(ctx, s.DB, editJSON(t, d)); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(link)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	_, a, _, err := e.read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.cfg.Agents["free"]; ok {
		t.Fatal("Agent not removed")
	}
	if _, ok := a.cfg.Agents["helper"]; !ok {
		t.Fatal("unrelated Agent removed")
	}
}

func TestUIAgentDeleteAppliedReferenceAndConcurrentEditors(t *testing.T) {
	e, s := editorFixture(t)
	ctx := context.Background()
	_, err := s.Mutate(ctx, core.Request{Scope: "global", Command: "editor.fixture"}, func(tx *core.Tx) (any, error) {
		return tx.CommitAppliedConfig(ctx, 0, 1, json.RawMessage(`{"agents":{},"applications":{"proactive":{"agent":"free"}}}`), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	d := editorDraft(t, e, "free", "delete")
	if _, err := e.save(ctx, s.DB, editJSON(t, d)); core.ErrorCode(err) != "conflict" {
		t.Fatal("applied reference ignored", err)
	}
	e2 := &agentConfigEditor{home: e.home, path: e.path}
	d = editorDraft(t, e, "free", "update")
	d.Agent.ExecutionModel = "sonnet"
	input := editJSON(t, d)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, editor := range []*agentConfigEditor{e, e2} {
		wg.Add(1)
		go func(editor *agentConfigEditor) {
			defer wg.Done()
			_, err := editor.save(ctx, s.DB, input)
			results <- err
		}(editor)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if core.ErrorCode(err) != "conflict" {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("concurrent file edits did not fence revisions", success)
	}
}
