package core

import (
	"context"
	"strings"
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
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 1000 {
		return nil, Fail("invalid_input", "limit must not exceed 1000")
	}
	args = append(args, limit)
	query := "SELECT search_fts.kind,ref_id,search_fts.title,search_fts.summary,substr(search_fts.content,1,300),search_fts.workspace_id,''," + rank + " AS score FROM search_fts LEFT JOIN sources s ON search_fts.kind='source' AND s.id=ref_id WHERE " + strings.Join(where, " AND ") + " ORDER BY score,search_fts.kind,ref_id LIMIT ?"
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

func (tx *Tx) Reindex(ctx context.Context) (any, error) {
	for _, stmt := range []string{"DELETE FROM search_fts", "INSERT INTO search_fts(title,summary,content,kind,ref_id,workspace_id) SELECT kind,'',content,'source',id,workspace_id FROM sources WHERE redacted=0"} {
		if _, err := tx.Conn.ExecContext(ctx, stmt); err != nil {
			return nil, err
		}
	}
	return tx.Audit(ctx, "index.rebuild", "rebuild internal source index", nil)
}
