package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// PlatformMessageProof is a result of looking up one message at the provider.
// EventKey and GroupKey are opaque provider values, never message text or a
// local time/hash heuristic. There is deliberately no production lookup here
// until the provider can prove equivalence across DWS and the app transport.
type PlatformMessageProof struct {
	Issuer   string
	GroupKey string
	EventKey string
}

type PlatformMessageVerifier interface {
	LookupMessage(context.Context, Channel, MessageView) (PlatformMessageProof, error)
}

type MessageAssociation struct {
	ID               string `json:"id,omitempty"`
	Status           string `json:"status"`
	FirstMessageID   string `json:"first_message_id"`
	SecondMessageID  string `json:"second_message_id"`
	ProofIssuer      string `json:"proof_issuer,omitempty"`
	ProofDigest      string `json:"proof_digest,omitempty"`
	ClaimedMessageID string `json:"claimed_message_id,omitempty"`
	ClaimedRuntimeID string `json:"claimed_runtime_id,omitempty"`
	ClaimedBatchID   string `json:"claimed_batch_id,omitempty"`
	VerifiedAt       string `json:"verified_at,omitempty"`
}

// AssociateVerifiedMessages asks an authoritative provider lookup about each
// observation independently. Missing or unequal platform proof remains
// unresolved; an apparent match in words, timestamp, sender, or group route is
// not enough. This method is intentionally only an internal adapter API.
func (tx *Tx) AssociateVerifiedMessages(ctx context.Context, firstID, secondID string, verifier PlatformMessageVerifier) (MessageAssociation, error) {
	if firstID == secondID || firstID == "" || secondID == "" || verifier == nil {
		return MessageAssociation{}, Fail("invalid_input", "two distinct messages and a platform verifier are required")
	}
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}
	out := MessageAssociation{Status: "unresolved", FirstMessageID: firstID, SecondMessageID: secondID}
	first, err := ReadMessage(ctx, tx.Conn, firstID)
	if err != nil {
		return out, err
	}
	second, err := ReadMessage(ctx, tx.Conn, secondID)
	if err != nil {
		return out, err
	}
	firstChannel, err := ReadChannel(ctx, tx.Conn, first.ChannelID)
	if err != nil {
		return out, err
	}
	secondChannel, err := ReadChannel(ctx, tx.Conn, second.ChannelID)
	if err != nil {
		return out, err
	}
	if firstChannel.ID == secondChannel.ID || firstChannel.Tenant != secondChannel.Tenant || firstChannel.Kind == secondChannel.Kind {
		return out, Fail("denied", "association requires different transports in one tenant")
	}
	if firstChannel.Kind != ChannelDwsPersonal && secondChannel.Kind != ChannelDwsPersonal {
		return out, Fail("denied", "association requires a DWS observation")
	}
	if firstChannel.Kind != ChannelDingTalkApp && secondChannel.Kind != ChannelDingTalkApp {
		return out, Fail("denied", "association requires an application observation")
	}
	if prior, e := ReadMessageAssociation(ctx, tx.Conn, firstID); e == nil {
		if prior.FirstMessageID == firstID && prior.SecondMessageID == secondID {
			return prior, nil
		}
		return out, Fail("conflict", "message already has a verified association")
	} else if ErrorCode(e) != "not_found" {
		return out, e
	}
	if prior, e := ReadMessageAssociation(ctx, tx.Conn, secondID); e == nil {
		return out, Fail("conflict", "message already has a verified association: %s", prior.ID)
	} else if ErrorCode(e) != "not_found" {
		return out, e
	}
	a, err := verifier.LookupMessage(ctx, firstChannel, first)
	if err != nil {
		return out, err
	}
	b, err := verifier.LookupMessage(ctx, secondChannel, second)
	if err != nil {
		return out, err
	}
	if a.Issuer != "platform_lookup" || b.Issuer != "platform_lookup" ||
		strings.TrimSpace(a.GroupKey) == "" || strings.TrimSpace(a.EventKey) == "" ||
		a.GroupKey != b.GroupKey || a.EventKey != b.EventKey {
		return out, nil
	}
	out.ID, out.Status, out.ProofIssuer, out.ProofDigest, out.VerifiedAt = NewID(), "verified", "platform_lookup", Digest(map[string]string{"group": a.GroupKey, "event": a.EventKey}), Now()
	// If one observation already entered a batch, a late proof prevents its
	// counterpart from starting fresh work. It cannot retract earlier deliveries.
	err = tx.Conn.QueryRowContext(ctx, `SELECT runtime_id,message_id,batch_id FROM runtime_message_states
WHERE message_id IN (?,?) AND state IN ('batched','processed') ORDER BY first_seen_at,runtime_id LIMIT 1`, firstID, secondID).
		Scan(&out.ClaimedRuntimeID, &out.ClaimedMessageID, &out.ClaimedBatchID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MessageAssociation{}, err
	}
	_, err = tx.Conn.ExecContext(ctx, `INSERT INTO verified_message_associations(id,tenant,first_message_id,second_message_id,proof_issuer,proof_digest,claimed_message_id,claimed_runtime_id,claimed_batch_id,verified_at,claimed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, out.ID, firstChannel.Tenant, firstID, secondID, out.ProofIssuer, out.ProofDigest,
		out.ClaimedMessageID, out.ClaimedRuntimeID, out.ClaimedBatchID, out.VerifiedAt, Now())
	if err != nil {
		return MessageAssociation{}, err
	}
	_, err = tx.Audit(ctx, "message.association.verified", "platform lookup verified a cross-transport message association", objectChange("message_association", out.ID))
	return out, err
}

// ReadMessageAssociation reports unresolved when no verified platform mapping
// exists. It never invents equality from route configuration or similar text.
func ReadMessageAssociation(ctx context.Context, q Queryer, messageID string) (MessageAssociation, error) {
	var out MessageAssociation
	err := q.QueryRowContext(ctx, `SELECT id,first_message_id,second_message_id,proof_issuer,proof_digest,claimed_message_id,claimed_runtime_id,claimed_batch_id,verified_at
FROM verified_message_associations WHERE first_message_id=? OR second_message_id=?`, messageID, messageID).
		Scan(&out.ID, &out.FirstMessageID, &out.SecondMessageID, &out.ProofIssuer, &out.ProofDigest, &out.ClaimedMessageID, &out.ClaimedRuntimeID, &out.ClaimedBatchID, &out.VerifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, Fail("not_found", "message has no verified association")
	}
	out.Status = "verified"
	return out, err
}

// MessageAssociationStatus distinguishes an existing but unresolved message
// from a missing local message, which keeps operator inspection honest.
func MessageAssociationStatus(ctx context.Context, q Queryer, messageID string) (MessageAssociation, error) {
	if _, err := ReadMessage(ctx, q, messageID); err != nil {
		return MessageAssociation{}, err
	}
	out, err := ReadMessageAssociation(ctx, q, messageID)
	if ErrorCode(err) == "not_found" {
		return MessageAssociation{Status: "unresolved", FirstMessageID: messageID}, nil
	}
	return out, err
}
