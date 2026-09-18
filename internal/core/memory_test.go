package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"unicode/utf8"
)

func fixtureMemory(t *testing.T, s *Store, scope string) Memory {
	t.Helper()
	ctx := context.Background()
	var source Source
	_, err := s.Mutate(ctx, Request{Scope: scope, Command: "fixture"}, func(tx *Tx) (any, error) {
		var err error
		source, err = tx.Ingest(ctx, SourceInput{Kind: "note", URI: "fixture://release", Content: "发布失败时先检查权限，再查看错误日志。测试环境已验证该处理流程。"})
		return source, err
	})
	if err != nil {
		t.Fatal(err)
	}
	f := source.Fragments[0]
	return Memory{Category: "fact", Title: "发布权限检查", Summary: "发布失败时先检查权限。", Content: "发布失败时先检查权限，再查看错误日志。", WorkspaceID: scope, Evidence: []Evidence{{source.ID, f.ID, f.SHA256, "发布失败时先检查权限"}}}
}
func createMemory(t *testing.T, s *Store, m Memory) Memory {
	t.Helper()
	ctx := context.Background()
	var got Memory
	_, err := s.Mutate(ctx, Request{Scope: scopeID(m.WorkspaceID), Command: "create"}, func(tx *Tx) (any, error) {
		c, err := tx.SubmitCandidate(ctx, CandidateInput{Memory: m, Reason: "test evidence"}, "", "tester")
		if err != nil {
			return nil, err
		}
		out, err := tx.ApplyCandidate(ctx, c.ID, c.Digest)
		if err == nil {
			got = out.(map[string]any)["memory"].(Memory)
		}
		return out, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func TestCandidateRevisionSearchAndScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	for _, query := range []string{"发布", "权限检查", "发布 权限"} {
		hits, err := Search(ctx, s.DB, query, SearchOptions{Kind: "memory"})
		if err != nil || len(hits) != 1 {
			t.Fatalf("query %q: %+v %v", query, hits, err)
		}
	}
	if _, err := Search(ctx, s.DB, "\"--not sql", SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err := Recall(ctx, s.DB, "发布", SearchOptions{}, 200, true)
	if err != nil || len(r.Items) != 1 || r.UsedChars != utf8.RuneCountInString(r.Context) || r.UsedChars > 200 {
		t.Fatalf("recall: %+v %v", r, err)
	}
	r, err = Recall(ctx, s.DB, "发布", SearchOptions{}, 1, false)
	if err != nil || len(r.Items) != 0 || r.Skipped != 1 {
		t.Fatal(r, err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "retire"}, func(tx *Tx) (any, error) { return tx.SetState(ctx, m.ID, "retired", "outdated", 1, 0) })
	if err != nil {
		t.Fatal(err)
	}
	r, err = Recall(ctx, s.DB, "发布", SearchOptions{}, 1000, false)
	if err != nil || len(r.Items) != 0 {
		t.Fatal(r, err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.SetState(ctx, m.ID, "active", "stale writer", 1, 0) })
	if ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	history, err := History(ctx, s.DB, m.ID, "global")
	if err != nil || len(history) != 2 {
		t.Fatal(history, err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.SetState(ctx, m.ID, "active", "restore snapshot", 2, 1) })
	if err != nil {
		t.Fatal(err)
	}
	var w Workspace
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { var e error; w, e = tx.AddWorkspace(ctx, "private", ""); return w, e })
	if err != nil {
		t.Fatal(err)
	}
	private := fixtureMemory(t, s, w.ID)
	private.Title = "私域发布"
	pm := createMemory(t, s, private)
	if _, err = ReadMemory(ctx, s.DB, pm.ID, "global", 0); ErrorCode(err) != "not_found" {
		t.Fatalf("workspace leaked: %v", err)
	}
	all, err := Search(ctx, s.DB, "发布", SearchOptions{Kind: "memory", AllWorkspaces: true})
	if err != nil || len(all) != 2 {
		t.Fatal(all, err)
	}
}
func TestEvidenceAndStaleCandidateRejectedWithoutPartialWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := fixtureMemory(t, s, "global")
	m.Evidence[0].Quote = "not in source"
	_, err := s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		return tx.SubmitCandidate(ctx, CandidateInput{Memory: m, Reason: "test"}, "", "")
	})
	if ErrorCode(err) != "invalid_input" {
		t.Fatal(err)
	}
	m.Evidence[0].Quote = ""
	m = createMemory(t, s, m)
	draft := m
	draft.ID = ""
	draft.Version = 0
	draft.Content += " 新版本。"
	var c Candidate
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		var e error
		c, e = tx.SubmitCandidate(ctx, CandidateInput{Action: "update", TargetID: m.ID, ExpectedVersion: 1, Memory: draft, Reason: "revise"}, "", "")
		return c, e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.SetState(ctx, m.ID, "retired", "new evidence", 1, 0) })
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.ApplyCandidate(ctx, c.ID, c.Digest) })
	if ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	c, err = ReadCandidate(ctx, s.DB, c.ID, "global")
	if err != nil || c.Status != "pending" {
		t.Fatal(c, err)
	}
}
func TestSourceFragmentCoverageAndDuplicateLocations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	body := ""
	for i := 0; i < 1500; i++ {
		body += fmt.Sprintf("第 %d 行记录。\n", i)
	}
	var source Source
	for _, uri := range []string{"fixture://a", "fixture://b"} {
		result, err := s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.Ingest(ctx, SourceInput{URI: uri, Content: body}) })
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(result.Data, &source); err != nil {
			t.Fatal(err)
		}
	}
	joined := ""
	for _, f := range source.Fragments {
		joined += f.Content
		if Hash([]byte(f.Content)) != f.SHA256 {
			t.Fatal("bad digest")
		}
	}
	if joined != body || len(source.Locations) != 2 || len(source.Fragments) < 2 {
		t.Fatal("lost fragments or duplicate source")
	}
}

func TestRestoreRetiredRevisionReactivatesWithNewVersion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	for _, step := range []struct {
		status             string
		expected, revision int
	}{{"retired", 1, 0}, {"active", 2, 2}} {
		_, err := s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
			return tx.SetState(ctx, m.ID, step.status, "restore lifecycle test", step.expected, step.revision)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadMemory(ctx, s.DB, m.ID, "global", 0)
	if err != nil || got.Status != "active" || got.Version != 3 {
		t.Fatal(got, err)
	}
	old, err := ReadMemory(ctx, s.DB, m.ID, "global", 2)
	if err != nil || old.Status != "retired" {
		t.Fatal("historical status mutated", old, err)
	}
}
