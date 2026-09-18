package core

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestMergeAtomicUndoAndVersionConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := createMemory(t, s, fixtureMemory(t, s, "global"))
	b := fixtureMemory(t, s, "global")
	b.Content = "处理发布故障时查看错误日志。"
	b = createMemory(t, s, b)
	target := a
	target.ID = ""
	target.Version = 0
	target.Content = "发布失败时检查权限并查看错误日志。"
	var p MergePlan
	_, err := s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		var e error
		p, e = tx.PreviewMerge(ctx, MergeInput{Sources: []VersionRef{{a.ID, 1}, {b.ID, 1}}, Memory: target, Reason: "combine related instructions"})
		return p, e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.ApplyMerge(ctx, p.ID, p.Digest) })
	if err != nil {
		t.Fatal(err)
	}
	g, err := MemoryGraph(ctx, s.DB, p.TargetID, "global", 2, 20)
	if err != nil || len(g.Edges) < 3 {
		t.Fatal(g, err)
	}
	for _, id := range []string{a.ID, b.ID} {
		m, e := ReadMemory(ctx, s.DB, id, "global", 0)
		if e != nil || m.Status != "superseded" || m.Version != 2 {
			t.Fatal(m, e)
		}
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.UndoMerge(ctx, p.ID, "undo verified merge") })
	if err != nil {
		t.Fatal(err)
	}
	m, err := ReadMemory(ctx, s.DB, a.ID, "global", 0)
	if err != nil || m.Version != 3 || m.Status != "active" {
		t.Fatal(m, err)
	}
	// A fresh preview must not cover changes made after it.
	p.Input.Sources = []VersionRef{{a.ID, 3}, {b.ID, 3}}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { var e error; p, e = tx.PreviewMerge(ctx, p.Input); return p, e })
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.SetState(ctx, b.ID, "retired", "later edit", 3, 0) })
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.ApplyMerge(ctx, p.ID, p.Digest) })
	if ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	if _, err = ReadMemory(ctx, s.DB, p.TargetID, "global", 0); ErrorCode(err) != "not_found" {
		t.Fatal("partial merge", err)
	}
}
func TestPurgeOwnedCopiesTombstonesAndResumableCompaction(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	// Both current and retired history reference the same immutable source.
	_, err := s.Mutate(ctx, Request{Scope: "global", Key: "cached-retire", Command: "retire"}, func(tx *Tx) (any, error) { return tx.SetState(ctx, m.ID, "retired", "contains old payload", 1, 0) })
	if err != nil {
		t.Fatal(err)
	}
	src, err := ReadSource(ctx, s.DB, m.Evidence[0].SourceID, "global")
	if err != nil {
		t.Fatal(err)
	}
	var p PurgePlan
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		out, e := tx.PreviewPurge(ctx, m.ID, 2)
		if e == nil {
			p = out.(map[string]any)["plan"].(PurgePlan)
		}
		return out, e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.ApplyPurge(ctx, p.ID, p.Digest) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = History(ctx, s.DB, m.ID, "global"); ErrorCode(err) != "not_found" {
		t.Fatal(err)
	}
	hits, err := Search(ctx, s.DB, "发布", SearchOptions{})
	if err != nil || len(hits) != 0 {
		t.Fatal(hits, err)
	}
	for _, in := range []SourceInput{{URI: src.Locations[0], Content: "changed body"}, {URI: "copy://other", Content: src.Fragments[0].Content}} {
		_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.Ingest(ctx, in) })
		if ErrorCode(err) != "denied" {
			t.Fatal("resurrection", err)
		}
	}
	var n int
	s.DB.QueryRow("SELECT count(*) FROM idempotency").Scan(&n)
	if n != 0 {
		t.Fatal("cached body survived")
	}
	path := s.Path
	s.Close()
	exclusive, err := OpenMaintenance(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := exclusive.CompactPurge(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.(map[string]any)["status"] != "database_compacted" {
		t.Fatal(status)
	}
	exclusive.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), m.Content) || strings.Contains(string(raw), src.Fragments[0].Content) {
		t.Fatal("owned plaintext survived compaction")
	}
}
func TestPurgePreviewRejectsNewDependentCandidate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	var p PurgePlan
	_, err := s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		out, e := tx.PreviewPurge(ctx, m.ID, 1)
		if e == nil {
			raw, _ := json.Marshal(out.(map[string]any)["plan"])
			json.Unmarshal(raw, &p)
		}
		return out, e
	})
	if err != nil {
		t.Fatal(err)
	}
	draft := m
	draft.ID = ""
	draft.Version = 0
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		return tx.SubmitCandidate(ctx, CandidateInput{Memory: draft, Reason: "later proposal"}, "", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.ApplyPurge(ctx, p.ID, p.Digest) })
	if ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
}

func TestConflictLinksWithdrawBothMemoriesAndRequireVersions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := createMemory(t, s, fixtureMemory(t, s, "global"))
	doc := fixtureMemory(t, s, "global")
	doc.Title = "另一份记录"
	doc.Content = "另一份不同的发布记录"
	b := createMemory(t, s, doc)
	_, err := s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		return tx.MemoryConflict(ctx, a.ID, b.ID, 1, 1, false, "two evidence-backed statements disagree")
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		m, e := ReadMemory(ctx, s.DB, id, "global", 0)
		if e != nil || m.Status != "disputed" || m.Version != 2 {
			t.Fatal(m, e)
		}
	}
	graph, err := MemoryGraph(ctx, s.DB, a.ID, "global", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, edge := range graph.Edges {
		found = found || edge.Kind == "conflicts_with"
	}
	if !found {
		t.Fatal(graph)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.SetState(ctx, a.ID, "active", "cannot ignore conflict", 2, 0) })
	if ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.MemoryConflict(ctx, a.ID, b.ID, 1, 2, true, "stale resolution") })
	if ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		return tx.MemoryConflict(ctx, a.ID, b.ID, 2, 2, true, "reviewed resolution; reactivation separate")
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ReadMemory(ctx, s.DB, a.ID, "global", 0)
	if err != nil || m.Status != "disputed" || m.Version != 3 {
		t.Fatal(m, err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.SetState(ctx, a.ID, "active", "resolved and reviewed", 3, 0) })
	if err != nil {
		t.Fatal(err)
	}
}
