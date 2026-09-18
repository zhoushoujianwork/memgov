package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

type CandidateInput struct {
	Action          string `json:"action"`
	TargetID        string `json:"target_id,omitempty"`
	ExpectedVersion int    `json:"expected_version,omitempty"`
	Memory          Memory `json:"memory"`
	Reason          string `json:"reason"`
}
type Candidate struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	CandidateInput
	Digest          string `json:"digest"`
	Status          string `json:"status"`
	RunID           string `json:"legacy_run_id,omitempty"`
	Consolidated    bool   `json:"consolidated"`
	Generator       string `json:"generator,omitempty"`
	CreatedAt       string `json:"created_at"`
	AppliedMemoryID string `json:"applied_memory_id,omitempty"`
}

func candidateDigest(in CandidateInput) string { return Digest(in) }
func (tx *Tx) SubmitCandidate(ctx context.Context, in CandidateInput, runID, generator string) (Candidate, error) {
	if in.Action == "" {
		in.Action = "create"
	}
	if !contains([]string{"create", "update"}, in.Action) {
		return Candidate{}, Fail("invalid_input", "candidate action must be create or update")
	}
	if in.Reason == "" {
		return Candidate{}, Fail("invalid_input", "candidate reason is required")
	}
	if in.Memory.ID != "" || in.Memory.Version != 0 {
		return Candidate{}, Fail("invalid_input", "candidate memory must omit id and version; use target_id and expected_version")
	}
	if scopeID(in.Memory.WorkspaceID) != scopeID(tx.Request.Scope) {
		if in.Memory.WorkspaceID == "" && scopeID(tx.Request.Scope) != "global" {
			in.Memory.WorkspaceID = tx.Request.Scope
		} else {
			return Candidate{}, Fail("denied", "candidate workspace differs from request")
		}
	}
	if in.Memory.WorkspaceID == "global" {
		in.Memory.WorkspaceID = ""
	}
	if in.Memory.Status == "" {
		in.Memory.Status = "active"
	}
	if in.Action == "update" {
		if in.TargetID == "" || in.ExpectedVersion < 1 {
			return Candidate{}, Fail("invalid_input", "updates require target_id and expected_version")
		}
		m, err := ReadMemory(ctx, tx.Conn, in.TargetID, tx.Request.Scope, 0)
		if err != nil {
			return Candidate{}, err
		}
		if scopeID(m.WorkspaceID) != scopeID(tx.Request.Scope) {
			return Candidate{}, Fail("denied", "select the target workspace explicitly")
		}
		if m.Version != in.ExpectedVersion {
			return Candidate{}, Fail("conflict", "target version changed")
		}
	} else if in.TargetID != "" || in.ExpectedVersion != 0 {
		return Candidate{}, Fail("invalid_input", "create cannot specify target or expected version")
	}
	if err := ValidateMemory(in.Memory); err != nil {
		return Candidate{}, err
	}
	if err := checkEvidence(ctx, tx.Conn, in.Memory); err != nil {
		return Candidate{}, err
	}
	c := Candidate{ID: NewID(), WorkspaceID: scopeID(tx.Request.Scope), CandidateInput: in, Digest: candidateDigest(in), Status: "pending", RunID: runID, Consolidated: runID == "", Generator: generator, CreatedAt: Now()}
	var rid any
	if runID != "" {
		rid = runID
	}
	_, err := tx.Conn.ExecContext(ctx, "INSERT INTO candidates VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", c.ID, c.WorkspaceID, c.Action, c.TargetID, c.ExpectedVersion, JSON(c.Memory), c.Digest, c.Status, c.Reason, rid, c.Consolidated, c.Generator, c.CreatedAt, c.AppliedMemoryID)
	if err != nil {
		return c, err
	}
	_, err = tx.Audit(ctx, "candidate.submit", c.Reason, objectChange("candidate", c.ID))
	return c, err
}

const candidateColumns = "id,workspace_id,action,target_id,expected_version,document,digest,status,reason,coalesce(run_id,''),consolidated,generator,created_at,applied_memory_id"

type scanner interface{ Scan(...any) error }

func scanCandidate(row scanner) (Candidate, error) {
	var c Candidate
	var raw string
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.Action, &c.TargetID, &c.ExpectedVersion, &raw, &c.Digest, &c.Status, &c.Reason, &c.RunID, &c.Consolidated, &c.Generator, &c.CreatedAt, &c.AppliedMemoryID)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal([]byte(raw), &c.Memory)
	return c, err
}
func ReadCandidate(ctx context.Context, q Queryer, id, scope string) (Candidate, error) {
	c, err := scanCandidate(q.QueryRowContext(ctx, "SELECT "+candidateColumns+" FROM candidates WHERE id=? AND workspace_id=?", id, scopeID(scope)))
	if errors.Is(err, sql.ErrNoRows) {
		return c, Fail("not_found", "candidate not found in this workspace")
	}
	if err == nil {
		err = redactReadMemoryQuotes(ctx, q, &c.Memory)
	}
	return c, err
}
func CandidateList(ctx context.Context, q Queryer, scope, status string) ([]Candidate, error) {
	query := "SELECT " + candidateColumns + " FROM candidates WHERE workspace_id=?"
	args := []any{scopeID(scope)}
	if status != "" {
		query += " AND status=?"
		args = append(args, status)
	}
	query += " ORDER BY created_at,id"
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Candidate{}
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err = redactReadMemoryQuotes(ctx, q, &out[i].Memory); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (tx *Tx) ValidateCandidate(ctx context.Context, id string) (Candidate, error) {
	c, err := ReadCandidate(ctx, tx.Conn, id, tx.Request.Scope)
	if err != nil {
		return c, err
	}
	if err = ValidateMemory(c.Memory); err != nil {
		return c, err
	}
	if err = checkEvidence(ctx, tx.Conn, c.Memory); err != nil {
		return c, err
	}
	if c.Action == "update" {
		m, e := ReadMemory(ctx, tx.Conn, c.TargetID, tx.Request.Scope, 0)
		if e != nil {
			return c, e
		}
		if m.Version != c.ExpectedVersion {
			return c, Fail("conflict", "target revision changed")
		}
	}
	return c, nil
}
func (tx *Tx) ApplyCandidate(ctx context.Context, id, digest string) (any, error) {
	c, err := ReadCandidate(ctx, tx.Conn, id, tx.Request.Scope)
	if err != nil {
		return nil, err
	}
	if digest == "" || c.Digest != digest {
		return nil, Fail("conflict", "expected-digest must match the reviewed candidate")
	}
	if c.Status == "applied" {
		m, err := ReadMemory(ctx, tx.Conn, c.AppliedMemoryID, tx.Request.Scope, 0)
		return map[string]any{"memory": m, "already_applied": true}, err
	}
	if c.Status != "pending" && c.Status != "accepted" {
		return nil, Fail("conflict", "candidate is %s", c.Status)
	}
	if c.RunID != "" {
		var n int
		if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM reviews WHERE candidate_id=? AND candidate_digest=? AND decision='accept' AND reviewer<>?", c.ID, c.Digest, c.Generator).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 || !c.Consolidated || c.Status != "accepted" {
			return nil, Fail("denied", "legacy automated candidate requires consolidation and independent acceptance")
		}
	}
	if _, err = tx.ValidateCandidate(ctx, id); err != nil {
		return nil, err
	}
	m := c.Memory
	m.ID = c.TargetID
	if m.ID == "" {
		m.ID = NewID()
	}
	// Exact duplicates are reported rather than silently creating a second fact.
	var duplicate string
	err = tx.Conn.QueryRowContext(ctx, "SELECT id FROM memories WHERE workspace_id=? AND content=? AND id<>? AND status='active' LIMIT 1", c.WorkspaceID, m.Content, m.ID).Scan(&duplicate)
	if err == nil {
		return nil, Fail("conflict", "identical active memory already exists: %s", duplicate)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	items, op, err := tx.SaveMemories(ctx, "candidate.apply", c.Reason, []Memory{m}, map[string]int{m.ID: c.ExpectedVersion})
	if err != nil {
		return nil, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE candidates SET status='applied',applied_memory_id=? WHERE id=?", m.ID, c.ID); err != nil {
		return nil, err
	}
	return map[string]any{"memory": items[0], "operation": op}, nil
}
func (tx *Tx) RejectCandidate(ctx context.Context, id, reason string) (any, error) {
	if reason == "" {
		return nil, Fail("invalid_input", "reason is required")
	}
	c, err := ReadCandidate(ctx, tx.Conn, id, tx.Request.Scope)
	if err != nil {
		return nil, err
	}
	if c.Status == "applied" {
		return nil, Fail("conflict", "applied candidate cannot be rejected")
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE candidates SET status='rejected' WHERE id=?", id); err != nil {
		return nil, err
	}
	return tx.Audit(ctx, "candidate.reject", reason, objectChange("candidate", id))
}

// SubmitCandidateReview records the independent runtime review against the exact
// candidate digest. Runtime-generated candidates use the same review ledger;
// explicit candidates do not require it for structural apply validation.
func (tx *Tx) SubmitCandidateReview(ctx context.Context, id, reviewer, decision string, issues []string) (Candidate, error) {
	c, err := ReadCandidate(ctx, tx.Conn, id, tx.Request.Scope)
	if err != nil {
		return c, err
	}
	if strings.TrimSpace(reviewer) == "" || !contains([]string{"accept", "reject"}, decision) {
		return c, Fail("invalid_input", "reviewer and accept/reject decision are required")
	}
	if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO reviews VALUES(?,?,?,?,?,?,?)", NewID(), c.ID, c.Digest, reviewer, decision, JSON(issues), Now()); err != nil {
		return c, err
	}
	status := "accepted"
	if decision == "reject" {
		status = "rejected"
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE candidates SET status=? WHERE id=? AND status IN ('pending','accepted')", status, id); err != nil {
		return c, err
	}
	if _, err = tx.Audit(ctx, "candidate.review", reviewer+":"+decision, objectChange("candidate", id)); err != nil {
		return c, err
	}
	return ReadCandidate(ctx, tx.Conn, id, tx.Request.Scope)
}
