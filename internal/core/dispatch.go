package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Check is one named condition with a result and, when it failed, the reason.
// Passing every check means the technical conditions hold; it is never a record
// that a person approved anything.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Preview is the display contract for a draft: what would be sent, to whom, as
// whom, on what evidence, and which conditions currently hold. Reading it has no
// effect on delivery. There is no approval state to advance, no waiting queue
// and no notion of "approved" to infer from a passing check.
type Preview struct {
	Draft         Draft          `json:"draft"`
	Target        map[string]any `json:"target"`
	Citations     []Citation     `json:"citations"`
	Checks        []Check        `json:"checks"`
	DisplayDigest string         `json:"display_digest"`
	Approval      map[string]any `json:"approval"`
	SendPolicy    string         `json:"send_policy"`
	DeliveryState string         `json:"delivery_state"`
	Sendable      bool           `json:"sendable"`
	Attempts      []Attempt      `json:"attempts,omitempty"`
	Note          string         `json:"note"`
}

// Citation shows one cited memory as the caller may currently see it. A memory
// that has become undisclosable is shown as such rather than silently dropped.
type Citation struct {
	MemoryID   string   `json:"memory_id"`
	Version    int      `json:"version"`
	UsedAt     int      `json:"version_used,omitempty"`
	Title      string   `json:"title,omitempty"`
	Disclosed  bool     `json:"disclosable"`
	Reasons    []string `json:"reasons,omitempty"`
	Sources    []string `json:"evidence_sources,omitempty"`
	Withdrawn  bool     `json:"evidence_withdrawn,omitempty"`
	Unknown    bool     `json:"unknown,omitempty"`
	SourceNote string   `json:"note,omitempty"`
}

type Attempt struct {
	Attempt    int    `json:"attempt"`
	Transport  string `json:"transport"`
	State      string `json:"state"`
	Receipt    string `json:"receipt,omitempty"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// PreviewDraft assembles the display contract. It re-runs every check against
// current state instead of trusting what held when the draft was written, so a
// revoked publication or a withdrawn source shows up before anyone dispatches.
func PreviewDraft(ctx context.Context, q Queryer, id string) (Preview, error) {
	d, err := ReadDraft(ctx, q, id)
	if err != nil {
		return Preview{}, err
	}
	var seqRaw, taskID string
	if err = q.QueryRowContext(ctx, "SELECT evidence_seq,job_id FROM outbox WHERE id=?", d.ID).Scan(&seqRaw, &taskID); err != nil {
		return Preview{}, err
	}
	used := map[string]int{}
	if seqRaw != "" {
		if err = json.Unmarshal([]byte(seqRaw), &used); err != nil {
			return Preview{}, err
		}
	}
	c, err := ReadChannel(ctx, q, d.ChannelID)
	if err != nil {
		return Preview{}, err
	}
	out := Preview{Draft: d, DisplayDigest: d.DisplayDigest, SendPolicy: d.SendPolicy, DeliveryState: d.State,
		Citations: []Citation{}, Checks: []Check{},
		Target: map[string]any{"tenant": c.Tenant, "channel": c.Name, "kind": c.Kind,
			"conversation_id": d.ConversationID, "audience_key": d.AudienceKey, "sender_identity": d.SenderIdentity},
		// The approval fields are constants. memgov displays; it does not decide.
		Approval: map[string]any{"mode": "display_only", "status": "not_evaluated"}}

	route, routeErr := RouteFor(ctx, q, d.ChannelID, d.ConversationID)
	switch {
	case routeErr != nil:
		out.Checks = append(out.Checks, Check{Name: "route", Detail: routeErr.Error()})
	case route.Version != d.RouteVersion:
		out.Checks = append(out.Checks, Check{Name: "route", Detail: fmt.Sprintf(
			"draft was produced against route version %d but the route is now at version %d; re-check before sending", d.RouteVersion, route.Version)})
	default:
		out.Checks = append(out.Checks, Check{Name: "route", Passed: true})
	}
	if taskID != "" {
		// A ready owner-private delivery can retain its outbound direct route
		// after the original group is withdrawn. Dispatch must re-check the
		// task's trigger against the latest processing scope, not its target.
		check := Check{Name: "runtime_trigger", Detail: "task trigger is no longer in its processing scope or the task changed"}
		task, taskErr := ReadRuntimeTask(ctx, q, taskID)
		if taskErr != nil && ErrorCode(taskErr) != "not_found" {
			return Preview{}, taskErr
		}
		if taskErr == nil {
			config, configErr := ReadRuntime(ctx, q, task.RuntimeID)
			if configErr != nil && ErrorCode(configErr) != "not_found" {
				return Preview{}, configErr
			}
			if configErr == nil {
				admitted, scopeErr := runtimeTaskTriggerAdmitted(ctx, q, config, task)
				if scopeErr != nil {
					return Preview{}, scopeErr
				}
				current, sourceErr := runtimeTaskMessagesCurrent(ctx, q, task.ID)
				if sourceErr != nil {
					return Preview{}, sourceErr
				}
				versionDigest := Digest(map[string]any{"task": task.ID, "version": task.Version, "result": d.Content})
				check.Passed = task.Kind != "memory" && RuntimeCompletionPolicy(config) != "record_only" && admitted && current && d.InputDigest == versionDigest && contains([]string{"completed", "awaiting_confirmation", "clarification"}, task.Status)
				if RuntimeCompletionPolicy(config) == "record_only" {
					check.Detail = "background automatic delivery is disabled; Agent communication uses its audited action interface"
				}
				if task.Kind == "memory" {
					check.Detail = "legacy memory task is archived and cannot be delivered"
				}
			}
		}
		out.Checks = append(out.Checks, check)
	}

	for _, id := range d.Citations {
		out.Citations = append(out.Citations, Citation{MemoryID: id, UsedAt: used[id], Unknown: true, SourceNote: "legacy memory reference archived"})
	}
	out.Checks = append(out.Checks, Check{Name: "citations", Passed: len(d.Citations) == 0, Detail: "legacy memory references cannot be dispatched"})

	length := len([]rune(d.Content))
	out.Checks = append(out.Checks, Check{Name: "length", Passed: length <= maxDraftChars,
		Detail: fmt.Sprintf("%d of %d characters", length, maxDraftChars)})
	out.Checks = append(out.Checks, Check{Name: "state", Passed: d.State == "draft" || d.State == "ready",
		Detail: "delivery state is " + d.State})

	// Sending is a capability that has to have been verified, and a policy that
	// has to have been enabled. Neither is implied by the draft existing.
	sendPolicy := d.SendPolicy
	if routeErr == nil {
		sendPolicy = route.SendPolicy
	}
	verified := c.Capabilities.Verified["send"]
	switch {
	case sendPolicy == "draft_only":
		out.Checks = append(out.Checks, Check{Name: "send_policy",
			Detail: "route policy is draft_only; sending is not enabled for this binding"})
	case !verified:
		out.Checks = append(out.Checks, Check{Name: "send_capability",
			Detail: "the channel has no verified send capability; probe it before enabling delivery"})
	default:
		out.Checks = append(out.Checks, Check{Name: "send_policy", Passed: true, Detail: "policy " + sendPolicy})
		out.Checks = append(out.Checks, Check{Name: "send_capability", Passed: true})
	}

	if out.Attempts, err = attemptsFor(ctx, q, d.ID); err != nil {
		return Preview{}, err
	}
	out.Sendable = true
	for _, check := range out.Checks {
		if !check.Passed {
			out.Sendable = false
		}
	}
	out.Note = "this is a display of what would be sent; reading it sends nothing and records no approval. " +
		"all checks passing means the technical conditions hold, not that a person approved it"
	return out, nil
}

func attemptsFor(ctx context.Context, q Queryer, draftID string) ([]Attempt, error) {
	rows, err := q.QueryContext(ctx, "SELECT attempt,transport,state,receipt,error,started_at,finished_at FROM delivery_attempts WHERE outbox_id=? ORDER BY attempt", draftID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var a Attempt
		if err = rows.Scan(&a.Attempt, &a.Transport, &a.State, &a.Receipt, &a.Error, &a.StartedAt, &a.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AuthorizeDispatch is the second permission check, run inside the dispatch
// transaction. It re-reads current state and binds to the exact digest that was
// displayed, so the text that was read is the text that goes out.
func (tx *Tx) AuthorizeDispatch(ctx context.Context, id, expectedDigest string) (Draft, error) {
	p, err := PreviewDraft(ctx, tx.Conn, id)
	if err != nil {
		return Draft{}, err
	}
	if expectedDigest == "" {
		return Draft{}, Fail("invalid_input", "--expected-digest is required; a dispatch must bind the draft that was actually read")
	}
	if expectedDigest != p.DisplayDigest {
		return Draft{}, Fail("conflict", "draft changed since it was displayed; re-read it and dispatch the current digest")
	}
	if !p.Sendable {
		var failed []string
		for _, check := range p.Checks {
			if !check.Passed {
				failed = append(failed, check.Name+": "+check.Detail)
			}
		}
		return Draft{}, Fail("denied", "draft %s is not sendable: %s", id, strings.Join(failed, "; "))
	}
	// The attempt is recorded before the platform is contacted, so a crash between
	// the two leaves a visible attempt rather than a silent gap.
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE outbox SET state='sending',reason='',updated_at=? WHERE id=? AND state IN ('draft','ready')", Now(), id); err != nil {
		return Draft{}, err
	}
	attempt := len(p.Attempts) + 1
	if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO delivery_attempts(id,outbox_id,attempt,transport,idempotency_key,state,started_at) VALUES(?,?,?,?,?,?,?)",
		NewID(), id, attempt, p.Draft.Transport, p.Draft.InputDigest, "sending", Now()); err != nil {
		return Draft{}, err
	}
	if _, err = tx.Audit(ctx, "outbox.dispatch", "authorized dispatch of draft "+id, objectChange("outbox", id)); err != nil {
		return Draft{}, err
	}
	p.Draft.State = "sending"
	return p.Draft, nil
}

// RecordDelivery stores what the platform actually said. An unknown outcome stays
// unknown: a missing receipt is neither a failure nor a delivery, and it must not
// be retried blindly.
func (tx *Tx) RecordDelivery(ctx context.Context, id, state, receipt, detail string) (any, error) {
	if !contains([]string{"accepted", "retryable_failed", "delivery_unknown", "failed"}, state) {
		return nil, Fail("invalid_input", "unsupported delivery state %q", state)
	}
	d, err := ReadDraft(ctx, tx.Conn, id)
	if err != nil {
		return nil, err
	}
	if d.State != "sending" {
		return nil, Fail("conflict", "draft %s is in state %q; only a sending draft has a delivery result", id, d.State)
	}
	if state == "accepted" && receipt == "" {
		return nil, Fail("invalid_input", "accepted requires the platform receipt; without it the result is delivery_unknown")
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE outbox SET state=?,reason=?,updated_at=? WHERE id=?", state, detail, Now(), id); err != nil {
		return nil, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE delivery_attempts SET state=?,receipt=?,error=?,finished_at=? WHERE outbox_id=? AND state='sending'",
		state, receipt, detail, Now(), id); err != nil {
		return nil, err
	}
	if _, err = tx.Audit(ctx, "outbox.delivery", "delivery result "+state, objectChange("outbox", id)); err != nil {
		return nil, err
	}
	out := map[string]any{"draft_id": id, "state": state, "receipt": receipt}
	switch state {
	case "accepted":
		out["note"] = "the platform accepted the message; delivered and read are separate facts and are not claimed here"
		out["retry_safe"] = false
	case "delivery_unknown":
		out["note"] = "the outcome is unknown; do not resend without a reliable query or native idempotency, because the message may already be out"
		out["retry_safe"] = false
	case "retryable_failed":
		out["note"] = "the platform refused in a way that allows a retry; the request is known not to have been delivered"
		out["retry_safe"] = true
	default:
		out["note"] = "the platform refused the message"
		out["retry_safe"] = false
	}
	return out, nil
}

// CancelDraft withdraws a draft that has not been handed to the platform. A
// sending draft cannot be cancelled, because cancelling it locally would not
// recall a message that may already be out.
func (tx *Tx) CancelDraft(ctx context.Context, id, reason string) (any, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, Fail("invalid_input", "cancelling a draft requires a reason")
	}
	d, err := ReadDraft(ctx, tx.Conn, id)
	if err != nil {
		return nil, err
	}
	if !contains([]string{"draft", "ready", "stale", "blocked"}, d.State) {
		return nil, Fail("conflict", "draft %s is in state %q and cannot be cancelled; a handed-over message is not withdrawn by a local cancel", id, d.State)
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE outbox SET state='cancelled',reason=?,updated_at=? WHERE id=?", reason, Now(), id); err != nil {
		return nil, err
	}
	if _, err = tx.Audit(ctx, "outbox.cancel", reason, objectChange("outbox", id)); err != nil {
		return nil, err
	}
	return map[string]any{"draft_id": id, "state": "cancelled", "sent": false}, nil
}

// Reconcile records a verifiable conclusion about an unknown delivery. It only
// accepts an outcome that came with evidence; it never invents a platform proof
// of delivery, and it never turns silence into a result.
func (tx *Tx) Reconcile(ctx context.Context, id, outcome, receipt, evidence string) (any, error) {
	d, err := ReadDraft(ctx, tx.Conn, id)
	if err != nil {
		return nil, err
	}
	if d.State != "delivery_unknown" {
		return nil, Fail("conflict", "draft %s is in state %q; only an unknown delivery needs reconciling", id, d.State)
	}
	if strings.TrimSpace(evidence) == "" {
		return nil, Fail("invalid_input", "reconciling requires the evidence that was checked; an assumption is not a conclusion")
	}
	switch outcome {
	case "accepted":
		if receipt == "" {
			return nil, Fail("invalid_input", "an accepted reconciliation requires the platform receipt")
		}
	case "not_sent":
		outcome = "retryable_failed"
	default:
		return nil, Fail("invalid_input", "reconciliation outcome must be accepted or not_sent")
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE outbox SET state=?,reason=?,updated_at=? WHERE id=?", outcome, "reconciled: "+evidence, Now(), id); err != nil {
		return nil, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE delivery_attempts SET state=?,receipt=?,error=?,finished_at=? WHERE outbox_id=? AND attempt=(SELECT MAX(attempt) FROM delivery_attempts WHERE outbox_id=?)",
		outcome, receipt, "reconciled: "+evidence, Now(), id, id); err != nil {
		return nil, err
	}
	if _, err = tx.Audit(ctx, "outbox.reconcile", "reconciled to "+outcome+": "+evidence, objectChange("outbox", id)); err != nil {
		return nil, err
	}
	return map[string]any{"draft_id": id, "state": outcome, "evidence": evidence,
		"note": "reconciliation records a checked conclusion; it is not a platform delivery proof"}, nil
}
