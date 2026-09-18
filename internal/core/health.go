package core

import (
	"context"
	"encoding/json"
	"strings"
)

func checkDerivedAndEvidence(ctx context.Context, q Queryer) ([]string, error) {
	issues := []string{}
	const expected = `WITH expected AS (SELECT 'memory' AS kind,id AS ref_id,title,summary,content,workspace_id FROM memories UNION ALL SELECT 'source',id,kind,'',content,workspace_id FROM sources WHERE redacted=0) `
	const cols = "kind,ref_id,title,summary,content,workspace_id"
	for _, query := range []string{expected + "SELECT count(*) FROM (SELECT * FROM expected EXCEPT SELECT " + cols + " FROM search_fts)", expected + "SELECT count(*) FROM (SELECT " + cols + " FROM search_fts EXCEPT SELECT * FROM expected)", "SELECT count(*) FROM (SELECT kind,ref_id,count(*) n FROM search_fts GROUP BY kind,ref_id HAVING n<>1)"} {
		var n int
		if err := q.QueryRowContext(ctx, query).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			issues = append(issues, "FTS differs from authoritative data; run index rebuild")
			break
		}
	}
	rows, err := q.QueryContext(ctx, "SELECT m.id,m.version,m.document,coalesce(r.document,''),coalesce(r.digest,'') FROM memories m LEFT JOIN revisions r ON r.memory_id=m.id AND r.version=m.version")
	if err != nil {
		return nil, err
	}
	type item struct {
		id                    string
		version               int
		raw, revision, digest string
	}
	items := []item{}
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.version, &i.raw, &i.revision, &i.digest); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, i := range items {
		var m Memory
		if e := json.Unmarshal([]byte(i.raw), &m); e != nil {
			issues = append(issues, "invalid memory JSON: "+i.id)
			continue
		}
		if i.raw != i.revision || Digest(m) != i.digest || m.Version != i.version || m.ID != i.id {
			issues = append(issues, "current revision mismatch: "+i.id)
		}
		if e := ValidateMemory(m); e != nil {
			issues = append(issues, "invalid memory structure: "+i.id)
		}
		liveEvidence := []Evidence{}
		for _, evidence := range m.Evidence {
			expired, expiryErr := sourceRawExpired(ctx, q, evidence.SourceID)
			if expiryErr != nil {
				return nil, expiryErr
			}
			if !expired {
				liveEvidence = append(liveEvidence, evidence)
			}
		}
		m.Evidence = liveEvidence
		if e := checkEvidence(ctx, q, m); e != nil {
			issues = append(issues, "invalid memory evidence: "+i.id)
		}
	}
	rows, err = q.QueryContext(ctx, "SELECT id,workspace_id,content,digest FROM sources WHERE redacted=0")
	if err != nil {
		return nil, err
	}
	type source struct{ id, scope, body, digest string }
	sources := []source{}
	for rows.Next() {
		var s source
		if err = rows.Scan(&s.id, &s.scope, &s.body, &s.digest); err != nil {
			rows.Close()
			return nil, err
		}
		sources = append(sources, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, s := range sources {
		expired, expiryErr := sourceRawExpired(ctx, q, s.id)
		if expiryErr != nil {
			return nil, expiryErr
		}
		if expired {
			continue
		}
		saved, e := ReadSource(ctx, q, s.id, s.scope)
		if e != nil {
			return nil, e
		}
		var b strings.Builder
		valid := Hash([]byte(s.body)) == s.digest
		for _, f := range saved.Fragments {
			b.WriteString(f.Content)
			valid = valid && Hash([]byte(f.Content)) == f.SHA256
		}
		if !valid || b.String() != s.body {
			issues = append(issues, "source snapshot or fragment mismatch: "+s.id)
		}
	}
	return issues, nil
}
