package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type VersionRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}
type MergeInput struct {
	Sources []VersionRef `json:"sources"`
	Memory  Memory       `json:"memory"`
	Reason  string       `json:"reason"`
}
type MergePlan struct {
	ID          string     `json:"id"`
	Digest      string     `json:"digest"`
	Status      string     `json:"status"`
	Scope       string     `json:"workspace_id"`
	Input       MergeInput `json:"input"`
	TargetID    string     `json:"target_id"`
	OperationID string     `json:"operation_id,omitempty"`
}

func unionEvidence(a, b []Evidence) []Evidence {
	out := append([]Evidence{}, a...)
	seen := map[string]bool{}
	for _, evidence := range a {
		seen[evidence.FragmentID] = true
	}
	for _, evidence := range b {
		if !seen[evidence.FragmentID] {
			seen[evidence.FragmentID] = true
			out = append(out, evidence)
		}
	}
	return out
}

func (tx *Tx) PreviewMerge(ctx context.Context, in MergeInput) (MergePlan, error) {
	p := MergePlan{ID: NewID(), Status: "pending", Scope: scopeID(tx.Request.Scope), Input: in, TargetID: NewID()}
	if len(in.Sources) < 2 || in.Reason == "" {
		return p, Fail("invalid_input", "merge requires at least two versioned sources and a reason")
	}
	if in.Memory.ID != "" || in.Memory.Version != 0 {
		return p, Fail("invalid_input", "target memory must omit id and version")
	}
	p.Input.Memory.WorkspaceID = p.Scope
	if p.Scope == "global" {
		p.Input.Memory.WorkspaceID = ""
	}
	seen := map[string]bool{}
	for _, ref := range in.Sources {
		if seen[ref.ID] {
			return p, Fail("invalid_input", "duplicate merge source")
		}
		seen[ref.ID] = true
		m, err := ReadMemory(ctx, tx.Conn, ref.ID, p.Scope, 0)
		if err != nil {
			return p, err
		}
		if scopeID(m.WorkspaceID) != p.Scope {
			return p, Fail("denied", "merge sources must share the selected workspace")
		}
		if ref.Version < 1 || m.Version != ref.Version {
			return p, Fail("conflict", "merge source %s version changed", ref.ID)
		}
		if m.Status == "superseded" || m.Status == "retired" {
			return p, Fail("conflict", "merge source %s is %s", ref.ID, m.Status)
		}
		p.Input.Memory.Evidence = unionEvidence(p.Input.Memory.Evidence, m.Evidence)
	}
	if err := ValidateMemory(p.Input.Memory); err != nil {
		return p, err
	}
	if err := checkEvidence(ctx, tx.Conn, p.Input.Memory); err != nil {
		return p, err
	}
	p.Digest = Digest(map[string]any{"input": p.Input, "target_id": p.TargetID, "scope": p.Scope})
	_, err := tx.Conn.ExecContext(ctx, "INSERT INTO plans(id,kind,document,digest,created_at) VALUES(?,'merge',?,?,?)", p.ID, JSON(p), p.Digest, Now())
	if err == nil {
		_, err = tx.Audit(ctx, "merge.preview", in.Reason, nil)
	}
	return p, err
}
func readMerge(ctx context.Context, q Queryer, id string) (MergePlan, error) {
	var p MergePlan
	var raw, status, op string
	err := q.QueryRowContext(ctx, "SELECT document,status,operation_id FROM plans WHERE id=? AND kind='merge'", id).Scan(&raw, &status, &op)
	if errors.Is(err, sql.ErrNoRows) {
		return p, Fail("not_found", "merge plan not found")
	}
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return p, err
	}
	p.Status = status
	p.OperationID = op
	return p, nil
}
func (tx *Tx) ApplyMerge(ctx context.Context, id, digest string) (any, error) {
	p, err := readMerge(ctx, tx.Conn, id)
	if err != nil {
		return nil, err
	}
	if p.Scope != scopeID(tx.Request.Scope) {
		return nil, Fail("denied", "select the merge workspace")
	}
	if digest == "" || p.Digest != digest {
		return nil, Fail("conflict", "expected-digest must match the merge preview")
	}
	if p.Status == "applied" {
		return p, nil
	}
	if p.Status != "pending" {
		return nil, Fail("conflict", "merge plan is %s", p.Status)
	}
	items := []Memory{}
	expected := map[string]int{}
	for _, r := range p.Input.Sources {
		m, e := ReadMemory(ctx, tx.Conn, r.ID, p.Scope, 0)
		if e != nil {
			return nil, e
		}
		if m.Version != r.Version {
			return nil, Fail("conflict", "merge source %s changed", r.ID)
		}
		m.Status = "superseded"
		items = append(items, m)
		expected[m.ID] = r.Version
	}
	target := p.Input.Memory
	target.ID = p.TargetID
	target.Status = "active"
	items = append(items, target)
	saved, op, err := tx.SaveMemories(ctx, "merge.apply", p.Input.Reason, items, expected)
	if err != nil {
		return nil, err
	}
	for _, r := range p.Input.Sources {
		if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO relations VALUES(?,'supersedes',?,?)", target.ID, r.ID, op.ID); err != nil {
			return nil, err
		}
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE plans SET status='applied',operation_id=? WHERE id=?", op.ID, id)
	return map[string]any{"plan_id": id, "memories": saved, "operation": op}, err
}
func (tx *Tx) UndoMerge(ctx context.Context, id, reason string) (any, error) {
	p, err := readMerge(ctx, tx.Conn, id)
	if err != nil {
		return nil, err
	}
	if p.Scope != scopeID(tx.Request.Scope) {
		return nil, Fail("denied", "select the merge workspace")
	}
	if p.Status == "undone" {
		return p, nil
	}
	if p.Status != "applied" {
		return nil, Fail("conflict", "merge plan is %s", p.Status)
	}
	target, err := ReadMemory(ctx, tx.Conn, p.TargetID, p.Scope, 0)
	if err != nil {
		return nil, err
	}
	if target.Version != 1 {
		return nil, Fail("conflict", "merged target changed; automatic undo refused")
	}
	target.Status = "retired"
	items := []Memory{target}
	expected := map[string]int{target.ID: 1}
	for _, r := range p.Input.Sources {
		current, e := ReadMemory(ctx, tx.Conn, r.ID, p.Scope, 0)
		if e != nil {
			return nil, e
		}
		if current.Version != r.Version+1 {
			return nil, Fail("conflict", "merge source %s changed; automatic undo refused", r.ID)
		}
		old, e := ReadMemory(ctx, tx.Conn, r.ID, p.Scope, r.Version)
		if e != nil {
			return nil, e
		}
		items = append(items, old)
		expected[r.ID] = current.Version
	}
	saved, op, err := tx.SaveMemories(ctx, "merge.undo", reason, items, expected)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "DELETE FROM relations WHERE operation_id=?", p.OperationID); err != nil {
		return nil, err
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE plans SET status='undone' WHERE id=?", id)
	return map[string]any{"memories": saved, "operation": op}, err
}

type Edge struct {
	From string `json:"from"`
	Kind string `json:"kind"`
	To   string `json:"to"`
}
type Graph struct {
	Nodes     []string `json:"nodes"`
	Edges     []Edge   `json:"edges"`
	Truncated bool     `json:"truncated"`
}

func MemoryGraph(ctx context.Context, q Queryer, id, scope string, depth, limit int) (Graph, error) {
	g := Graph{Nodes: []string{}, Edges: []Edge{}}
	if depth < 1 || depth > 5 || limit < 1 || limit > 1000 {
		return g, Fail("invalid_input", "depth must be 1..5 and limit 1..1000")
	}
	if _, err := ReadMemory(ctx, q, id, scope, 0); err != nil {
		return g, err
	}
	edges := []Edge{}
	rows, err := q.QueryContext(ctx, `SELECT r.src_id,r.kind,r.dst_id FROM relations r JOIN memories a ON a.id=r.src_id JOIN memories b ON b.id=r.dst_id WHERE a.workspace_id IN (?,'global') AND b.workspace_id IN (?,'global') UNION SELECT me.memory_id,'supported_by',f.source_id FROM memory_evidence me JOIN fragments f ON f.id=me.fragment_id JOIN memories m ON m.id=me.memory_id WHERE m.workspace_id IN (?,'global') UNION SELECT a.id,'derived_from',b.id FROM sources a JOIN sources b ON a.lineage=b.digest WHERE a.id<>b.id AND a.workspace_id IN (?,'global') AND b.workspace_id IN (?,'global') AND a.redacted=0 AND b.redacted=0`, scopeID(scope), scopeID(scope), scopeID(scope), scopeID(scope), scopeID(scope))
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var e Edge
		if err = rows.Scan(&e.From, &e.Kind, &e.To); err != nil {
			rows.Close()
			return g, err
		}
		edges = append(edges, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, err
	}
	seen := map[string]bool{id: true}
	used := map[string]bool{}
	for d := 0; d < depth; d++ {
		next := map[string]bool{}
		for _, e := range edges {
			if !seen[e.From] && !seen[e.To] {
				continue
			}
			key := JSON(e)
			if used[key] {
				continue
			}
			if len(g.Edges) >= limit {
				g.Truncated = true
				continue
			}
			g.Edges = append(g.Edges, e)
			used[key] = true
			next[e.From] = true
			next[e.To] = true
		}
		for n := range next {
			seen[n] = true
		}
	}
	for n := range seen {
		g.Nodes = append(g.Nodes, n)
	}
	sort.Strings(g.Nodes)
	return g, nil
}
func (g Graph) Mermaid() string {
	var b strings.Builder
	b.WriteString("graph LR\n")
	ids := map[string]string{}
	for i, n := range g.Nodes {
		ids[n] = fmt.Sprintf("n%d", i)
		fmt.Fprintf(&b, "  n%d[\"%s\"]\n", i, strings.ReplaceAll(n, "\"", "&quot;"))
	}
	for _, e := range g.Edges {
		fmt.Fprintf(&b, "  %s -->|%s| %s\n", ids[e.From], e.Kind, ids[e.To])
	}
	return b.String()
}
