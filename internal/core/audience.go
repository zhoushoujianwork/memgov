package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Audience decides what may leave this machine for one specific conversation.
// It is an intersection, so every condition must hold: the channel tenant, the
// route binding, the memory state and validity window, the evidence still being
// available, and an explicit local publication for that audience. A missing
// condition is a refusal, never a default allow.
type Audience struct {
	ChannelID      string `json:"channel_id"`
	ChannelName    string `json:"channel_name"`
	Kind           string `json:"kind"`
	Tenant         string `json:"tenant"`
	ConversationID string `json:"conversation_id"`
	AudienceKey    string `json:"audience_key"`
	WorkspaceID    string `json:"workspace_id"`
	RouteID        string `json:"route_id"`
	RouteVersion   int    `json:"route_version"`
	MemoryPolicy   string `json:"memory_policy"`
	SendPolicy     string `json:"send_policy"`
	Mode           string `json:"mode"`
}

func AudienceFor(ctx context.Context, q Queryer, channelValue, conversationID string) (Audience, error) {
	c, err := ReadChannel(ctx, q, channelValue)
	if err != nil {
		return Audience{}, err
	}
	r, err := RouteFor(ctx, q, c.ID, conversationID)
	if err != nil {
		return Audience{}, err
	}
	return Audience{ChannelID: c.ID, ChannelName: c.Name, Kind: c.Kind, Tenant: c.Tenant,
		ConversationID: r.ConversationID, AudienceKey: r.AudienceKey, WorkspaceID: r.WorkspaceID,
		RouteID: r.ID, RouteVersion: r.Version, MemoryPolicy: r.MemoryPolicy, SendPolicy: r.SendPolicy, Mode: r.Mode}, nil
}

// memoryFacts reads the authoritative row rather than the embedded document, so
// a disclosure decision cannot be made against a stale copy of the version.
func memoryFacts(ctx context.Context, q Queryer, memoryID string) (string, int, string, error) {
	var workspace, status string
	var version int
	err := q.QueryRowContext(ctx, "SELECT workspace_id,version,status FROM memories WHERE id=?", memoryID).Scan(&workspace, &version, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, "", Fail("not_found", "memory %q not found", memoryID)
	}
	return workspace, version, status, err
}

// Disclosure is the per-memory verdict. Reasons are recorded so a refusal can be
// explained without exposing the memory itself.
type Disclosure struct {
	MemoryID  string   `json:"memory_id"`
	Version   int      `json:"version"`
	Allowed   bool     `json:"allowed"`
	Reasons   []string `json:"reasons"`
	Evidence  []string `json:"evidence_sources,omitempty"`
	Published bool     `json:"published"`
}

// CheckDisclosure evaluates one memory against one audience. It is the single
// place that answers "may this leave the machine for that conversation".
func CheckDisclosure(ctx context.Context, q Queryer, a Audience, memoryID string) (Disclosure, error) {
	d := Disclosure{MemoryID: memoryID, Reasons: []string{}}
	m, err := ReadMemory(ctx, q, memoryID, a.WorkspaceID, 0)
	if err != nil {
		return d, err
	}
	// A memory belonging to another workspace is out of this route's scope even
	// if the caller can read it locally. The table row is authoritative.
	workspace, version, status, err := memoryFacts(ctx, q, memoryID)
	if err != nil {
		return d, err
	}
	d.Version = version
	if workspace != a.WorkspaceID && workspace != "global" {
		d.Reasons = append(d.Reasons, "memory belongs to workspace "+workspace+" which is not routed to this conversation")
	}
	if status != "active" {
		d.Reasons = append(d.Reasons, "memory status is "+status)
	}
	now := time.Now()
	if m.ValidFrom != "" {
		if t, err := time.Parse(time.RFC3339, m.ValidFrom); err == nil && now.Before(t) {
			d.Reasons = append(d.Reasons, "memory is not valid yet")
		}
	}
	if m.ValidUntil != "" {
		if t, err := time.Parse(time.RFC3339, m.ValidUntil); err == nil && !now.Before(t) {
			d.Reasons = append(d.Reasons, "memory validity window has ended")
		}
	}
	// Evidence must still be available. A recalled source withdraws the
	// conclusion that rests on it instead of the conclusion outliving it.
	for _, e := range m.Evidence {
		if e.SourceID == "" {
			continue
		}
		d.Evidence = append(d.Evidence, e.SourceID)
		var state, reason string
		err := q.QueryRowContext(ctx, "SELECT state,reason FROM source_availability WHERE source_id=?", e.SourceID).Scan(&state, &reason)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return d, err
		}
		if state != "available" && state != "expired" {
			d.Reasons = append(d.Reasons, "evidence source "+e.SourceID+" is "+state+": "+reason)
		}
	}
	// Cross-tenant and private material require an explicit publication for this
	// exact audience and memory version. Sharing is an act, not an inference.
	groupPolicy, err := groupMemoryPolicyForAudience(ctx, q, a)
	if err != nil {
		return d, err
	}
	for _, category := range groupPolicy.Excluded {
		if m.Category == category {
			d.Reasons = append(d.Reasons, "memory category is excluded from group queries")
		}
	}
	sharedGlobal := groupPolicy.Shared
	var published int
	err = q.QueryRowContext(ctx, "SELECT version FROM memory_publications WHERE memory_id=? AND audience_key=?", memoryID, a.AudienceKey).Scan(&published)
	switch {
	case errors.Is(err, sql.ErrNoRows) && sharedGlobal && workspace == "global":
		d.Published = true
	case errors.Is(err, sql.ErrNoRows):
		d.Reasons = append(d.Reasons, "memory is local_private for audience "+a.AudienceKey+
			"; publish it explicitly before disclosure. a publication for another conversation does not carry over")
	case err != nil:
		return d, err
	case published != version && !(sharedGlobal && workspace == "global"):
		d.Reasons = append(d.Reasons, fmt.Sprintf("publication covers version %d but current version is %d; re-publish after the change", published, version))
	default:
		d.Published = true
	}
	d.Allowed = len(d.Reasons) == 0
	if d.Allowed {
		d.Reasons = append(d.Reasons, "shared with this audience under the current publication or workspace policy with available evidence")
	}
	return d, nil
}

// Publish records an explicit local decision to disclose one memory version to
// one audience. Nothing is sent here; this only makes disclosure permissible.
func (tx *Tx) Publish(ctx context.Context, channelValue, conversationID, memoryID, reason string) (any, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, Fail("invalid_input", "publication reason is required")
	}
	a, err := AudienceFor(ctx, tx.Conn, channelValue, conversationID)
	if err != nil {
		return nil, err
	}
	if _, err = ReadMemory(ctx, tx.Conn, memoryID, a.WorkspaceID, 0); err != nil {
		return nil, err
	}
	workspace, version, status, err := memoryFacts(ctx, tx.Conn, memoryID)
	if err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, Fail("invalid_input", "only an active memory can be published; status is %s", status)
	}
	if workspace != a.WorkspaceID && workspace != "global" {
		return nil, Fail("denied", "memory workspace %s is not routed to this conversation", workspace)
	}
	op, err := tx.Audit(ctx, "memory.publish", reason+" (audience "+a.AudienceKey+")", []Change{{ObjectType: "memory", ObjectID: memoryID, MemoryID: memoryID, After: version}})
	if err != nil {
		return nil, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO memory_publications(memory_id,audience_key,version,reason,operation_id,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(memory_id,audience_key) DO UPDATE SET version=excluded.version,reason=excluded.reason,operation_id=excluded.operation_id,created_at=excluded.created_at",
		memoryID, a.AudienceKey, version, reason, op.ID, Now()); err != nil {
		return nil, err
	}
	return map[string]any{"memory_id": memoryID, "version": version, "audience_key": a.AudienceKey,
		"published": true, "sent": false, "note": "publication permits disclosure; it does not send anything"}, nil
}

// Unpublish withdraws a disclosure permission. Existing drafts built on it are
// marked stale so a withdrawn permission cannot be spent by an old draft.
func (tx *Tx) Unpublish(ctx context.Context, channelValue, conversationID, memoryID, reason string) (any, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, Fail("invalid_input", "withdrawal reason is required")
	}
	a, err := AudienceFor(ctx, tx.Conn, channelValue, conversationID)
	if err != nil {
		return nil, err
	}
	res, err := tx.Conn.ExecContext(ctx, "DELETE FROM memory_publications WHERE memory_id=? AND audience_key=?", memoryID, a.AudienceKey)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, Fail("not_found", "memory %q is not published to audience %s", memoryID, a.AudienceKey)
	}
	stale, err := tx.Conn.ExecContext(ctx, "UPDATE outbox SET state='stale',reason=?,updated_at=? WHERE route_id=? AND state IN ('draft','ready') AND citations LIKE ?",
		"disclosure withdrawn for memory "+memoryID, Now(), a.RouteID, "%"+memoryID+"%")
	if err != nil {
		return nil, err
	}
	affected, _ := stale.RowsAffected()
	if _, err = tx.Audit(ctx, "memory.unpublish", reason+" (audience "+a.AudienceKey+")", []Change{{ObjectType: "memory", ObjectID: memoryID, MemoryID: memoryID}}); err != nil {
		return nil, err
	}
	return map[string]any{"memory_id": memoryID, "audience_key": a.AudienceKey, "published": false, "stale_drafts": affected}, nil
}

// AudienceRecall answers a remote question using only what that audience may
// see. The audience filter runs before the model sees anything, so private
// material never enters a prompt built for a group conversation.
func AudienceRecall(ctx context.Context, q Queryer, a Audience, text string, budget int, explain bool) (any, error) {
	if db, ok := q.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		return AudienceRecall(ctx, tx, a, text, budget, explain)
	}
	if budget <= 0 {
		return nil, Fail("invalid_input", "budget-chars must be positive")
	}
	hits, err := Search(ctx, q, text, SearchOptions{Scope: a.WorkspaceID, Kind: "memory", Status: "active", ValidAt: Now(), Limit: 1000})
	if err != nil {
		return nil, err
	}
	allowed := []RecallItem{}
	refused := []Disclosure{}
	var context strings.Builder
	used := 0
	refusedCount := 0
	for _, hit := range hits {
		if len(allowed) >= 10 {
			break
		}
		d, err := CheckDisclosure(ctx, q, a, hit.ID)
		if err != nil {
			return nil, err
		}
		if !d.Allowed {
			refusedCount++
			if explain {
				refused = append(refused, d)
			}
			continue
		}
		m, err := ReadAudienceMemory(ctx, q, a, hit.ID)
		if err != nil {
			return nil, err
		}
		item := RecallItem{ID: m.ID, Version: m.Version, Title: m.Title, Summary: m.Summary, Evidence: []Evidence{}}
		block := fmt.Sprintf("## %s\n%s\n引用: memory:%s@%d\n\n", item.Title, item.Summary, item.ID, item.Version)
		if used+len([]rune(block)) > budget {
			continue
		}
		allowed = append(allowed, item)
		context.WriteString(block)
		used += len([]rune(block))
	}
	return map[string]any{"audience_key": a.AudienceKey, "workspace_id": a.WorkspaceID, "conversation_id": a.ConversationID,
		"items": allowed, "context": context.String(), "used_chars": used, "budget_chars": budget,
		"refused": refused, "refused_count": refusedCount,
		"note": "only memories visible under the current publication or workspace sharing policy are included; empty results do not mean the local database is empty"}, nil
}

// AudienceSources limits message evidence to one conversation and drops
// withdrawn material, so a personal thread never reaches a group prompt.
func AudienceSources(ctx context.Context, q Queryer, a Audience, limit int) (any, error) {
	if limit < 1 || limit > 500 {
		return nil, Fail("invalid_input", "limit must be 1..500")
	}
	rows, err := q.QueryContext(ctx, `SELECT o.source_id,o.message_id,o.revision,coalesce(av.state,'available')
		FROM source_origins o JOIN messages m ON m.id=o.message_id
		LEFT JOIN source_availability av ON av.source_id=o.source_id
		WHERE o.channel_id=? AND m.conversation_id=? AND m.availability='available' AND `+retainedSourcePredicate("o.source_id")+`
		ORDER BY m.sent_at DESC,o.revision DESC LIMIT ?`, a.ChannelID, a.ConversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	withdrawn := 0
	for rows.Next() {
		var sourceID, messageID, state string
		var revision int
		if err = rows.Scan(&sourceID, &messageID, &revision, &state); err != nil {
			return nil, err
		}
		if state != "available" {
			withdrawn++
			continue
		}
		items = append(items, map[string]any{"source_id": sourceID, "message_id": messageID, "revision": revision})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"audience_key": a.AudienceKey, "conversation_id": a.ConversationID,
		"sources": items, "withdrawn": withdrawn}, nil
}
