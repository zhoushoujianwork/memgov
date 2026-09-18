package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type SourceInput struct {
	Kind       string `json:"kind"`
	URI        string `json:"uri"`
	Content    string `json:"content"`
	ObservedAt string `json:"observed_at,omitempty"`
	Lineage    string `json:"lineage,omitempty"`
}
type Fragment struct {
	ID       string `json:"id"`
	SourceID string `json:"source_id"`
	Locator  string `json:"locator"`
	Content  string `json:"content"`
	SHA256   string `json:"sha256"`
}
type Source struct {
	ID           string     `json:"id"`
	WorkspaceID  string     `json:"workspace_id"`
	Kind         string     `json:"kind"`
	SHA256       string     `json:"sha256"`
	ObservedAt   string     `json:"observed_at,omitempty"`
	CapturedAt   string     `json:"captured_at"`
	Lineage      string     `json:"lineage,omitempty"`
	Availability string     `json:"availability,omitempty"`
	Locations    []string   `json:"locations"`
	Fragments    []Fragment `json:"fragments"`
}

func scopeID(value string) string {
	if value == "" {
		return "global"
	}
	return value
}
func SourceList(ctx context.Context, q Queryer, scope string, limit int) ([]Source, error) {
	if limit < 1 || limit > 1000 {
		return nil, Fail("invalid_input", "limit must be 1..1000")
	}
	rows, err := q.QueryContext(ctx, "SELECT id,workspace_id,kind,digest,observed_at,captured_at,lineage FROM sources WHERE workspace_id IN (?,'global') AND redacted=0 ORDER BY captured_at DESC,id LIMIT ?", scopeID(scope), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Source{}
	for rows.Next() {
		var s Source
		if err = rows.Scan(&s.ID, &s.WorkspaceID, &s.Kind, &s.SHA256, &s.ObservedAt, &s.CapturedAt, &s.Lineage); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func (tx *Tx) Ingest(ctx context.Context, in SourceInput) (Source, error) {
	if !utf8.ValidString(in.Content) {
		return Source{}, Fail("invalid_input", "source content must be valid UTF-8; use an explicit encoded snapshot for raw bytes")
	}
	if strings.TrimSpace(in.Content) == "" || strings.TrimSpace(in.URI) == "" {
		return Source{}, Fail("invalid_input", "source uri and content are required")
	}
	if in.ObservedAt != "" {
		if _, err := time.Parse(time.RFC3339, in.ObservedAt); err != nil {
			return Source{}, Fail("invalid_input", "observed_at must use RFC3339")
		}
	}
	if in.Kind == "" {
		in.Kind = "note"
	}
	digest := Hash([]byte(in.Content))
	scope := scopeID(tx.Request.Scope)
	for _, pair := range [][2]string{{"source_digest", digest}, {"source_uri", Hash([]byte(in.URI))}, {"source_lineage", in.Lineage}, {"content", Hash([]byte(strings.TrimSpace(in.Content)))}} {
		var n int
		if err := tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM tombstones WHERE kind=? AND fingerprint=?", pair[0], pair[1]).Scan(&n); err != nil {
			return Source{}, err
		}
		if n > 0 {
			return Source{}, Fail("denied", "source matches a purge tombstone")
		}
	}
	var id string
	err := tx.Conn.QueryRowContext(ctx, "SELECT id FROM sources WHERE workspace_id=? AND digest=?", scope, digest).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = NewID()
		if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO sources(id,workspace_id,kind,content,digest,observed_at,captured_at,lineage) VALUES(?,?,?,?,?,?,?,?)", id, scope, in.Kind, in.Content, digest, in.ObservedAt, Now(), in.Lineage); err != nil {
			return Source{}, err
		}
		lines := strings.SplitAfter(in.Content, "\n")
		start := 1
		var chunk strings.Builder
		flush := func(end int) error {
			if chunk.Len() == 0 {
				return nil
			}
			value := chunk.String()
			locator := fmt.Sprintf("lines:%d-%d", start, end)
			parts := []rune(value)
			for offset := 0; offset < len(parts); offset += 6000 {
				endPart := offset + 6000
				if endPart > len(parts) {
					endPart = len(parts)
				}
				body := string(parts[offset:endPart])
				loc := locator
				if len(parts) > 6000 {
					loc += fmt.Sprintf("/runes:%d-%d", offset, endPart)
				}
				fid := Hash([]byte(id + "\x00" + loc))
				if _, err := tx.Conn.ExecContext(ctx, "INSERT INTO fragments VALUES(?,?,?,?,?)", fid, id, loc, body, Hash([]byte(body))); err != nil {
					return err
				}
			}
			chunk.Reset()
			return nil
		}
		for i, line := range lines {
			if chunk.Len() > 0 && utf8.RuneCountInString(chunk.String())+utf8.RuneCountInString(line) > 6000 {
				if err = flush(i); err != nil {
					return Source{}, err
				}
				start = i + 1
			}
			chunk.WriteString(line)
		}
		if err = flush(len(lines)); err != nil {
			return Source{}, err
		}
	} else if err != nil {
		return Source{}, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO source_locations VALUES(?,?)", id, in.URI); err != nil {
		return Source{}, err
	}
	if _, err = tx.Audit(ctx, "source.ingest", in.URI, objectChange("source", id)); err != nil {
		return Source{}, err
	}
	return ReadSource(ctx, tx.Conn, id, scope)
}
func ReadSource(ctx context.Context, q Queryer, id, scope string) (Source, error) {
	var s Source
	err := q.QueryRowContext(ctx, "SELECT id,workspace_id,kind,digest,observed_at,captured_at,lineage FROM sources WHERE id=? AND workspace_id IN (?, 'global') AND (redacted=0 OR EXISTS(SELECT 1 FROM source_availability av WHERE av.source_id=sources.id AND av.state='expired'))", id, scopeID(scope)).Scan(&s.ID, &s.WorkspaceID, &s.Kind, &s.SHA256, &s.ObservedAt, &s.CapturedAt, &s.Lineage)
	if errors.Is(err, sql.ErrNoRows) {
		return s, Fail("not_found", "source %s not found in this workspace", id)
	}
	if err != nil {
		return s, err
	}
	expired, err := sourceRawExpired(ctx, q, id)
	if err != nil {
		return s, err
	}
	if expired {
		s.Availability = "expired"
	}
	s.Locations = []string{}
	s.Fragments = []Fragment{}
	rows, err := q.QueryContext(ctx, "SELECT uri FROM source_locations WHERE source_id=? ORDER BY uri", id)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var uri string
		if err = rows.Scan(&uri); err != nil {
			rows.Close()
			return s, err
		}
		s.Locations = append(s.Locations, uri)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s, err
	}
	rows, err = q.QueryContext(ctx, "SELECT id,source_id,locator,content,digest FROM fragments WHERE source_id=? ORDER BY rowid", id)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var f Fragment
		if err = rows.Scan(&f.ID, &f.SourceID, &f.Locator, &f.Content, &f.SHA256); err != nil {
			return s, err
		}
		if expired {
			f.Content = ""
		}
		s.Fragments = append(s.Fragments, f)
	}
	return s, rows.Err()
}
func checkEvidence(ctx context.Context, q Queryer, m Memory) error {
	for _, e := range m.Evidence {
		expired, expiryErr := sourceRawExpired(ctx, q, e.SourceID)
		if expiryErr != nil {
			return expiryErr
		}
		if expired {
			return Fail("denied", "evidence raw text expired; re-query platform before applying: %s", e.SourceID)
		}
		var source, hash, content, scope string
		err := q.QueryRowContext(ctx, "SELECT f.source_id,f.digest,f.content,s.workspace_id FROM fragments f JOIN sources s ON s.id=f.source_id WHERE f.id=? AND s.redacted=0", e.FragmentID).Scan(&source, &hash, &content, &scope)
		if errors.Is(err, sql.ErrNoRows) {
			return Fail("invalid_input", "evidence fragment %s does not exist", e.FragmentID)
		}
		if err != nil {
			return err
		}
		if e.SourceID != source || e.SHA256 != hash {
			return Fail("invalid_input", "evidence identity or digest mismatch: %s", e.FragmentID)
		}
		if scope != "global" && scope != scopeID(m.WorkspaceID) {
			return Fail("denied", "evidence cannot be promoted across workspaces")
		}
		if e.Quote != "" && !strings.Contains(content, e.Quote) {
			return Fail("invalid_input", "evidence quote is not present in source fragment")
		}
	}
	var n int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM tombstones WHERE (kind='memory_id' AND fingerprint=?) OR (kind='content' AND fingerprint=?)", m.ID, Hash([]byte(strings.TrimSpace(m.Content)))).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return Fail("denied", "memory matches a purge tombstone")
	}
	return nil
}
