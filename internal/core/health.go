package core

import (
	"context"
	"strings"
)

func checkDerivedAndEvidence(ctx context.Context, q Queryer) ([]string, error) {
	issues := []string{}
	const expected = `WITH expected AS (SELECT 'source' AS kind,id AS ref_id,kind AS title,'' AS summary,content,workspace_id FROM sources WHERE redacted=0) `
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
	rows, err := q.QueryContext(ctx, "SELECT id,workspace_id,content,digest FROM sources WHERE redacted=0")
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
