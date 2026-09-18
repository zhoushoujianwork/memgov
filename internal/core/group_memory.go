package core

import (
	"context"
	"database/sql"
	"encoding/json"
)

// GroupGlobalMemoryShared resolves the currently applied owner decision. The
// global workspace has no implicit disclosure rights; sharing must be explicit
// for the admitted group application's channel.
func GroupGlobalMemoryShared(ctx context.Context, q Queryer, a Audience) (bool, error) {
	p, err := groupMemoryPolicyForAudience(ctx, q, a)
	return p.Shared, err
}

type groupMemoryPolicy struct {
	Shared   bool
	Excluded []string
}

func groupMemoryPolicyForAudience(ctx context.Context, q Queryer, a Audience) (groupMemoryPolicy, error) {
	out := groupMemoryPolicy{}
	route, err := ReadRoute(ctx, q, a.RouteID)
	if err != nil {
		return out, err
	}
	if route.ChannelID != a.ChannelID || route.ConversationID != a.ConversationID || route.ConversationType != "group" || route.Status != "active" || route.Mode != "assistant" {
		return out, nil
	}
	applied, err := ReadAppliedConfig(ctx, q, 0)
	if err != nil {
		return out, err
	}
	if applied.Version == 0 {
		return out, nil
	}
	var declaration struct {
		Applications struct {
			Group *struct {
				Enabled  bool     `json:"enabled"`
				Channel  string   `json:"channel"`
				Shared   []string `json:"shared_memory_workspaces"`
				Excluded []string `json:"excluded_memory_categories"`
			} `json:"group_mention"`
		} `json:"applications"`
	}
	if err := json.Unmarshal(applied.Declaration, &declaration); err != nil {
		return out, err
	}
	g := declaration.Applications.Group
	if g == nil || !g.Enabled {
		return out, nil
	}
	configured, err := ReadChannel(ctx, q, g.Channel)
	if err != nil {
		return out, err
	}
	if configured.ID != a.ChannelID || configured.Kind != ChannelDingTalkApp || configured.Tenant != a.Tenant {
		return out, nil
	}
	out.Excluded = g.Excluded
	for _, workspace := range g.Shared {
		if workspace == "global" {
			out.Shared = true
		}
	}
	return out, nil
}

type AudienceMemory struct {
	ID          string `json:"id"`
	Version     int    `json:"version"`
	WorkspaceID string `json:"workspace_id"`
	Category    string `json:"category"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	Content     string `json:"content,omitempty"`
	ObservedAt  string `json:"observed_at,omitempty"`
	UpdatedAt   string `json:"updated_at"`
}

func ReadAudienceMemory(ctx context.Context, q Queryer, a Audience, id string) (AudienceMemory, error) {
	if db, ok := q.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return AudienceMemory{}, err
		}
		defer tx.Rollback()
		return ReadAudienceMemory(ctx, tx, a, id)
	}
	var out AudienceMemory
	verdict, err := CheckDisclosure(ctx, q, a, id)
	if err != nil {
		return out, err
	}
	if !verdict.Allowed {
		return out, Fail("denied", "memory is not visible to this group")
	}
	memory, err := ReadMemory(ctx, q, id, a.WorkspaceID, 0)
	if err != nil {
		return out, err
	}
	out = AudienceMemory{ID: memory.ID, Version: verdict.Version, WorkspaceID: scopeID(memory.WorkspaceID), Category: memory.Category, Title: memory.Title, Summary: memory.Summary, Content: memory.Content, ObservedAt: memory.ObservedAt}
	if err := q.QueryRowContext(ctx, "SELECT updated_at FROM memories WHERE id=?", id).Scan(&out.UpdatedAt); err != nil {
		return out, err
	}
	// Sharing approved memory text never grants access to the raw evidence source.
	// Large records remain bounded so a listing cannot exhaust a group context.
	out.Content = boundedMemoryText(out.Content, 20000)
	out.Summary = boundedMemoryText(out.Summary, 4000)
	return out, nil
}
func boundedMemoryText(value string, limit int) string {
	r := []rune(value)
	if len(r) > limit {
		return string(r[:limit]) + "\n[内容超过查询长度上限，已截断]"
	}
	return value
}

// ListAudienceMemories filters disclosure before applying the result limit;
// newer private or invalid records cannot conceal the latest visible memory.
func ListAudienceMemories(ctx context.Context, q Queryer, a Audience, limit int) (any, error) {
	if db, ok := q.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		return ListAudienceMemories(ctx, tx, a, limit)
	}
	if limit < 1 || limit > 20 {
		return nil, Fail("invalid_input", "limit must be 1..20")
	}
	policy, err := groupMemoryPolicyForAudience(ctx, q, a)
	if err != nil {
		return nil, err
	}
	excludePreferences := false
	for _, category := range policy.Excluded {
		if category == "preference" {
			excludePreferences = true
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT m.id FROM memories m WHERE m.workspace_id IN (?,'global') AND m.status='active'
	AND (?=0 OR m.category<>'preference')
 AND ((m.workspace_id='global' AND ?=1) OR EXISTS(SELECT 1 FROM memory_publications p WHERE p.memory_id=m.id AND p.audience_key=? AND p.version=m.version))
 ORDER BY m.updated_at DESC,m.id DESC`, a.WorkspaceID, boolInt(excludePreferences), boolInt(policy.Shared), a.AudienceKey)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	items := []AudienceMemory{}
	for _, id := range ids {
		item, err := ReadAudienceMemory(ctx, q, a, id)
		if ErrorCode(err) == "denied" {
			continue
		}
		if err != nil {
			return nil, err
		}
		item.Content = ""
		items = append(items, item)
		if len(items) == limit {
			break
		}
	}
	note := "only memories visible to this conversation are listed; empty results do not mean the local memory database is empty"
	return map[string]any{"items": items, "order": "updated_at_desc", "global_shared": policy.Shared, "excluded_memory_categories": policy.Excluded, "note": note}, nil
}
