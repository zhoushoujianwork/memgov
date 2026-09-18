package console

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// These are read-only presentation views, not a second card storage model.
type memoryCard struct {
	ID          string `json:"id"`
	Category    string `json:"category"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	Status      string `json:"status"`
	WorkspaceID string `json:"workspace_id"`
	UpdatedAt   string `json:"updated_at"`
	Version     int    `json:"version"`
}

func memoryScope(ctx context.Context, q core.Queryer, r *http.Request) (string, error) {
	scope := r.URL.Query().Get("workspace")
	if scope == "" || scope == "global" {
		return "global", nil
	}
	var id string
	err := q.QueryRowContext(ctx, "SELECT id FROM workspaces WHERE id=?", scope).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.Fail("not_found", "workspace not found")
	}
	return id, err
}

func memoryWorkspaces(ctx context.Context, q core.Queryer) (any, error) {
	rows, err := q.QueryContext(ctx, "SELECT id,name FROM workspaces WHERE id<>'global' ORDER BY name,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Workspace{}
	for rows.Next() {
		var w core.Workspace
		if err = rows.Scan(&w.ID, &w.Name); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func memoryList(ctx context.Context, q core.Queryer, r *http.Request) (any, error) {
	scope, err := memoryScope(ctx, q, r)
	if err != nil {
		return nil, err
	}
	v := r.URL.Query()
	if all := v.Get("all_workspaces"); all != "" && all != "true" && all != "false" {
		return nil, core.Fail("invalid_input", "invalid all_workspaces")
	}
	status := v.Get("status")
	if status == "" {
		status = "active"
	}
	if !slices.Contains([]string{"active", "disputed", "retired", "superseded", "all"}, status) {
		return nil, core.Fail("invalid_input", "invalid memory status")
	}
	category := v.Get("category")
	if category != "" && !slices.Contains([]string{"fact", "preference", "constraint", "decision", "procedure", "lesson"}, category) {
		return nil, core.Fail("invalid_input", "invalid memory category")
	}
	page := 1
	if raw := v.Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 1000000 {
			return nil, core.Fail("invalid_input", "invalid page")
		}
	}
	search := strings.TrimSpace(v.Get("q"))
	if len([]rune(search)) > 500 {
		return nil, core.Fail("invalid_input", "search exceeds 500 characters")
	}
	where := " WHERE 1=1"
	args := []any{}
	if v.Get("all_workspaces") != "true" {
		where += " AND workspace_id IN (?,'global')"
		args = append(args, scope)
	}
	if status != "all" {
		where += " AND status=?"
		args = append(args, status)
	}
	if category != "" {
		where += " AND category=?"
		args = append(args, category)
	}
	// Literal substring search: % and _ in a user's query are not wildcards.
	if search != "" {
		where += " AND (instr(lower(title),lower(?))>0 OR instr(lower(summary),lower(?))>0 OR instr(lower(content),lower(?))>0)"
		args = append(args, search, search, search)
	}
	var total int
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM memories"+where, args...).Scan(&total); err != nil {
		return nil, err
	}
	pages := (total + 23) / 24
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	rows, err := q.QueryContext(ctx, "SELECT id,category,title,summary,status,workspace_id,updated_at,version FROM memories"+where+" ORDER BY updated_at DESC,id LIMIT 24 OFFSET ?", append(args, (page-1)*24)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cards := []memoryCard{}
	for rows.Next() {
		var c memoryCard
		if err = rows.Scan(&c.ID, &c.Category, &c.Title, &c.Summary, &c.Status, &c.WorkspaceID, &c.UpdatedAt, &c.Version); err != nil {
			return nil, err
		}
		cards = append(cards, c)
	}
	return map[string]any{"cards": cards, "total": total, "page": page, "page_size": 24}, rows.Err()
}

type memorySource struct {
	core.Source
	ExpiresAt string `json:"expires_at,omitempty"`
}

// Re-check message visibility even before physical retention cleanup, including
// recalled/replaced source revisions. Keep the source identifier for tracing.
func readMemorySource(ctx context.Context, q core.Queryer, id, scope string) (memorySource, error) {
	s, err := core.ReadSource(ctx, q, id, scope)
	if core.ErrorCode(err) == "not_found" {
		return memorySource{Source: core.Source{ID: id, Availability: "unavailable"}}, nil
	}
	if err != nil {
		return memorySource{}, err
	}
	out := memorySource{Source: s}
	rows, err := q.QueryContext(ctx, "SELECT message_id FROM source_origins WHERE source_id=?", id)
	if err != nil {
		return out, err
	}
	ids := []string{}
	for rows.Next() {
		var mid string
		if err = rows.Scan(&mid); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, mid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if out.Availability == "" {
		out.Availability = "available"
	}
	for _, mid := range ids {
		m, visible, e := messageVisibility(ctx, q, mid, time.Now())
		if e != nil {
			return out, e
		}
		if m.ExpiresAt != "" && (out.ExpiresAt == "" || m.ExpiresAt < out.ExpiresAt) {
			out.ExpiresAt = m.ExpiresAt
		}
		if !visible || m.SourceID != id {
			out.Availability = "unavailable"
		}
	}
	if out.Availability != "available" {
		for i := range out.Fragments {
			out.Fragments[i].Content = ""
		}
	}
	return out, nil
}

func sanitizeMemoryEvidence(ctx context.Context, q core.Queryer, m *core.Memory, scope string) ([]memorySource, string, error) {
	sources := []memorySource{}
	seen := map[string]memorySource{}
	expires := ""
	for i, e := range m.Evidence {
		s, ok := seen[e.SourceID]
		if !ok {
			var err error
			s, err = readMemorySource(ctx, q, e.SourceID, scope)
			if err != nil {
				return nil, "", err
			}
			seen[e.SourceID] = s
			sources = append(sources, s)
		}
		if s.Availability != "available" {
			m.Evidence[i].Quote = ""
		}
		deadline, _ := time.Parse(time.RFC3339Nano, s.ExpiresAt)
		if deadline.After(time.Now()) && (expires == "" || s.ExpiresAt < expires) {
			expires = s.ExpiresAt
		}
	}
	return sources, expires, nil
}

func memoryDetail(ctx context.Context, q core.Queryer, r *http.Request) (any, error) {
	scope, err := memoryScope(ctx, q, r)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/memories/"), "/")
	if len(parts) > 2 || parts[0] == "" {
		return nil, core.Fail("not_found", "memory query not found")
	}
	if len(parts) == 2 && parts[1] != "history" && parts[1] != "sources" {
		return nil, core.Fail("not_found", "memory query not found")
	}
	version := 0
	if raw := r.URL.Query().Get("version"); raw != "" {
		version, err = strconv.Atoi(raw)
		if err != nil || version < 1 {
			return nil, core.Fail("invalid_input", "invalid version")
		}
	}
	m, err := core.ReadMemory(ctx, q, parts[0], scope, version)
	if err != nil {
		return nil, err
	}
	if len(parts) == 2 && parts[1] == "history" {
		revisions, e := core.HistoryWithOperations(ctx, q, m.ID, scope)
		if e != nil {
			return nil, e
		}
		expires := ""
		for i := range revisions {
			_, at, e := sanitizeMemoryEvidence(ctx, q, &revisions[i].Memory, scope)
			if e != nil {
				return nil, e
			}
			if at != "" && (expires == "" || at < expires) {
				expires = at
			}
		}
		return map[string]any{"revisions": revisions, "expires_at": expires}, nil
	}
	sources, expires, err := sanitizeMemoryEvidence(ctx, q, &m, scope)
	if err != nil {
		return nil, err
	}
	if len(parts) == 2 {
		return map[string]any{"sources": sources, "expires_at": expires}, nil
	}
	var updated string
	if version > 0 {
		err = q.QueryRowContext(ctx, "SELECT created_at FROM revisions WHERE memory_id=? AND version=?", m.ID, version).Scan(&updated)
	} else {
		err = q.QueryRowContext(ctx, "SELECT updated_at FROM memories WHERE id=?", m.ID).Scan(&updated)
	}
	return map[string]any{"memory": m, "updated_at": updated, "expires_at": expires}, err
}
