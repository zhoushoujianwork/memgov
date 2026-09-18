package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// InboxEvent kinds the adapters may hand over. Anything else is refused so an
// unknown platform event is never silently normalized into a message.
const (
	EventMessage = "message"
	EventEdit    = "edit"
	EventRecall  = "recall"
)

// Sender identity is always a typed triple. A display name is never an identity,
// because two people may share one and a name is trivially spoofable.
type Sender struct {
	IDType      string `json:"id_type"`
	IDValue     string `json:"id_value"`
	DisplayName string `json:"display_name,omitempty"`
	SelfAuthor  bool   `json:"self_authored,omitempty"`
}

// validate is shared by message intake and request contexts, so an identity is
// judged by the same rule wherever it enters the system.
func (s Sender) validate() error {
	if s.IDType == "" || s.IDValue == "" {
		return Fail("invalid_input", "sender id_type and id_value are required; a display name is not an identity")
	}
	if !contains([]string{"union_id", "user_id", "staff_id", "open_id", "robot_code", "unknown"}, s.IDType) {
		return Fail("invalid_input", "unsupported sender id_type %q", s.IDType)
	}
	return nil
}

type Attachment struct {
	Name       string `json:"name,omitempty"`
	MediaType  string `json:"media_type,omitempty"`
	ResourceID string `json:"resource_id,omitempty"`
}
type Relation struct {
	Kind              string `json:"kind"`
	ProviderMessageID string `json:"provider_message_id"`
	OriginSender      string `json:"origin_sender,omitempty"`
	Confidence        string `json:"confidence,omitempty"`
}

// NormalizedEvent is the only shape an adapter may submit. It carries platform
// identifiers and times separately from local receipt time so a replayed or
// out-of-order event keeps its real ordering.
type NormalizedEvent struct {
	Kind              string        `json:"kind"`
	Adapter           string        `json:"adapter"`
	ParseVersion      string        `json:"parse_version"`
	Origin            string        `json:"origin"`
	ProviderEventID   string        `json:"provider_event_id,omitempty"`
	ProviderMessageID string        `json:"provider_message_id"`
	ConversationID    string        `json:"conversation_id"`
	ConversationType  string        `json:"conversation_type,omitempty"`
	Tenant            string        `json:"tenant,omitempty"`
	Sender            Sender        `json:"sender"`
	Body              string        `json:"body,omitempty"`
	Quote             *MessageQuote `json:"quote,omitempty"`
	Format            string        `json:"format,omitempty"`
	SentAt            string        `json:"sent_at,omitempty"`
	SourceTimeRaw     string        `json:"source_time_raw,omitempty"`
	EditedAt          string        `json:"edited_at,omitempty"`
	EventAt           string        `json:"event_at,omitempty"`
	RecalledAt        string        `json:"recalled_at,omitempty"`
	Ordering          string        `json:"ordering,omitempty"`
	Mentioned         bool          `json:"mentioned,omitempty"`
	Attachments       []Attachment  `json:"attachments,omitempty"`
	Relations         []Relation    `json:"relations,omitempty"`
	Payload           string        `json:"payload,omitempty"`

	// Set only by the trusted DWS history adapter after parsing its CST+8 display time.
	HistoryPreviousSentAt string `json:"-"`
}

// IntakeResult reports exactly what the write did, so a caller can distinguish a
// genuine new message from a duplicate delivery without guessing.
type IntakeResult struct {
	InboxEventID string `json:"inbox_event_id"`
	ChannelID    string `json:"channel_id,omitempty"`
	Duplicate    bool   `json:"duplicate"`
	MessageID    string `json:"message_id,omitempty"`
	MessageKey   string `json:"message_key,omitempty"`
	Revision     int    `json:"revision,omitempty"`
	NewRevision  bool   `json:"new_revision"`
	SourceID     string `json:"source_id,omitempty"`
	Availability string `json:"availability,omitempty"`
	Status       string `json:"status"`
	Note         string `json:"note,omitempty"`
}

func validTime(label, value string) error {
	if value == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return Fail("invalid_input", "%s must use RFC3339", label)
	}
	return nil
}

func (e NormalizedEvent) validate() error {
	if !contains([]string{EventMessage, EventEdit, EventRecall}, e.Kind) {
		return Fail("invalid_input", "unsupported event kind %q", e.Kind)
	}
	if e.Adapter == "" || e.ParseVersion == "" {
		return Fail("invalid_input", "adapter and parse_version are required for provenance")
	}
	if !contains([]string{"", "stream", "history", "import"}, e.Origin) {
		return Fail("invalid_input", "origin must be stream, history or import")
	}
	if !identifier.MatchString(e.ProviderMessageID) {
		return Fail("invalid_input", "provider_message_id is required")
	}
	if !identifier.MatchString(e.ConversationID) {
		return Fail("invalid_input", "conversation_id is required")
	}
	if e.Kind != EventRecall {
		if err := e.Sender.validate(); err != nil {
			return err
		}
		if !utf8.ValidString(e.Body) {
			return Fail("invalid_input", "message body must be valid UTF-8")
		}
	}
	for _, r := range e.Relations {
		if !contains([]string{"reply", "quote", "forward"}, r.Kind) {
			return Fail("invalid_input", "unsupported relation kind %q", r.Kind)
		}
		if !identifier.MatchString(r.ProviderMessageID) {
			return Fail("invalid_input", "relation provider_message_id is invalid")
		}
	}
	if err := e.Quote.Validate(); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"sent_at", e.SentAt}, {"edited_at", e.EditedAt}, {"event_at", e.EventAt}, {"recalled_at", e.RecalledAt}} {
		if err := validTime(pair[0], pair[1]); err != nil {
			return err
		}
	}
	if e.Payload != "" && !json.Valid([]byte(e.Payload)) {
		return Fail("invalid_input", "payload must be JSON")
	}
	return nil
}

// messageKey identifies one platform message inside one ID namespace. Two
// channels that see the same conversation converge on the same key, while two
// tenants never collide.
func messageKey(idNamespace, conversationID, providerMessageID string) string {
	return idNamespace + "|" + conversationID + "|" + providerMessageID
}

// dedupeKey makes a redelivered platform event idempotent. Stream events carry
// their own event ID; a history page has none, so the message revision digest
// stands in and a re-read of the same page cannot double-write.
func dedupeKey(c Channel, e NormalizedEvent) string {
	base := c.Provider + "|" + c.IDNamespace + "|" + c.AuthNamespace + "|" + e.Kind + "|"
	if e.ProviderEventID != "" {
		return base + "event:" + e.ProviderEventID
	}
	return base + "content:" + Hash([]byte(messageKey(c.IDNamespace, e.ConversationID, e.ProviderMessageID)+"\x00"+
		e.Sender.IDType+":"+e.Sender.IDValue+"\x00"+e.SentAt+"\x00"+e.EditedAt+"\x00"+messageContentDigest(e)))
}

// PrincipalFor resolves a platform identifier to a stable local principal. An
// unverified alias is recorded but never used to authorize anything.
func (tx *Tx) PrincipalFor(ctx context.Context, tenant string, s Sender) (string, bool, error) {
	if tenant == "" {
		return "", false, Fail("invalid_input", "principal tenant is required")
	}
	weak := s.IDType == "unknown"
	var id string
	err := tx.Conn.QueryRowContext(ctx, "SELECT principal_id FROM identity_aliases WHERE tenant=? AND id_type=? AND id_value=? AND verified=1", tenant, s.IDType, s.IDValue).Scan(&id)
	if err == nil {
		return id, weak, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", weak, err
	}
	err = tx.Conn.QueryRowContext(ctx, "SELECT id FROM principals WHERE tenant=? AND id_type=? AND id_value=?", tenant, s.IDType, s.IDValue).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = NewID()
		if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO principals(id,tenant,id_type,id_value,created_at) VALUES(?,?,?,?,?)", id, tenant, s.IDType, s.IDValue, Now()); err != nil {
			return "", weak, err
		}
		// The identifier the platform gave us is its own basis; it is verified
		// only as "this exact identifier", not as a link to any other one.
		basis := "platform_event"
		if weak {
			basis = "unresolved"
		}
		if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO identity_aliases(tenant,id_type,id_value,principal_id,basis,verified,created_at) VALUES(?,?,?,?,?,?,?)",
			tenant, s.IDType, s.IDValue, id, basis, boolInt(!weak), Now()); err != nil {
			return "", weak, err
		}
		return id, weak, nil
	}
	return id, weak, err
}

// LinkIdentity merges two platform identifiers into one principal. It requires an
// explicit verifiable basis, and a display name is never accepted as one.
func (tx *Tx) LinkIdentity(ctx context.Context, tenant string, from, to Sender, basis string) (any, error) {
	if !contains([]string{"platform_directory", "operator_confirmed", "same_open_id"}, basis) {
		return nil, Fail("invalid_input", "identity basis must be platform_directory, operator_confirmed or same_open_id; a display name is never a basis")
	}
	target, _, err := tx.PrincipalFor(ctx, tenant, to)
	if err != nil {
		return nil, err
	}
	if from.IDType == "" || from.IDValue == "" {
		return nil, Fail("invalid_input", "source identity is required")
	}
	var existing string
	err = tx.Conn.QueryRowContext(ctx, "SELECT principal_id FROM identity_aliases WHERE tenant=? AND id_type=? AND id_value=?", tenant, from.IDType, from.IDValue).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && existing != target {
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE messages SET sender_principal=? WHERE sender_id_type=? AND sender_id_value=?", target, from.IDType, from.IDValue); err != nil {
			return nil, err
		}
	}
	if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO identity_aliases(tenant,id_type,id_value,principal_id,basis,verified,created_at) VALUES(?,?,?,?,?,1,?) ON CONFLICT(tenant,id_type,id_value) DO UPDATE SET principal_id=excluded.principal_id,basis=excluded.basis,verified=1",
		tenant, from.IDType, from.IDValue, target, basis, Now()); err != nil {
		return nil, err
	}
	if _, err = tx.Audit(ctx, "identity.link", basis, objectChange("principal", target)); err != nil {
		return nil, err
	}
	return map[string]any{"principal_id": target, "basis": basis, "verified": true}, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// messageSnapshot is the canonical evidence text for one message revision. It
// includes message identity and sender, so the same words from two people
// produce two distinct sources and can never be merged by content dedupe.
func messageSnapshot(c Channel, e NormalizedEvent, key string, revision int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "channel: %s\nkind: %s\nmessage_key: %s\nrevision: %d\nconversation: %s\n", c.Name, c.Kind, key, revision, e.ConversationID)
	fmt.Fprintf(&b, "sender: %s:%s\n", e.Sender.IDType, e.Sender.IDValue)
	if e.Sender.DisplayName != "" {
		fmt.Fprintf(&b, "sender_display_name: %s\n", e.Sender.DisplayName)
	}
	fmt.Fprintf(&b, "sent_at: %s\n", e.SentAt)
	if e.EditedAt != "" {
		fmt.Fprintf(&b, "edited_at: %s\n", e.EditedAt)
	}
	for _, r := range e.Relations {
		fmt.Fprintf(&b, "relation: %s -> %s\n", r.Kind, r.ProviderMessageID)
	}
	for _, a := range e.Attachments {
		fmt.Fprintf(&b, "attachment: %s (%s)\n", a.Name, a.MediaType)
	}
	b.WriteString("\n")
	b.WriteString(e.Body)
	if e.Quote != nil {
		b.WriteString("\n\nprovider-supplied quote (untrusted background):\n")
		b.WriteString(JSON(e.Quote))
	}
	return b.String()
}

// Intake records one normalized platform event and, when it carries content,
// the message revision and its canonical evidence snapshot. A redelivery of the
// same event is a no-op that reports the original write.
func (tx *Tx) Intake(ctx context.Context, channelValue string, e NormalizedEvent) (IntakeResult, error) {
	var out IntakeResult
	c, err := ReadChannel(ctx, tx.Conn, channelValue)
	if err != nil {
		return out, err
	}
	if err = e.validate(); err != nil {
		return out, err
	}
	if e.Tenant != "" && e.Tenant != c.Tenant {
		return out, Fail("denied", "event tenant %q does not belong to channel tenant %q", e.Tenant, c.Tenant)
	}
	route, err := RouteFor(ctx, tx.Conn, c.ID, e.ConversationID)
	if err != nil {
		return out, err
	}
	if route.Mode == "ignore" {
		return IntakeResult{ChannelID: c.ID, Status: "ignored", Note: "conversation is marked ignore; no message content was stored"}, nil
	}
	if e.Origin == "" {
		e.Origin = "stream"
	}
	// An offline import may not claim to be a live platform observation, which is
	// what stops a hand-written file from forging an online principal.
	if e.Origin == "import" && e.Sender.IDType != "unknown" {
		var n int
		if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM identity_aliases WHERE tenant=? AND id_type=? AND id_value=? AND verified=1", c.Tenant, e.Sender.IDType, e.Sender.IDValue).Scan(&n); err != nil {
			return out, err
		}
		if n == 0 {
			return out, Fail("denied", "imported event claims an unverified identity %s:%s; link it explicitly before import", e.Sender.IDType, e.Sender.IDValue)
		}
	}
	key := messageKey(c.IDNamespace, e.ConversationID, e.ProviderMessageID)
	payload := e.Payload
	if payload == "" {
		payload = "{}"
	}
	dedupe := dedupeKey(c, e)
	eventID := NewID()
	res, err := tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO inbox_events(id,channel_id,dedupe_key,event_kind,provider_event_id,provider_message_id,conversation_id,event_at,received_at,payload,payload_digest,adapter,parse_version,origin,status,source_time_raw) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,'received',?)",
		eventID, c.ID, dedupe, e.Kind, e.ProviderEventID, e.ProviderMessageID, e.ConversationID, e.EventAt, Now(), payload, Hash([]byte(payload)), e.Adapter, e.ParseVersion, e.Origin, e.SourceTimeRaw)
	if err != nil {
		return out, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var status, messageID string
		if err = tx.Conn.QueryRowContext(ctx, "SELECT id,status,message_id FROM inbox_events WHERE dedupe_key=?", dedupe).Scan(&eventID, &status, &messageID); err != nil {
			return out, err
		}
		return IntakeResult{InboxEventID: eventID, ChannelID: c.ID, Duplicate: true, MessageID: messageID, MessageKey: key,
			Status: status, Note: "event was already recorded; nothing was written again"}, nil
	}
	out.InboxEventID = eventID
	out.ChannelID = c.ID
	out.MessageKey = key
	if e.Kind == EventRecall {
		return tx.applyRecall(ctx, c, e, key, out)
	}
	if e.SentAt == "" {
		e.SentAt = e.EventAt
	}
	result, err := tx.applyMessage(ctx, c, route, e, key, out)
	if err != nil {
		return out, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE inbox_events SET status='applied',message_id=? WHERE id=?", result.MessageID, eventID); err != nil {
		return result, err
	}
	if err = tx.correctDWSHistoryTime(ctx, c, e, result); err != nil {
		return result, err
	}
	result.Status = "applied"
	return result, nil
}

func (tx *Tx) applyMessage(ctx context.Context, c Channel, route Route, e NormalizedEvent, key string, out IntakeResult) (IntakeResult, error) {
	principal, weak, err := tx.PrincipalFor(ctx, c.Tenant, e.Sender)
	if err != nil {
		return out, err
	}
	selfAuthored := e.Sender.SelfAuthor
	switch c.Kind {
	case ChannelDwsPersonal:
		selfAuthored = selfAuthored || (e.Sender.IDValue != "" && e.Sender.IDValue == c.Identity.ExpectedUserID)
	case ChannelDingTalkApp:
		selfAuthored = selfAuthored || (c.Identity.RobotCode != "" && e.Sender.IDValue == c.Identity.RobotCode)
	}
	if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO conversations(id,channel_id,provider_conversation_id,kind,created_at) VALUES(?,?,?,?,?)",
		NewID(), c.ID, e.ConversationID, route.ConversationType, Now()); err != nil {
		return out, err
	}
	var messageID string
	var revision int
	err = tx.Conn.QueryRowContext(ctx, "SELECT id,current_revision FROM messages WHERE message_key=?", key).Scan(&messageID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		messageID, revision = NewID(), 1
		addressed := e.Mentioned || route.ConversationType == "direct"
		if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO messages(id,channel_id,message_key,conversation_id,provider_message_id,sender_principal,sender_id_type,sender_id_value,sent_at,current_revision,weak_identity,self_authored,created_at,updated_at,addressed) VALUES(?,?,?,?,?,?,?,?,?,1,?,?,?,?,?)",
			messageID, c.ID, key, e.ConversationID, e.ProviderMessageID, principal, e.Sender.IDType, e.Sender.IDValue, e.SentAt, boolInt(weak), boolInt(selfAuthored), Now(), Now(), boolInt(addressed)); err != nil {
			return out, err
		}
	} else if err != nil {
		return out, err
	}
	if e.Origin == "history" && route.ConversationType == "direct" && e.Sender.IDType == "open_id" {
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE messages SET sender_principal=?,sender_id_type=?,sender_id_value=?,self_authored=?,updated_at=? WHERE id=?", principal, e.Sender.IDType, e.Sender.IDValue, boolInt(selfAuthored), Now(), messageID); err != nil {
			return out, err
		}
	}
	if e.Mentioned || route.ConversationType == "direct" {
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE messages SET addressed=1,updated_at=? WHERE id=? AND addressed=0", Now(), messageID); err != nil {
			return out, err
		}
	}
	out.MessageID = messageID
	// A body identical to the newest revision is the same content seen twice, so
	// no new revision is created and the observation is simply recorded.
	digest := messageContentDigest(e)
	var lastDigest string
	if err = tx.Conn.QueryRowContext(ctx, "SELECT body_digest FROM message_revisions WHERE message_id=? AND revision=?", messageID, revision).Scan(&lastDigest); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	newRevision := errors.Is(err, sql.ErrNoRows) || lastDigest != digest
	if newRevision && !errors.Is(err, sql.ErrNoRows) {
		revision++
	}
	out.Revision, out.NewRevision = revision, newRevision
	if newRevision {
		format := e.Format
		if format == "" {
			format = "text"
		}
		snapshot := messageSnapshot(c, e, key, revision)
		if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO message_revisions(message_id,revision,body,body_digest,format,edited_at,ordering,snapshot,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
			messageID, revision, e.Body, digest, format, e.EditedAt, e.Ordering, JSON(map[string]any{"attachments": e.Attachments, "relations": e.Relations, "quote": e.Quote}), Now()); err != nil {
			return out, err
		}
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE messages SET current_revision=?,updated_at=? WHERE id=?", revision, Now(), messageID); err != nil {
			return out, err
		}
		if strings.TrimSpace(e.Body) != "" {
			source, err := tx.ingestMessageSnapshot(ctx, c, route, messageID, revision, key, e, snapshot)
			if err != nil {
				return out, err
			}
			out.SourceID = source
		}
		for i, a := range e.Attachments {
			if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO message_attachments(message_id,revision,ordinal,name,media_type,resource_id,state) VALUES(?,?,?,?,?,?, 'referenced')",
				messageID, revision, i, a.Name, a.MediaType, a.ResourceID); err != nil {
				return out, err
			}
		}
	}
	if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO message_observations(message_id,revision,channel_id,adapter,inbox_event_id,observed_at) VALUES(?,?,?,?,?,?)",
		messageID, revision, c.ID, e.Adapter, out.InboxEventID, Now()); err != nil {
		return out, err
	}
	for _, r := range e.Relations {
		confidence := r.Confidence
		if confidence == "" {
			confidence = "provider"
		}
		// The parent may be unknown locally; the link is still recorded so a later
		// backfill can resolve it instead of the reference being lost.
		var dst string
		if err = tx.Conn.QueryRowContext(ctx, "SELECT id FROM messages WHERE message_key=?", messageKey(c.IDNamespace, e.ConversationID, r.ProviderMessageID)).Scan(&dst); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO message_relations(src_message_id,kind,dst_provider_message_id,dst_message_id,origin_sender,confidence) VALUES(?,?,?,?,?,?)",
			messageID, r.Kind, r.ProviderMessageID, dst, r.OriginSender, confidence); err != nil {
			return out, err
		}
	}
	// A late backfill resolves relations that pointed at this message.
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE message_relations SET dst_message_id=? WHERE dst_provider_message_id=? AND dst_message_id=''", messageID, e.ProviderMessageID); err != nil {
		return out, err
	}
	return tx.settleAvailability(ctx, messageID, key, out)
}

// settleAvailability applies a recall that arrived before the message body. The
// recall is authoritative, so the backfilled content stays unusable.
func (tx *Tx) settleAvailability(ctx context.Context, messageID, key string, out IntakeResult) (IntakeResult, error) {
	var recalledAt string
	err := tx.Conn.QueryRowContext(ctx, "SELECT recalled_at FROM message_recalls WHERE message_key=?", key).Scan(&recalledAt)
	if errors.Is(err, sql.ErrNoRows) {
		out.Availability = "available"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err = tx.markRecalled(ctx, messageID, recalledAt); err != nil {
		return out, err
	}
	out.Availability = "recalled"
	out.Note = "a recall was already recorded for this message; the backfilled content stays unusable"
	return out, nil
}

func (tx *Tx) applyRecall(ctx context.Context, c Channel, e NormalizedEvent, key string, out IntakeResult) (IntakeResult, error) {
	at := e.RecalledAt
	if at == "" {
		at = e.EventAt
	}
	if _, err := tx.Conn.ExecContext(ctx, "INSERT INTO message_recalls(message_key,recalled_at,inbox_event_id,created_at) VALUES(?,?,?,?) ON CONFLICT(message_key) DO UPDATE SET recalled_at=excluded.recalled_at",
		key, at, out.InboxEventID, Now()); err != nil {
		return out, err
	}
	var messageID string
	err := tx.Conn.QueryRowContext(ctx, "SELECT id FROM messages WHERE message_key=?", key).Scan(&messageID)
	if errors.Is(err, sql.ErrNoRows) {
		// The original has not been seen. The recall is kept so that a later
		// backfill of the same message is immediately marked unusable.
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE inbox_events SET status='pending_original' WHERE id=?", out.InboxEventID); err != nil {
			return out, err
		}
		out.Status = "pending_original"
		out.Availability = "recalled"
		out.Note = "recall recorded before the original message; a later backfill will stay unusable"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err = tx.markRecalled(ctx, messageID, at); err != nil {
		return out, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE inbox_events SET status='applied',message_id=? WHERE id=?", messageID, out.InboxEventID); err != nil {
		return out, err
	}
	out.MessageID, out.Status, out.Availability = messageID, "applied", "recalled"
	return out, nil
}

// markRecalled withdraws the message and every source derived from it. The
// availability change_seq advances so a consumer can tell that a conclusion
// built on this evidence must be re-checked.
func (tx *Tx) markRecalled(ctx context.Context, messageID, at string) error {
	if _, err := tx.Conn.ExecContext(ctx, "UPDATE messages SET availability='recalled',availability_reason=?,updated_at=? WHERE id=?",
		"recalled at "+at, Now(), messageID); err != nil {
		return err
	}
	rows, err := tx.Conn.QueryContext(ctx, "SELECT source_id FROM source_origins WHERE message_id=? AND source_id<>''", messageID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO source_availability(source_id,state,reason,change_seq,updated_at) VALUES(?,'recalled',?,1,?) ON CONFLICT(source_id) DO UPDATE SET state='recalled',reason=excluded.reason,change_seq=source_availability.change_seq+1,updated_at=excluded.updated_at",
			id, "source message recalled at "+at, Now()); err != nil {
			return err
		}
	}
	return nil
}

// ingestMessageSnapshot stores the revision snapshot as a normal Source so the
// existing evidence, fragment and purge machinery applies unchanged.
func (tx *Tx) ingestMessageSnapshot(ctx context.Context, c Channel, route Route, messageID string, revision int, key string, e NormalizedEvent, snapshot string) (string, error) {
	scope := tx.Request.Scope
	tx.Request.Scope = route.WorkspaceID
	defer func() { tx.Request.Scope = scope }()
	uri := "dingtalk://" + c.Name + "/" + e.ConversationID + "/" + e.ProviderMessageID + "#" + fmt.Sprint(revision)
	observed := e.SentAt
	if observed == "" {
		observed = e.EventAt
	}
	source, err := tx.Ingest(ctx, SourceInput{URI: uri, Kind: "message", Content: snapshot, ObservedAt: observed, Lineage: key})
	if err != nil {
		return "", err
	}
	if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO source_origins(source_id,channel_id,message_id,revision,route_id,created_at) VALUES(?,?,?,?,?,?)",
		source.ID, c.ID, messageID, revision, route.ID, Now()); err != nil {
		return "", err
	}
	if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO source_availability(source_id,state,reason,change_seq,updated_at) VALUES(?,'available','',1,?)", source.ID, Now()); err != nil {
		return "", err
	}
	return source.ID, nil
}

type MessageView struct {
	ID           string        `json:"id"`
	ChannelID    string        `json:"channel_id"`
	MessageKey   string        `json:"message_key"`
	Conversation string        `json:"conversation_id"`
	ProviderID   string        `json:"provider_message_id"`
	Sender       string        `json:"sender_principal"`
	SenderIDType string        `json:"sender_id_type"`
	SenderID     string        `json:"sender_id_value"`
	SentAt       string        `json:"sent_at"`
	Revision     int           `json:"current_revision"`
	Availability string        `json:"availability"`
	Reason       string        `json:"availability_reason,omitempty"`
	WeakIdentity bool          `json:"weak_identity"`
	SelfAuthored bool          `json:"self_authored"`
	Body         string        `json:"body,omitempty"`
	Quote        *MessageQuote `json:"quote,omitempty"`
	Revisions    []RevisionOf  `json:"revisions,omitempty"`
	Relations    []Relation    `json:"relations,omitempty"`
	SourceIDs    []string      `json:"source_ids,omitempty"`
}
type RevisionOf struct {
	Revision int    `json:"revision"`
	Body     string `json:"body"`
	Digest   string `json:"body_digest"`
	EditedAt string `json:"edited_at,omitempty"`
	SourceID string `json:"source_id,omitempty"`
}

const messageColumns = "id,channel_id,message_key,conversation_id,provider_message_id,sender_principal,sender_id_type,sender_id_value,sent_at,current_revision,availability,availability_reason,weak_identity,self_authored"

func scanMessage(row scanner) (MessageView, error) {
	var m MessageView
	var weak, self int
	err := row.Scan(&m.ID, &m.ChannelID, &m.MessageKey, &m.Conversation, &m.ProviderID, &m.Sender, &m.SenderIDType, &m.SenderID, &m.SentAt, &m.Revision, &m.Availability, &m.Reason, &weak, &self)
	m.WeakIdentity, m.SelfAuthored = weak == 1, self == 1
	return m, err
}

// MessageList shows only what the caller may see. A conversation the caller did
// not name is never mixed in, so a private thread cannot surface in a group view.
func MessageList(ctx context.Context, q Queryer, channelValue, conversation string, limit int) ([]MessageView, error) {
	if limit < 1 || limit > 500 {
		return nil, Fail("invalid_input", "limit must be 1..500")
	}
	c, err := ReadChannel(ctx, q, channelValue)
	if err != nil {
		return nil, err
	}
	query := "SELECT " + messageColumns + " FROM messages WHERE channel_id=?"
	args := []any{c.ID}
	var retentionDays int
	if e := q.QueryRowContext(ctx, "SELECT retention_days FROM data_sources WHERE channel_id=?", c.ID).Scan(&retentionDays); e == nil && retentionDays > 0 {
		query += " AND julianday(sent_at)>=julianday(?)"
		args = append(args, time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339Nano))
	}
	if conversation != "" {
		query += " AND conversation_id=?"
		args = append(args, conversation)
	}
	query += " ORDER BY sent_at DESC,id LIMIT ?"
	rows, err := q.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MessageView{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		// A recalled message never renders its body, not even in a local listing.
		if out[i].Availability == "available" {
			if err = q.QueryRowContext(ctx, "SELECT body FROM message_revisions WHERE message_id=? AND revision=?", out[i].ID, out[i].Revision).Scan(&out[i].Body); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
	}
	return out, nil
}

type MessageQueryInput struct {
	Conversation     string `json:"conversation_id,omitempty"`
	ConversationType string `json:"conversation_type,omitempty"`
	ContactIDType    string `json:"contact_id_type,omitempty"`
	ContactIDValue   string `json:"contact_id_value,omitempty"`
	Query            string `json:"query,omitempty"`
	Since            string `json:"since,omitempty"`
	Until            string `json:"until,omitempty"`
	Limit            int    `json:"limit"`
}

type ObservedMessage struct {
	MessageView
	SenderDisplayName string `json:"sender_display_name,omitempty"`
	SourceID          string `json:"source_id,omitempty"`
}

type MessageQueryCoverage struct {
	ConversationID   string    `json:"conversation_id"`
	ConversationType string    `json:"conversation_type"`
	Watermark        Watermark `json:"watermark"`
}

type MessageQueryCoverageSummary struct {
	ActiveConversations   int            `json:"active_conversations"`
	ConversationTypes     map[string]int `json:"conversation_types"`
	ObservedConversations int            `json:"observed_conversations"`
	UnresolvedGaps        int            `json:"unresolved_gaps"`
	LatestObservedAt      string         `json:"latest_observed_at,omitempty"`
}

type MessageQueryResult struct {
	Channel         string                      `json:"channel"`
	Query           MessageQueryInput           `json:"query"`
	Messages        []ObservedMessage           `json:"messages"`
	Coverage        []MessageQueryCoverage      `json:"coverage"`
	CoverageSummary MessageQueryCoverageSummary `json:"coverage_summary"`
	DirectEnabledAt string                      `json:"direct_enabled_at,omitempty"`
	AvailableSince  string                      `json:"available_since,omitempty"`
	RetentionDays   int                         `json:"retention_days,omitempty"`
	Note            string                      `json:"note"`
}

const messageQueryColumns = "m.id,m.channel_id,m.message_key,m.conversation_id,m.provider_message_id,m.sender_principal,m.sender_id_type,m.sender_id_value,m.sent_at,m.current_revision,m.availability,m.availability_reason,m.weak_identity,m.self_authored"

func normalizedMessageQueryTimes(since, until string) (string, string, error) {
	parse := func(label, value string) (time.Time, string, error) {
		if value == "" {
			return time.Time{}, "", nil
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return time.Time{}, "", Fail("invalid_input", "%s must use RFC3339", label)
		}
		return parsed, parsed.UTC().Format(time.RFC3339Nano), nil
	}
	from, normalizedSince, err := parse("since", since)
	if err != nil {
		return "", "", err
	}
	to, normalizedUntil, err := parse("until", until)
	if err != nil {
		return "", "", err
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return "", "", Fail("invalid_input", "since must be before until")
	}
	return normalizedSince, normalizedUntil, nil
}

func messageSnapshotField(snapshot, field string) string {
	prefix := field + ": "
	for _, line := range strings.Split(snapshot, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
		if line == "" {
			break
		}
	}
	return ""
}

func messageQueryCoverageSummary(ctx context.Context, q Queryer, channelID string) (MessageQueryCoverageSummary, error) {
	summary := MessageQueryCoverageSummary{ConversationTypes: map[string]int{}}
	err := q.QueryRowContext(ctx, `SELECT count(*),
		coalesce(sum(CASE WHEN w.observed_at IS NOT NULL AND w.observed_at<>'' THEN 1 ELSE 0 END),0),
		coalesce(sum(CASE WHEN w.gap_unresolved=1 THEN 1 ELSE 0 END),0),coalesce(max(w.observed_at),'')
		FROM channel_routes r LEFT JOIN channel_watermarks w ON w.channel_id=r.channel_id AND w.conversation_id=r.conversation_id
		WHERE r.channel_id=? AND r.status='active'`, channelID).Scan(&summary.ActiveConversations, &summary.ObservedConversations, &summary.UnresolvedGaps, &summary.LatestObservedAt)
	if err != nil {
		return summary, err
	}
	rows, err := q.QueryContext(ctx, "SELECT conversation_type,count(*) FROM channel_routes WHERE channel_id=? AND status='active' GROUP BY conversation_type ORDER BY conversation_type", channelID)
	if err != nil {
		return summary, err
	}
	defer rows.Close()
	for rows.Next() {
		var conversationType string
		var count int
		if err = rows.Scan(&conversationType, &count); err != nil {
			return summary, err
		}
		summary.ConversationTypes[conversationType] = count
	}
	return summary, rows.Err()
}

// MessageQuery searches committed observations only. The generated source
// snapshot is searched as well as the body so sender display names can locate a
// message without turning an unverified display name into an identity.
func MessageQuery(ctx context.Context, q Queryer, channelValue string, in MessageQueryInput) (MessageQueryResult, error) {
	out := MessageQueryResult{Query: in, Messages: []ObservedMessage{}, Coverage: []MessageQueryCoverage{},
		Note: "results are observed messages, not formal memory; inspect coverage before treating an empty result as absence"}
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.Limit < 1 || in.Limit > 500 {
		return out, Fail("invalid_input", "limit must be 1..500")
	}
	since, until, err := normalizedMessageQueryTimes(in.Since, in.Until)
	if err != nil {
		return out, err
	}
	in.Since, in.Until = since, until
	in.Query = strings.TrimSpace(in.Query)
	if in.ConversationType != "" && in.ConversationType != "direct" && in.ConversationType != "group" {
		return out, Fail("invalid_input", "conversation type must be direct or group")
	}
	if (in.ContactIDType == "") != (in.ContactIDValue == "") {
		return out, Fail("invalid_input", "contact id type and value must be provided together")
	}
	out.Query = in
	c, err := ReadChannel(ctx, q, channelValue)
	if err != nil {
		return out, err
	}
	out.Channel = c.Name
	var retentionDays int
	var directEnabledAt string
	if e := q.QueryRowContext(ctx, "SELECT retention_days,direct_enabled_at FROM data_sources WHERE channel_id=?", c.ID).Scan(&retentionDays, &directEnabledAt); e == nil {
		out.RetentionDays, out.DirectEnabledAt = retentionDays, directEnabledAt
		if retentionDays > 0 {
			out.AvailableSince = time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339Nano)
		}
	}
	if in.Conversation != "" {
		if _, err = RouteFor(ctx, q, c.ID, in.Conversation); err != nil {
			return out, err
		}
	}
	out.CoverageSummary, err = messageQueryCoverageSummary(ctx, q, c.ID)
	if err != nil {
		return out, err
	}
	query := "SELECT " + messageQueryColumns + ",mr.body,coalesce(s.id,''),coalesce(s.content,'') FROM messages m " +
		"JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision " +
		"JOIN channel_routes cr ON cr.channel_id=m.channel_id AND cr.conversation_id=m.conversation_id " +
		"LEFT JOIN direct_conversation_contacts dc ON dc.channel_id=m.channel_id AND dc.conversation_id=m.conversation_id " +
		"LEFT JOIN source_origins so ON so.message_id=m.id AND so.revision=m.current_revision " +
		"LEFT JOIN sources s ON s.id=so.source_id WHERE m.channel_id=? AND m.availability='available'"
	args := []any{c.ID}
	if out.AvailableSince != "" {
		query += " AND julianday(m.sent_at)>=julianday(?)"
		args = append(args, out.AvailableSince)
	}
	if in.ConversationType != "" {
		query += " AND cr.conversation_type=?"
		args = append(args, in.ConversationType)
	}
	if in.ContactIDType != "" {
		query += " AND dc.peer_id_type=? AND dc.peer_id_value=?"
		args = append(args, in.ContactIDType, in.ContactIDValue)
	}
	if in.Conversation != "" {
		query += " AND m.conversation_id=?"
		args = append(args, in.Conversation)
	}
	if since != "" {
		query += " AND julianday(m.sent_at)>=julianday(?)"
		args = append(args, since)
	}
	if until != "" {
		query += " AND julianday(m.sent_at)<julianday(?)"
		args = append(args, until)
	}
	if in.Query != "" {
		query += " AND (instr(coalesce(s.content,mr.body),?)>0 OR instr(coalesce(dc.display_name,''),?)>0)"
		args = append(args, in.Query, in.Query)
	}
	query += " ORDER BY m.sent_at DESC,m.id DESC LIMIT ?"
	args = append(args, in.Limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	conversationIDs := map[string]bool{}
	for rows.Next() {
		var item ObservedMessage
		var weak, self int
		var snapshot string
		if err = rows.Scan(&item.ID, &item.ChannelID, &item.MessageKey, &item.Conversation, &item.ProviderID, &item.Sender, &item.SenderIDType, &item.SenderID, &item.SentAt, &item.Revision, &item.Availability, &item.Reason, &weak, &self, &item.Body, &item.SourceID, &snapshot); err != nil {
			rows.Close()
			return out, err
		}
		item.WeakIdentity, item.SelfAuthored = weak == 1, self == 1
		item.SenderDisplayName = messageSnapshotField(snapshot, "sender_display_name")
		out.Messages = append(out.Messages, item)
		conversationIDs[item.Conversation] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if in.Conversation != "" {
		conversationIDs[in.Conversation] = true
	}
	ids := make([]string, 0, len(conversationIDs))
	for id := range conversationIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		route, routeErr := RouteFor(ctx, q, c.ID, id)
		if routeErr != nil {
			return out, routeErr
		}
		watermark, watermarkErr := ReadWatermark(ctx, q, c.ID, id)
		if watermarkErr != nil {
			return out, watermarkErr
		}
		out.Coverage = append(out.Coverage, MessageQueryCoverage{ConversationID: id, ConversationType: route.ConversationType, Watermark: watermark})
	}
	return out, nil
}

func ReadMessage(ctx context.Context, q Queryer, value string) (MessageView, error) {
	m, err := scanMessage(q.QueryRowContext(ctx, "SELECT "+messageColumns+" FROM messages WHERE id=? OR message_key=?", value, value))
	if errors.Is(err, sql.ErrNoRows) {
		return m, Fail("not_found", "message %q not found", value)
	}
	if err != nil {
		return m, err
	}
	var expired bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM data_sources d JOIN channel_routes r ON r.channel_id=d.channel_id WHERE d.channel_id=? AND r.conversation_id=? AND r.id IN (SELECT value FROM json_each(d.route_ids)) AND d.retention_days>0 AND julianday(?)<julianday('now','-'||d.retention_days||' days'))`, m.ChannelID, m.Conversation, m.SentAt).Scan(&expired)
	if err != nil {
		return m, err
	}
	if expired {
		m.Availability = "expired"
		m.Reason = "raw text expired; re-query platform to verify"
	}
	rows, err := q.QueryContext(ctx, "SELECT revision,body,body_digest,edited_at FROM message_revisions WHERE message_id=? ORDER BY revision", m.ID)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var r RevisionOf
		if err = rows.Scan(&r.Revision, &r.Body, &r.Digest, &r.EditedAt); err != nil {
			return m, err
		}
		if m.Availability != "available" {
			r.Body = ""
		}
		m.Revisions = append(m.Revisions, r)
	}
	if err = rows.Err(); err != nil {
		return m, err
	}
	if m.Availability == "available" && len(m.Revisions) > 0 {
		m.Body = m.Revisions[len(m.Revisions)-1].Body
		m.Quote, err = runtimeMessageQuote(ctx, q, m.ID, m.Revision)
		if err != nil {
			return m, err
		}
	}
	srcRows, err := q.QueryContext(ctx, "SELECT source_id,revision FROM source_origins WHERE message_id=? ORDER BY revision", m.ID)
	if err != nil {
		return m, err
	}
	defer srcRows.Close()
	m.SourceIDs = []string{}
	for srcRows.Next() {
		var id string
		var rev int
		if err = srcRows.Scan(&id, &rev); err != nil {
			return m, err
		}
		m.SourceIDs = append(m.SourceIDs, id)
		for i := range m.Revisions {
			if m.Revisions[i].Revision == rev {
				m.Revisions[i].SourceID = id
			}
		}
	}
	if err = srcRows.Err(); err != nil {
		return m, err
	}
	relRows, err := q.QueryContext(ctx, "SELECT kind,dst_provider_message_id,origin_sender,confidence FROM message_relations WHERE src_message_id=? ORDER BY kind,dst_provider_message_id", m.ID)
	if err != nil {
		return m, err
	}
	defer relRows.Close()
	m.Relations = []Relation{}
	for relRows.Next() {
		var r Relation
		if err = relRows.Scan(&r.Kind, &r.ProviderMessageID, &r.OriginSender, &r.Confidence); err != nil {
			return m, err
		}
		m.Relations = append(m.Relations, r)
	}
	return m, relRows.Err()
}

// RecordRejectedEvent keeps an unparseable or unsupported event visible instead
// of dropping it, so a parser gap can be found and replayed rather than silently
// becoming an empty message.
func (tx *Tx) RecordRejectedEvent(ctx context.Context, channelID, reason, detail, payload string) error {
	if channelID == "" || reason == "" {
		return Fail("invalid_input", "a rejected event requires a channel and a reason")
	}
	if payload == "" {
		payload = "{}"
	}
	if !json.Valid([]byte(payload)) {
		// The raw line is preserved as a JSON string so the row stays readable
		// while keeping the exact bytes that failed to parse.
		payload = JSON(map[string]string{"raw": payload})
	}
	_, err := tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO inbox_events(id,channel_id,dedupe_key,event_kind,received_at,payload,payload_digest,adapter,parse_version,origin,status,error) VALUES(?,?,?,'rejected',?,?,?,'','','stream','rejected',?)",
		NewID(), channelID, "rejected|"+channelID+"|"+Hash([]byte(payload+reason+detail)), Now(), payload, Hash([]byte(payload)), reason+": "+detail)
	return err
}

// InboxList exposes what was received but not yet applied, which is how an
// interrupted run is diagnosed and replayed.
func InboxList(ctx context.Context, q Queryer, channelValue, status string, limit int) (any, error) {
	if limit < 1 || limit > 500 {
		return nil, Fail("invalid_input", "limit must be 1..500")
	}
	c, err := ReadChannel(ctx, q, channelValue)
	if err != nil {
		return nil, err
	}
	query := "SELECT id,event_kind,provider_event_id,provider_message_id,conversation_id,event_at,received_at,adapter,origin,status,error,message_id FROM inbox_events WHERE channel_id=?"
	args := []any{c.ID}
	if status != "" {
		query += " AND status=?"
		args = append(args, status)
	}
	rows, err := q.QueryContext(ctx, query+" ORDER BY received_at DESC,id LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, kind, eventID, messageProviderID, conversation, eventAt, receivedAt, adapter, origin, st, errText, messageID string
		if err = rows.Scan(&id, &kind, &eventID, &messageProviderID, &conversation, &eventAt, &receivedAt, &adapter, &origin, &st, &errText, &messageID); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"id": id, "event_kind": kind, "provider_event_id": eventID,
			"provider_message_id": messageProviderID, "conversation_id": conversation, "event_at": eventAt,
			"received_at": receivedAt, "adapter": adapter, "origin": origin, "status": st, "error": errText, "message_id": messageID})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	countRows, err := q.QueryContext(ctx, "SELECT status,count(*) FROM inbox_events WHERE channel_id=? GROUP BY status", c.ID)
	if err != nil {
		return nil, err
	}
	defer countRows.Close()
	for countRows.Next() {
		var st string
		var n int
		if err = countRows.Scan(&st, &n); err != nil {
			return nil, err
		}
		counts[st] = n
	}
	if err = countRows.Err(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return map[string]any{"channel": c.Name, "events": items, "status_counts": counts, "statuses": keys}, nil
}
