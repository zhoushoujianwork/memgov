package core

import (
	"context"
	"encoding/json"
)

func objectChange(kind, id string) []Change { return []Change{{ObjectType: kind, ObjectID: id}} }

type AuditedRevision struct {
	Memory
	Operation Operation `json:"operation"`
}

// Revision audit facts are returned with the exact historical memory version.
func HistoryWithOperations(ctx context.Context, q Queryer, id, scope string) ([]AuditedRevision, error) {
	if _, err := ReadMemory(ctx, q, id, scope, 0); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT r.document,o.id,o.request_id,o.kind,o.actor,o.reason,o.changes,o.created_at FROM revisions r JOIN operations o ON o.id=r.operation_id WHERE r.memory_id=? ORDER BY r.version`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditedRevision{}
	for rows.Next() {
		var v AuditedRevision
		var raw, changes string
		o := &v.Operation
		if err = rows.Scan(&raw, &o.ID, &o.RequestID, &o.Kind, &o.Actor, &o.Reason, &changes, &o.CreatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &v.Memory); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(changes), &o.Changes); err != nil {
			return nil, err
		}
		out = append(out, v)
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

type CandidateReview struct {
	ID              string   `json:"id"`
	CandidateID     string   `json:"candidate_id"`
	CandidateDigest string   `json:"candidate_digest"`
	Reviewer        string   `json:"reviewer"`
	Decision        string   `json:"decision"`
	Issues          []string `json:"issues"`
	CreatedAt       string   `json:"created_at"`
}

func CandidateReviews(ctx context.Context, q Queryer, id, scope string) ([]CandidateReview, error) {
	if _, err := ReadCandidate(ctx, q, id, scope); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, "SELECT id,candidate_id,candidate_digest,reviewer,decision,issues,created_at FROM reviews WHERE candidate_id=? ORDER BY created_at,id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CandidateReview{}
	for rows.Next() {
		var r CandidateReview
		var issues string
		if err = rows.Scan(&r.ID, &r.CandidateID, &r.CandidateDigest, &r.Reviewer, &r.Decision, &issues, &r.CreatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(issues), &r.Issues); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
