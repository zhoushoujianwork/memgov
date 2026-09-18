package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ExchangePackage includes all authoritative tables. FTS and request-result
// caches are derived/private operational state and deliberately not exchanged.
type ExchangePackage struct {
	Format        string                      `json:"format"`
	SchemaVersion int                         `json:"schema_version"`
	CreatedAt     string                      `json:"created_at"`
	Tables        map[string][]map[string]any `json:"tables"`
}

var exchangeTables = []string{"schema_migrations", "settings", "workspaces", "sources", "source_locations", "fragments", "migration_runs", "migration_sources", "candidates", "reviews", "memories", "revisions", "memory_evidence", "relations", "operations", "requests", "jobs", "plans", "tombstones", "purge_runs",
	// Channel configuration and message business tables. Credentials stay outside
	// the database as references, so no secret leaves through an exchange package.
	"channels", "channel_routes", "principals", "identity_aliases", "conversations", "inbox_events",
	"messages", "message_revisions", "message_observations", "message_relations", "message_attachments",
	"message_recalls", "source_origins", "source_availability", "memory_publications",
	"request_contexts", "outbox", "delivery_attempts", "coverage_windows", "channel_watermarks",
	"runtime_configs", "runtime_message_states", "runtime_batches", "runtime_batch_messages", "runtime_tasks",
	"runtime_task_messages", "runtime_attempts", "runtime_pending_actions", "runtime_action_attempts",
	"runtime_direct_sessions", "runtime_direct_turns"}

func Export(ctx context.Context, s *Store) (ExchangePackage, error) {
	out := ExchangePackage{Format: "memgov.exchange", SchemaVersion: SchemaVersion, CreatedAt: Now(), Tables: map[string][]map[string]any{}}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	for _, table := range exchangeTables {
		rows, err := tx.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			return out, err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return out, err
		}
		items := []map[string]any{}
		for rows.Next() {
			values := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if err = rows.Scan(dest...); err != nil {
				rows.Close()
				return out, err
			}
			row := map[string]any{}
			for i, k := range cols {
				if b, ok := values[i].([]byte); ok {
					row[k] = string(b)
				} else {
					row[k] = values[i]
				}
			}
			items = append(items, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		out.Tables[table] = items
	}
	return out, tx.Commit()
}
func (p ExchangePackage) Markdown() (string, error) {
	var b strings.Builder
	b.WriteString("# memgov 记忆导出\n\n")
	for _, row := range p.Tables["memories"] {
		var m Memory
		raw, ok := row["document"].(string)
		if !ok {
			return "", Fail("internal", "invalid memory document")
		}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "## %s\n\n- ID：%s\n- 版本：%d\n- 状态：%s\n- 工作区：%s\n- 分类：%s\n\n%s\n\n%s\n\n", m.Title, m.ID, m.Version, m.Status, scopeID(m.WorkspaceID), m.Category, m.Summary, m.Content)
		for _, a := range m.Applicability {
			fmt.Fprintf(&b, "适用条件：%s\n\n", a)
		}
		for _, e := range m.Evidence {
			fmt.Fprintf(&b, "证据：source=%s fragment=%s sha256=%s\n\n", e.SourceID, e.FragmentID, e.SHA256)
		}
	}
	return b.String(), nil
}
