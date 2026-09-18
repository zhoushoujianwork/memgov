package core

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

type Tombstone struct {
	Kind        string `json:"kind"`
	Fingerprint string `json:"fingerprint"`
	OperationID string `json:"operation_id"`
	CreatedAt   string `json:"created_at"`
}
type PurgeManifest struct {
	Memories   map[string]int `json:"memories"`
	Sources    []string       `json:"source_ids"`
	Candidates []string       `json:"candidate_ids"`
	Jobs       []string       `json:"job_ids"`
	Plans      []string       `json:"plan_ids"`
	Operations []string       `json:"operation_ids"`
	Tombstones []Tombstone    `json:"tombstones"`
}
type PurgePlan struct {
	ID       string        `json:"id"`
	Digest   string        `json:"digest"`
	Scope    string        `json:"workspace_id"`
	Seeds    []string      `json:"memory_ids"`
	Manifest PurgeManifest `json:"manifest"`
	Status   string        `json:"status"`
}

func keys(m map[string]bool) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func intersects(values []string, set map[string]bool) bool {
	for _, v := range values {
		if set[v] {
			return true
		}
	}
	return false
}
func evidenceIntersects(e []Evidence, set map[string]bool) bool {
	for _, v := range e {
		if set[v.SourceID] {
			return true
		}
	}
	return false
}
func containsPayload(raw string, bodies map[string]bool) bool {
	for body := range bodies {
		if body != "" && strings.Contains(raw, body) {
			return true
		}
	}
	return false
}

// Derive the complete owned-copy closure. Shared evidence can affect other
// memories; the preview enumerates that collateral effect before application.
func purgeClosure(ctx context.Context, q Queryer, memorySeeds, sourceSeeds []string) (PurgeManifest, []string, error) {
	p := PurgeManifest{Memories: map[string]int{}, Sources: []string{}, Candidates: []string{}, Jobs: []string{}, Plans: []string{}, Operations: []string{}, Tombstones: []Tombstone{}}
	ms, ss, cs, js, ps, ops := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	bodies := map[string]bool{}
	for _, id := range memorySeeds {
		ms[id] = true
	}
	for _, id := range sourceSeeds {
		ss[id] = true
	}
	type version struct {
		m  Memory
		op string
	}
	versions := []version{}
	rows, err := q.QueryContext(ctx, "SELECT document,operation_id FROM revisions")
	if err != nil {
		return p, nil, err
	}
	for rows.Next() {
		var raw, op string
		if err = rows.Scan(&raw, &op); err != nil {
			rows.Close()
			return p, nil, err
		}
		var m Memory
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			rows.Close()
			return p, nil, err
		}
		versions = append(versions, version{m, op})
		if ms[m.ID] {
			bodies[strings.TrimSpace(m.Content)] = true
			for _, e := range m.Evidence {
				ss[e.SourceID] = true
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, nil, err
	}
	type source struct{ id, body, digest, lineage string }
	sources := []source{}
	rows, err = q.QueryContext(ctx, "SELECT id,content,digest,lineage FROM sources WHERE redacted=0")
	if err != nil {
		return p, nil, err
	}
	for rows.Next() {
		var s source
		if err = rows.Scan(&s.id, &s.body, &s.digest, &s.lineage); err != nil {
			rows.Close()
			return p, nil, err
		}
		sources = append(sources, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, nil, err
	}
	// Exact source duplicates and declared derivatives carry the same purge scope.
	lineage := map[string]bool{}
	for changed := true; changed; {
		changed = false
		before := len(lineage)
		for _, s := range sources {
			if ss[s.id] || lineage[s.digest] || (s.lineage != "" && lineage[s.lineage]) || containsPayload(s.body, bodies) {
				if !ss[s.id] {
					ss[s.id] = true
					changed = true
				}
				bodies[strings.TrimSpace(s.body)] = true
				lineage[s.digest] = true
				if s.lineage != "" {
					lineage[s.lineage] = true
				}
			}
		}
		changed = changed || len(lineage) != before
	}
	for changed := true; changed; {
		changed = false
		for _, v := range versions {
			if ms[v.m.ID] || evidenceIntersects(v.m.Evidence, ss) || containsPayload(v.m.Content, bodies) {
				if !ms[v.m.ID] {
					ms[v.m.ID] = true
					changed = true
				}
				bodies[strings.TrimSpace(v.m.Content)] = true
				ops[v.op] = true
			}
		}
	}
	tombs := map[string]Tombstone{}
	addTomb := func(kind, value string) {
		if value != "" {
			tombs[kind+":"+value] = Tombstone{Kind: kind, Fingerprint: value}
		}
	}
	for id := range ms {
		var ver int
		if err = q.QueryRowContext(ctx, "SELECT version FROM memories WHERE id=?", id).Scan(&ver); err != nil {
			return p, nil, err
		}
		p.Memories[id] = ver
		addTomb("memory_id", id)
	}
	for b := range bodies {
		if b != "" {
			addTomb("content", Hash([]byte(b)))
		}
	}
	for _, s := range sources {
		if ss[s.id] {
			addTomb("source_id", s.id)
			addTomb("source_digest", s.digest)
			if s.lineage != "" {
				addTomb("source_lineage", s.lineage)
			}
		}
	}
	locations := []string{}
	rows, err = q.QueryContext(ctx, "SELECT source_id,uri FROM source_locations")
	if err != nil {
		return p, nil, err
	}
	for rows.Next() {
		var id, uri string
		if err = rows.Scan(&id, &uri); err != nil {
			rows.Close()
			return p, nil, err
		}
		if ss[id] {
			addTomb("source_uri", Hash([]byte(uri)))
			locations = append(locations, uri)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, nil, err
	}
	rows, err = q.QueryContext(ctx, "SELECT "+candidateColumns+" FROM candidates WHERE status<>'purged'")
	if err != nil {
		return p, nil, err
	}
	for rows.Next() {
		c, e := scanCandidate(rows)
		if e != nil {
			rows.Close()
			return p, nil, e
		}
		if ms[c.TargetID] || ms[c.AppliedMemoryID] || evidenceIntersects(c.Memory.Evidence, ss) || containsPayload(c.Memory.Content, bodies) {
			cs[c.ID] = true
			bodies[strings.TrimSpace(c.Memory.Content)] = true
			addTomb("content", Hash([]byte(strings.TrimSpace(c.Memory.Content))))
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, nil, err
	}
	rows, err = q.QueryContext(ctx, "SELECT "+legacyJobColumns+" FROM jobs WHERE status<>'purged'")
	if err != nil {
		return p, nil, err
	}
	for rows.Next() {
		j, e := scanLegacyJob(rows)
		if e != nil {
			rows.Close()
			return p, nil, e
		}
		if intersects(j.Input.SourceIDs, ss) || intersects(j.Input.ContextSourceIDs, ss) || intersects(j.Input.CandidateIDs, cs) || containsPayload(string(j.Output), bodies) {
			js[j.ID] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, nil, err
	}
	rows, err = q.QueryContext(ctx, "SELECT id,kind,document,operation_id FROM plans WHERE status<>'purged'")
	if err != nil {
		return p, nil, err
	}
	for rows.Next() {
		var id, kind, raw, op string
		if err = rows.Scan(&id, &kind, &raw, &op); err != nil {
			rows.Close()
			return p, nil, err
		}
		if kind == "purge" {
			continue
		}
		hit := containsPayload(raw, bodies)
		for m := range ms {
			hit = hit || strings.Contains(raw, m)
		}
		if hit {
			ps[id] = true
			if op != "" {
				ops[op] = true
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, nil, err
	}
	p.Sources = keys(ss)
	p.Candidates = keys(cs)
	p.Jobs = keys(js)
	p.Plans = keys(ps)
	p.Operations = keys(ops)
	tk := []string{}
	for k := range tombs {
		tk = append(tk, k)
	}
	sort.Strings(tk)
	for _, k := range tk {
		p.Tombstones = append(p.Tombstones, tombs[k])
	}
	sort.Strings(locations)
	return p, locations, nil
}
func (tx *Tx) PreviewPurge(ctx context.Context, id string, expected int) (any, error) {
	m, err := ReadMemory(ctx, tx.Conn, id, tx.Request.Scope, 0)
	if err != nil {
		return nil, err
	}
	if scopeID(m.WorkspaceID) != scopeID(tx.Request.Scope) {
		return nil, Fail("denied", "select the memory workspace")
	}
	if expected < 1 || m.Version != expected {
		return nil, Fail("conflict", "expected-version must match the current memory")
	}
	manifest, locations, err := purgeClosure(ctx, tx.Conn, []string{id}, nil)
	if err != nil {
		return nil, err
	}
	p := PurgePlan{ID: NewID(), Scope: scopeID(tx.Request.Scope), Seeds: []string{id}, Manifest: manifest, Status: "pending"}
	p.Digest = Digest(p.Manifest)
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO plans(id,kind,document,digest,created_at) VALUES(?,'purge',?,?,?)", p.ID, JSON(p), p.Digest, Now())
	return map[string]any{"plan": p, "external_locations": locations, "cache_and_audit_effect": "All cached responses and free-form audit annotations are erased; operation facts and versions remain.", "external_residuals": "Original files, previous backups and exports are outside this purge. Physical storage erasure is not claimed."}, err
}
func (tx *Tx) ApplyPurge(ctx context.Context, id, digest string) (any, error) {
	var raw, status string
	if err := tx.Conn.QueryRowContext(ctx, "SELECT document,status FROM plans WHERE id=? AND kind='purge'", id).Scan(&raw, &status); err != nil {
		return nil, Fail("not_found", "purge plan not found")
	}
	var p PurgePlan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, err
	}
	if p.Scope != scopeID(tx.Request.Scope) {
		return nil, Fail("denied", "select the purge workspace")
	}
	if digest == "" || digest != p.Digest {
		return nil, Fail("conflict", "expected-digest must match the purge preview")
	}
	if status == "applied" {
		return PurgeStatus(ctx, tx.Conn, id)
	}
	current, _, err := purgeClosure(ctx, tx.Conn, p.Seeds, nil)
	if err != nil {
		return nil, err
	}
	if Digest(current) != p.Digest {
		return nil, Fail("conflict", "purge dependencies changed; generate a fresh preview")
	}
	op, err := tx.Audit(ctx, "memory.purge", "sensitive payload removal", nil)
	if err != nil {
		return nil, err
	}
	if err = tx.redact(ctx, current, op.ID); err != nil {
		return nil, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO purge_runs VALUES(?,'logical_complete',?,?)", id, JSON(current), Now()); err != nil {
		return nil, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE plans SET status='applied',operation_id=? WHERE id=?", op.ID, id); err != nil {
		return nil, err
	}
	return PurgeStatus(ctx, tx.Conn, id)
}
func (tx *Tx) redact(ctx context.Context, p PurgeManifest, operation string) error {
	exec := func(sql string, args ...any) error { _, err := tx.Conn.ExecContext(ctx, sql, args...); return err }
	for _, t := range p.Tombstones {
		if err := exec("INSERT OR IGNORE INTO tombstones VALUES(?,?,?,?)", t.Kind, t.Fingerprint, operation, Now()); err != nil {
			return err
		}
	}
	for _, id := range p.Jobs {
		j, err := readLegacyJob(ctx, tx.Conn, id)
		if err != nil {
			return err
		}
		j.Input.Issues = nil
		if err = exec("UPDATE jobs SET input=?,output='',status='purged',worker='',lease_token='',lease_until='' WHERE id=?", JSON(j.Input), id); err != nil {
			return err
		}
	}
	for _, id := range p.Candidates {
		if err := exec("UPDATE reviews SET issues='[]',reviewer='redacted' WHERE candidate_id=?", id); err != nil {
			return err
		}
		if err := exec("UPDATE candidates SET document='{}',reason='redacted',generator='',status='purged' WHERE id=?", id); err != nil {
			return err
		}
	}
	for _, id := range p.Plans {
		if err := exec("UPDATE plans SET document='{}',status='purged' WHERE id=?", id); err != nil {
			return err
		}
	}
	for id := range p.Memories {
		for _, stmt := range []string{"DELETE FROM memory_evidence WHERE memory_id=?", "DELETE FROM revisions WHERE memory_id=?", "DELETE FROM relations WHERE src_id=? OR dst_id=?", "DELETE FROM memories WHERE id=?"} {
			args := []any{id}
			if strings.Contains(stmt, " OR ") {
				args = append(args, id)
			}
			if err := exec(stmt, args...); err != nil {
				return err
			}
		}
	}
	for _, id := range p.Sources {
		if err := exec("DELETE FROM fragments WHERE source_id=?", id); err != nil {
			return err
		}
		if err := exec("UPDATE sources SET kind='redacted',content='',observed_at='',lineage='',redacted=1 WHERE id=?", id); err != nil {
			return err
		}
		if err := exec("DELETE FROM source_locations WHERE source_id=?", id); err != nil {
			return err
		}
		if err := exec("UPDATE migration_sources SET disposition='purged',reason='removed by purge' WHERE source_id=?", id); err != nil {
			return err
		}
	}
	for _, id := range p.Operations {
		if err := exec("UPDATE operations SET reason='redacted by purge',actor='redacted' WHERE id=?", id); err != nil {
			return err
		}
	}
	// Request results and free-form audit annotations can contain copies from any
	// affected command. Clear payload-bearing cache/annotations conservatively;
	// retain operation IDs, kinds, timestamps and versions with redacted actor labels.
	if err := exec("DELETE FROM idempotency"); err != nil {
		return err
	}
	if err := exec("UPDATE operations SET reason='redacted by purge',actor='redacted'"); err != nil {
		return err
	}
	if err := exec("UPDATE requests SET actor='redacted'"); err != nil {
		return err
	}
	if _, err := tx.Reindex(ctx); err != nil {
		return err
	}
	return nil
}
func PurgeStatus(ctx context.Context, q Queryer, id string) (any, error) {
	var status, created, raw string
	if err := q.QueryRowContext(ctx, "SELECT status,manifest,created_at FROM purge_runs WHERE id=?", id).Scan(&status, &raw, &created); err != nil {
		return nil, Fail("not_found", "purge run not found")
	}
	return map[string]any{"id": id, "status": status, "created_at": created, "manifest": json.RawMessage(raw), "next_action": "memory purge apply with the same plan ID resumes physical database cleanup; external files remain outside this scope"}, nil
}
func (s *Store) CompactPurge(ctx context.Context, id string) (any, error) {
	if !s.exclusive {
		return nil, Fail("denied", "physical cleanup requires an exclusive maintenance connection")
	}
	if _, err := PurgeStatus(ctx, s.DB, id); err != nil {
		return nil, err
	}
	if err := checkpoint(ctx, s.DB); err != nil {
		return nil, err
	}
	if _, err := s.DB.ExecContext(ctx, "VACUUM"); err != nil {
		return nil, dbError(err)
	}
	if err := checkpoint(ctx, s.DB); err != nil {
		return nil, err
	}
	_, err := s.Mutate(ctx, Request{Command: "purge.compact", Scope: "global"}, func(tx *Tx) (any, error) {
		_, e := tx.Conn.ExecContext(ctx, "UPDATE purge_runs SET status='database_compacted' WHERE id=?", id)
		return map[string]string{"id": id}, e
	})
	if err != nil {
		return nil, err
	}
	if err = checkpoint(ctx, s.DB); err != nil {
		return nil, err
	}
	return PurgeStatus(ctx, s.DB, id)
}
func checkpoint(ctx context.Context, q Queryer) error {
	var busy, log, done int
	if err := q.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &done); err != nil {
		return dbError(err)
	}
	if busy != 0 {
		return Fail("unavailable", "WAL checkpoint is busy; logical purge is complete, retry cleanup")
	}
	return nil
}
