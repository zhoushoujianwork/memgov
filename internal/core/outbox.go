package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// RequestContext is the trusted description of one incoming question. The
// adapter creates it from a verified connection and a platform event, so an
// external Agent can only ever work inside a target, an audience and a route
// version that memgov itself established. A model may not widen it by filling
// in its own sender, workspace or conversation.
type RequestContext struct {
	ID             string `json:"id"`
	ChannelID      string `json:"channel_id"`
	ChannelName    string `json:"channel_name,omitempty"`
	RouteID        string `json:"route_id"`
	RouteVersion   int    `json:"route_version"`
	ConversationID string `json:"conversation_id"`
	AudienceKey    string `json:"audience_key"`
	WorkspaceID    string `json:"workspace_id"`
	PrincipalID    string `json:"principal_id,omitempty"`
	Sender         Sender `json:"sender"`
	TriggerMessage string `json:"trigger_message_id,omitempty"`
	Intent         string `json:"intent"`
	Query          string `json:"query,omitempty"`
	Origin         string `json:"origin"`
	ReplyTransport string `json:"reply_transport,omitempty"`
	ReplyExpiresAt string `json:"reply_credential_expires_at,omitempty"`
	ExpiresAt      string `json:"expires_at"`
	CreatedAt      string `json:"created_at"`
}

// ContextInput is what an adapter supplies. Everything authorizing the request —
// route, audience, workspace, principal — is resolved here rather than accepted.
type ContextInput struct {
	ConversationID string        `json:"conversation_id"`
	Sender         Sender        `json:"sender"`
	TriggerMessage string        `json:"trigger_message_id,omitempty"`
	Intent         string        `json:"intent,omitempty"`
	Query          string        `json:"query,omitempty"`
	Origin         string        `json:"origin,omitempty"`
	ReplyTransport string        `json:"reply_transport,omitempty"`
	ReplyExpiresAt string        `json:"reply_credential_expires_at,omitempty"`
	TTL            time.Duration `json:"-"`
}

var contextIntents = []string{"query", "remember", "correct", "notify"}

const contextTTL = 30 * time.Minute

// OpenContext records one trusted request. The reply target is fixed at this
// moment; a later draft cannot be pointed at a different conversation.
func (tx *Tx) OpenContext(ctx context.Context, channelValue string, in ContextInput) (RequestContext, error) {
	out := RequestContext{}
	c, err := ReadChannel(ctx, tx.Conn, channelValue)
	if err != nil {
		return out, err
	}
	r, err := RouteFor(ctx, tx.Conn, c.ID, in.ConversationID)
	if err != nil {
		return out, err
	}
	if in.Intent == "" {
		in.Intent = "query"
	}
	if !contains(contextIntents, in.Intent) {
		return out, Fail("invalid_input", "unsupported intent %q", in.Intent)
	}
	if in.Origin == "" {
		in.Origin = "stream"
	}
	if !contains([]string{"stream", "history", "import", "local"}, in.Origin) {
		return out, Fail("invalid_input", "unsupported context origin %q", in.Origin)
	}
	if err = in.Sender.validate(); err != nil {
		return out, err
	}
	// A request from an offline import may not claim a trusted online trigger,
	// because nothing verified that the message ever arrived that way.
	if in.Origin == "import" && in.TriggerMessage != "" {
		return out, Fail("denied", "an imported context must not claim a verified trigger message")
	}
	principal, weak, err := tx.PrincipalFor(ctx, c.Tenant, in.Sender)
	if err != nil {
		return out, err
	}
	if weak {
		principal = ""
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = contextTTL
	}
	out = RequestContext{ID: NewID(), ChannelID: c.ID, ChannelName: c.Name, RouteID: r.ID, RouteVersion: r.Version,
		ConversationID: r.ConversationID, AudienceKey: r.AudienceKey, WorkspaceID: r.WorkspaceID, PrincipalID: principal,
		Sender: in.Sender, TriggerMessage: in.TriggerMessage, Intent: in.Intent, Query: in.Query, Origin: in.Origin,
		ReplyTransport: in.ReplyTransport, ReplyExpiresAt: in.ReplyExpiresAt,
		ExpiresAt: time.Now().UTC().Add(ttl).Format(time.RFC3339), CreatedAt: Now()}
	if _, err = tx.Conn.ExecContext(ctx, `INSERT INTO request_contexts(id,channel_id,route_id,route_version,conversation_id,audience_key,
 workspace_id,principal_id,sender_id_type,sender_id_value,trigger_message_id,intent,query,origin,reply_transport,
 reply_credential_expires_at,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		out.ID, out.ChannelID, out.RouteID, out.RouteVersion, out.ConversationID, out.AudienceKey, out.WorkspaceID,
		out.PrincipalID, out.Sender.IDType, out.Sender.IDValue, out.TriggerMessage, out.Intent, out.Query, out.Origin,
		out.ReplyTransport, out.ReplyExpiresAt, out.ExpiresAt, out.CreatedAt); err != nil {
		return out, err
	}
	_, err = tx.Audit(ctx, "context.open", "trusted request context for "+out.ConversationID, objectChange("request_context", out.ID))
	return out, err
}

func ReadContext(ctx context.Context, q Queryer, id string) (RequestContext, error) {
	var out RequestContext
	err := q.QueryRowContext(ctx, `SELECT c.id,c.channel_id,ch.name,c.route_id,c.route_version,c.conversation_id,c.audience_key,
 c.workspace_id,c.principal_id,c.sender_id_type,c.sender_id_value,c.trigger_message_id,c.intent,c.query,c.origin,
 c.reply_transport,c.reply_credential_expires_at,c.expires_at,c.created_at
 FROM request_contexts c JOIN channels ch ON ch.id=c.channel_id WHERE c.id=?`, id).Scan(&out.ID, &out.ChannelID,
		&out.ChannelName, &out.RouteID, &out.RouteVersion, &out.ConversationID, &out.AudienceKey, &out.WorkspaceID,
		&out.PrincipalID, &out.Sender.IDType, &out.Sender.IDValue, &out.TriggerMessage, &out.Intent, &out.Query,
		&out.Origin, &out.ReplyTransport, &out.ReplyExpiresAt, &out.ExpiresAt, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, Fail("not_found", "request context %q not found", id)
	}
	return out, err
}

// audienceForContext rebuilds the audience from the route the context was opened
// against. The route is re-read, so a policy change between opening a context
// and producing a draft is noticed rather than inherited.
func audienceForContext(ctx context.Context, q Queryer, rc RequestContext) (Audience, error) {
	c, err := ReadChannel(ctx, q, rc.ChannelID)
	if err != nil {
		return Audience{}, err
	}
	r, err := RouteFor(ctx, q, rc.ChannelID, rc.ConversationID)
	if err != nil {
		return Audience{}, err
	}
	return Audience{ChannelID: c.ID, ChannelName: c.Name, Kind: c.Kind, Tenant: c.Tenant,
		ConversationID: r.ConversationID, AudienceKey: r.AudienceKey, WorkspaceID: r.WorkspaceID,
		RouteID: r.ID, RouteVersion: r.Version, SendPolicy: r.SendPolicy, Mode: r.Mode}, nil
}

// Draft is one reply awaiting a deliberate decision. It is never a send: it
// exists so a person can read what would go out, to whom, and on what evidence.
type Draft struct {
	ID             string   `json:"id"`
	ChannelID      string   `json:"channel_id"`
	ChannelName    string   `json:"channel_name,omitempty"`
	RouteID        string   `json:"route_id"`
	RouteVersion   int      `json:"route_version"`
	ContextID      string   `json:"request_context_id,omitempty"`
	ConversationID string   `json:"conversation_id"`
	AudienceKey    string   `json:"audience_key"`
	SenderIdentity string   `json:"sender_identity"`
	Transport      string   `json:"transport,omitempty"`
	Content        string   `json:"content"`
	Format         string   `json:"format"`
	Citations      []string `json:"citations"`
	InputDigest    string   `json:"input_digest"`
	DisplayDigest  string   `json:"display_digest"`
	SendPolicy     string   `json:"send_policy"`
	State          string   `json:"state"`
	Reason         string   `json:"reason,omitempty"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

// DraftInput is what a model may supply: the text and the memory versions it
// used. Target, identity and permission come from the trusted context.
type DraftInput struct {
	ContextID string   `json:"request_context_id"`
	Content   string   `json:"content"`
	Format    string   `json:"format,omitempty"`
	Citations []string `json:"citations"`
}

const maxDraftChars = 4000

// Draft records one reply as a draft. Every cited memory is re-checked against
// the live audience here, so a draft can never carry material the conversation
// is not allowed to see, whatever the model produced.
func (tx *Tx) Draft(ctx context.Context, in DraftInput) (any, error) {
	rc, err := ReadContext(ctx, tx.Conn, in.ContextID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Content) == "" {
		return nil, Fail("invalid_input", "a draft needs content; an empty reply is not a result")
	}
	if n := len([]rune(in.Content)); n > maxDraftChars {
		return nil, Fail("invalid_input", "draft content is %d characters, above the %d limit", n, maxDraftChars)
	}
	if in.Format == "" {
		in.Format = "text"
	}
	if !contains([]string{"text", "markdown"}, in.Format) {
		return nil, Fail("invalid_input", "unsupported draft format %q", in.Format)
	}
	if rc.ExpiresAt < Now() {
		return nil, Fail("denied", "request context %s expired at %s; open a new one rather than replying late", rc.ID, rc.ExpiresAt)
	}
	a, err := audienceForContext(ctx, tx.Conn, rc)
	if err != nil {
		return nil, err
	}
	if a.RouteVersion != rc.RouteVersion {
		return nil, Fail("conflict", "route changed from version %d to %d while the reply was produced; re-check the audience", rc.RouteVersion, a.RouteVersion)
	}
	// Legacy database memory citations are archived and cannot authorize new sends.
	if len(in.Citations) != 0 {
		return nil, Fail("denied", "database memory citations have been removed; prepare a new workspace-based reply")
	}
	seq := map[string]int{}
	c, err := ReadChannel(ctx, tx.Conn, rc.ChannelID)
	if err != nil {
		return nil, err
	}
	out := Draft{ID: NewID(), ChannelID: c.ID, ChannelName: c.Name, RouteID: a.RouteID, RouteVersion: a.RouteVersion,
		ContextID: rc.ID, ConversationID: rc.ConversationID, AudienceKey: a.AudienceKey,
		SenderIdentity: c.AuthNamespace, Transport: rc.ReplyTransport, Content: in.Content, Format: in.Format,
		Citations: in.Citations, SendPolicy: a.SendPolicy, State: "draft", CreatedAt: Now(), UpdatedAt: Now()}
	if out.Citations == nil {
		out.Citations = []string{}
	}
	out.InputDigest = Digest(map[string]any{"context": rc.ID, "content": in.Content, "format": in.Format, "citations": out.Citations})
	out.DisplayDigest = displayDigest(out, seq)
	_, err = tx.Conn.ExecContext(ctx, `INSERT INTO outbox(id,channel_id,route_id,route_version,request_context_id,conversation_id,
 audience_key,sender_identity,transport,content,format,citations,evidence_seq,input_digest,display_digest,send_policy,
 state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		out.ID, out.ChannelID, out.RouteID, out.RouteVersion, out.ContextID, out.ConversationID, out.AudienceKey,
		out.SenderIdentity, out.Transport, out.Content, out.Format, JSON(out.Citations), JSON(seq), out.InputDigest,
		out.DisplayDigest, out.SendPolicy, out.State, out.CreatedAt, out.UpdatedAt)
	if err != nil {
		// One reply task and one input produce one draft; a retry finds the
		// existing draft instead of queueing a second copy of the same reply.
		if strings.Contains(err.Error(), "UNIQUE constraint failed: outbox.route_id") {
			existing, readErr := draftByDigest(ctx, tx.Conn, out.RouteID, out.InputDigest)
			if readErr != nil {
				return nil, readErr
			}
			return map[string]any{"draft": existing, "duplicate": true, "sent": false,
				"note": "an identical draft already exists for this route and input"}, nil
		}
		return nil, err
	}
	if _, err = tx.Audit(ctx, "outbox.draft", "reply draft for "+out.ConversationID, objectChange("outbox", out.ID)); err != nil {
		return nil, err
	}
	return map[string]any{"draft": out, "duplicate": false, "sent": false,
		"note": "a draft is stored only; nothing was sent and no approval was recorded"}, nil
}

// displayDigest binds the content, the target, the sending identity, the cited
// versions and the policy version together. It exists so the text that was read
// cannot be swapped for another before a dispatch; it is not an approval record.
func displayDigest(d Draft, seq map[string]int) string {
	return Digest(map[string]any{"content": d.Content, "format": d.Format, "conversation": d.ConversationID,
		"audience": d.AudienceKey, "sender_identity": d.SenderIdentity, "citations": seq,
		"route_version": d.RouteVersion, "send_policy": d.SendPolicy})
}

const draftColumns = `o.id,o.channel_id,ch.name,o.route_id,o.route_version,o.request_context_id,o.conversation_id,
 o.audience_key,o.sender_identity,o.transport,o.content,o.format,o.citations,o.input_digest,o.display_digest,
 o.send_policy,o.state,o.reason,o.created_at,o.updated_at`

func scanDraft(row interface{ Scan(...any) error }) (Draft, map[string]int, error) {
	var d Draft
	var citations string
	err := row.Scan(&d.ID, &d.ChannelID, &d.ChannelName, &d.RouteID, &d.RouteVersion, &d.ContextID, &d.ConversationID,
		&d.AudienceKey, &d.SenderIdentity, &d.Transport, &d.Content, &d.Format, &citations, &d.InputDigest,
		&d.DisplayDigest, &d.SendPolicy, &d.State, &d.Reason, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return d, nil, err
	}
	d.Citations = []string{}
	if err = json.Unmarshal([]byte(citations), &d.Citations); err != nil {
		return d, nil, err
	}
	return d, map[string]int{}, nil
}

func ReadDraft(ctx context.Context, q Queryer, id string) (Draft, error) {
	d, _, err := scanDraft(q.QueryRowContext(ctx, "SELECT "+draftColumns+" FROM outbox o JOIN channels ch ON ch.id=o.channel_id WHERE o.id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, Fail("not_found", "draft %q not found", id)
	}
	if err == nil {
		err = redactReadDraft(ctx, q, &d)
	}
	return d, err
}

func draftByDigest(ctx context.Context, q Queryer, routeID, digest string) (Draft, error) {
	d, _, err := scanDraft(q.QueryRowContext(ctx, "SELECT "+draftColumns+" FROM outbox o JOIN channels ch ON ch.id=o.channel_id WHERE o.route_id=? AND o.input_digest=?", routeID, digest))
	if errors.Is(err, sql.ErrNoRows) {
		return d, Fail("not_found", "no draft for that input digest")
	}
	if err == nil {
		err = redactReadDraft(ctx, q, &d)
	}
	return d, err
}

func redactReadDraft(ctx context.Context, q Queryer, d *Draft) error {
	var taskID string
	if err := q.QueryRowContext(ctx, "SELECT job_id FROM outbox WHERE id=?", d.ID).Scan(&taskID); err != nil {
		return err
	}
	if taskID == "" {
		return nil
	}
	t := RuntimeTask{ID: taskID, ResultSummary: d.Content}
	if err := redactReadRuntimeTask(ctx, q, &t); err != nil {
		return err
	}
	d.Content = t.ResultSummary
	return nil
}

func DraftList(ctx context.Context, q Queryer, channelValue, state string, limit int) (any, error) {
	c, err := ReadChannel(ctx, q, channelValue)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	query := "SELECT " + draftColumns + " FROM outbox o JOIN channels ch ON ch.id=o.channel_id WHERE o.channel_id=?"
	args := []any{c.ID}
	if state != "" {
		query += " AND o.state=?"
		args = append(args, state)
	}
	query += " ORDER BY o.created_at DESC,o.id LIMIT ?"
	rows, err := q.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	drafts := []Draft{}
	for rows.Next() {
		d, _, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, d)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range drafts {
		if err = redactReadDraft(ctx, q, &drafts[i]); err != nil {
			return nil, err
		}
	}
	counts, err := stateCounts(ctx, q, c.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"channel": c.Name, "drafts": drafts, "states": counts,
		"note": "listing drafts never sends anything"}, nil
}

func stateCounts(ctx context.Context, q Queryer, channelID string) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, "SELECT state,COUNT(*) FROM outbox WHERE channel_id=? GROUP BY state", channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err = rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}
