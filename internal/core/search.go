package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type SearchOptions struct {
	Scope         string
	AllWorkspaces bool
	Kind          string
	Status        string
	Category      string
	Limit         int
	ValidAt       string
}
type Hit struct {
	Kind        string  `json:"kind"`
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Summary     string  `json:"summary"`
	Snippet     string  `json:"snippet"`
	WorkspaceID string  `json:"workspace_id"`
	Status      string  `json:"status,omitempty"`
	Score       float64 `json:"score"`
}

func ListMemories(ctx context.Context, q Queryer, opts SearchOptions) ([]Memory, error) {
	query := "SELECT document FROM memories WHERE 1=1"
	args := []any{}
	if !opts.AllWorkspaces {
		query += " AND workspace_id IN (?,'global')"
		args = append(args, scopeID(opts.Scope))
	}
	if opts.Status != "" {
		query += " AND status=?"
		args = append(args, opts.Status)
	}
	if opts.Category != "" {
		query += " AND category=?"
		args = append(args, opts.Category)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		return nil, Fail("invalid_input", "limit must not exceed 1000")
	}
	query += " ORDER BY updated_at DESC,id LIMIT ?"
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		var raw string
		var m Memory
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err = redactReadMemoryQuotes(ctx, q, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func Search(ctx context.Context, q Queryer, text string, opts SearchOptions) ([]Hit, error) {
	terms := strings.Fields(text)
	if len(terms) == 0 {
		return nil, Fail("invalid_input", "search query is required")
	}
	if len(terms) > 30 {
		return nil, Fail("invalid_input", "query has too many terms")
	}
	where := []string{"(search_fts.kind<>'source' OR (s.redacted=0 AND " + retainedSourcePredicate("s.id") + "))"}
	args := []any{}
	long := []string{}
	for _, term := range terms {
		if utf8.RuneCountInString(term) >= 3 {
			long = append(long, "\""+strings.ReplaceAll(term, "\"", "\"\"")+"\"")
		} else {
			where = append(where, "instr(lower(search_fts.title || ' ' || search_fts.summary || ' ' || search_fts.content),lower(?))>0")
			args = append(args, term)
		}
	}
	rank := "0.0"
	if len(long) > 0 {
		where = append(where, "search_fts MATCH ?")
		args = append(args, strings.Join(long, " AND "))
		rank = "bm25(search_fts,5.0,10.0,1.0)"
	}
	if !opts.AllWorkspaces {
		where = append(where, "search_fts.workspace_id IN (?,'global')")
		args = append(args, scopeID(opts.Scope))
	}
	if opts.Kind != "" {
		where = append(where, "search_fts.kind=?")
		args = append(args, opts.Kind)
	}
	if opts.Status != "" {
		where = append(where, "m.status=?")
		args = append(args, opts.Status)
	}
	if opts.Category != "" {
		where = append(where, "m.category=?")
		args = append(args, opts.Category)
	}
	if opts.ValidAt != "" {
		where = append(where, "(coalesce(json_extract(m.document,'$.valid_from'),'')='' OR julianday(json_extract(m.document,'$.valid_from'))<=julianday(?))", "(coalesce(json_extract(m.document,'$.valid_until'),'')='' OR julianday(json_extract(m.document,'$.valid_until'))>julianday(?))")
		args = append(args, opts.ValidAt, opts.ValidAt)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 1000 {
		return nil, Fail("invalid_input", "limit must not exceed 1000")
	}
	args = append(args, limit)
	query := "SELECT search_fts.kind,ref_id,search_fts.title,search_fts.summary,substr(search_fts.content,1,300),search_fts.workspace_id,coalesce(m.status,'')," + rank + " AS score FROM search_fts LEFT JOIN memories m ON search_fts.kind='memory' AND m.id=ref_id LEFT JOIN sources s ON search_fts.kind='source' AND s.id=ref_id WHERE " + strings.Join(where, " AND ") + " ORDER BY score,search_fts.kind,ref_id LIMIT ?"
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Hit{}
	for rows.Next() {
		var h Hit
		if err = rows.Scan(&h.Kind, &h.ID, &h.Title, &h.Summary, &h.Snippet, &h.WorkspaceID, &h.Status, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

type RecallItem struct {
	ID            string     `json:"id"`
	Version       int        `json:"version"`
	Title         string     `json:"title"`
	Summary       string     `json:"summary"`
	Applicability []string   `json:"applicability,omitempty"`
	Evidence      []Evidence `json:"evidence"`
	Reason        string     `json:"reason,omitempty"`
}
type RecallResult struct {
	Context     string       `json:"context"`
	Items       []RecallItem `json:"items"`
	UsedChars   int          `json:"used_chars"`
	BudgetChars int          `json:"budget_chars"`
	Skipped     int          `json:"skipped"`
	BudgetUnit  string       `json:"budget_unit"`
}

func Recall(ctx context.Context, q Queryer, text string, opts SearchOptions, budget int, explain bool) (RecallResult, error) {
	if db, ok := q.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return RecallResult{}, err
		}
		defer tx.Rollback()
		result, err := Recall(ctx, tx, text, opts, budget, explain)
		if err != nil {
			return result, err
		}
		return result, tx.Commit()
	}
	result := RecallResult{Items: []RecallItem{}, BudgetChars: budget, BudgetUnit: "Unicode characters in context"}
	if budget <= 0 {
		return result, Fail("invalid_input", "budget-chars must be positive")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 1000 {
		return result, Fail("invalid_input", "limit must not exceed 1000")
	}
	opts.Kind = "memory"
	opts.Status = "active"
	opts.ValidAt = Now()
	opts.Limit = 1000
	hits, err := Search(ctx, q, text, opts)
	if err != nil {
		return result, err
	}
	now := time.Now()
	for _, hit := range hits {
		if len(result.Items) >= limit {
			break
		}
		m, err := ReadMemory(ctx, q, hit.ID, hit.WorkspaceID, 0)
		if err != nil {
			return result, err
		}
		if m.ValidFrom != "" {
			t, _ := time.Parse(time.RFC3339, m.ValidFrom)
			if now.Before(t) {
				result.Skipped++
				continue
			}
		}
		if m.ValidUntil != "" {
			t, _ := time.Parse(time.RFC3339, m.ValidUntil)
			if !now.Before(t) {
				result.Skipped++
				continue
			}
		}
		block := fmt.Sprintf("## %s\n%s\n", m.Title, m.Summary)
		if len(m.Applicability) > 0 {
			block += "适用条件: " + strings.Join(m.Applicability, "；") + "\n"
		}
		block += fmt.Sprintf("引用: memory:%s@%d\n\n", m.ID, m.Version)
		size := utf8.RuneCountInString(block)
		if result.UsedChars+size > budget {
			result.Skipped++
			continue
		}
		item := RecallItem{ID: m.ID, Version: m.Version, Title: m.Title, Summary: m.Summary, Applicability: m.Applicability, Evidence: m.Evidence}
		if explain {
			item.Reason = "query matched; active, in scope and within known validity window; applicability requires caller judgment"
		}
		result.Items = append(result.Items, item)
		result.Context += block
		result.UsedChars += size
	}
	return result, nil
}
func (tx *Tx) Reindex(ctx context.Context) (any, error) {
	statements := []string{"DELETE FROM search_fts", "INSERT INTO search_fts(title,summary,content,kind,ref_id,workspace_id) SELECT title,summary,content,'memory',id,workspace_id FROM memories", "INSERT INTO search_fts(title,summary,content,kind,ref_id,workspace_id) SELECT kind,'',content,'source',id,workspace_id FROM sources WHERE redacted=0"}
	for _, stmt := range statements {
		if _, err := tx.Conn.ExecContext(ctx, stmt); err != nil {
			return nil, err
		}
	}
	return tx.Audit(ctx, "index.rebuild", "rebuild derived FTS from authoritative records", nil)
}
