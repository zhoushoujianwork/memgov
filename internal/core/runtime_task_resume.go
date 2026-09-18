package core

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

type RuntimeAgentSession struct {
	ID            string          `json:"id,omitempty"`
	PolicyDigest  string          `json:"policy_digest,omitempty"`
	ContextDigest string          `json:"context_digest,omitempty"`
	Branch        string          `json:"branch,omitempty"`
	Base          string          `json:"base,omitempty"`
	Workspace     json.RawMessage `json:"workspace,omitempty"`
}

type RuntimeTaskResume struct {
	TaskVersion   int    `json:"task_version"`
	FromAttemptID string `json:"from_attempt_id"`
	Prompt        string `json:"prompt"`
	Mode          string `json:"mode"` // native or replay for pre-persistence tasks
}

func RuntimeTaskResumable(ctx context.Context, q Queryer, t RuntimeTask) error {
	if t.Status != "failed" {
		return Fail("conflict", "only failed or interrupted tasks can continue")
	}
	if len(t.Attempts) == 0 {
		return Fail("conflict", "task has no previous Agent attempt")
	}
	if len(t.Messages) == 0 {
		return Fail("conflict", "task has no available original request")
	}
	a := t.Attempts[len(t.Attempts)-1]
	if a.TaskVersion != t.Version || a.Status != "failed" {
		return Fail("conflict", "previous attempt no longer matches this task version")
	}
	if a.WorkspaceDir == "" {
		return Fail("conflict", "previous attempt has no working directory; use task retry")
	}
	c, err := ReadRuntime(ctx, q, t.RuntimeID)
	if err != nil {
		return err
	}
	admitted, err := runtimeTaskTriggerAdmitted(ctx, q, c, t)
	if err != nil {
		return err
	}
	if !admitted {
		return Fail("denied", "task route is no longer admitted")
	}
	current, err := runtimeTaskMessagesCurrent(ctx, q, t.ID)
	if err != nil {
		return err
	}
	if !current {
		return Fail("conflict", "task source or private session changed; cannot continue")
	}
	if _, err := ResolveRuntimeTaskAgent(ctx, q, c, t); err != nil {
		return err
	}
	if c.ApplicationMode == "direct" {
		for _, message := range t.Messages {
			verified, err := RuntimeOwnerDirectSenderCurrent(ctx, q, c, message.ID)
			if err != nil {
				return err
			}
			if !verified {
				return Fail("denied", "original private sender is no longer the verified owner")
			}
		}
	}
	for _, action := range t.Actions {
		if action.Status == "unknown" || action.Status == "executing" || action.Status == "confirmed" {
			return Fail("conflict", "resolve the external action outcome before continuing")
		}
	}
	var uncertain int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM outbox WHERE job_id=? AND reason NOT IN ('runtime_receipt','runtime_processing_receipt','runtime_completion_receipt','runtime_failure_receipt') AND state IN ('sending','unknown')", t.ID).Scan(&uncertain); err != nil {
		return err
	}
	if uncertain > 0 {
		return Fail("conflict", "resolve the delivery outcome before continuing")
	}
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM runtime_message_actions WHERE task_id=? AND state IN ('sending','unknown')", t.ID).Scan(&uncertain); err != nil {
		return err
	}
	if uncertain > 0 {
		return Fail("conflict", "resolve the Agent communication outcome before continuing")
	}
	return nil
}

func (tx *Tx) ResumeRuntimeTask(ctx context.Context, id string, expectedVersion int) (RuntimeTask, error) {
	t, err := ReadRuntimeTask(ctx, tx.Conn, id)
	if err != nil {
		return t, err
	}
	if expectedVersion > 0 && expectedVersion != t.Version {
		return t, Fail("conflict", "task version changed; refresh before continuing")
	}
	if err = RuntimeTaskResumable(ctx, tx.Conn, t); err != nil {
		return t, err
	}
	a := t.Attempts[len(t.Attempts)-1]
	mode := "replay"
	if a.AgentSession.ID != "" {
		mode = "native"
	}
	resume := RuntimeTaskResume{TaskVersion: t.Version + 1, FromAttemptID: a.ID, Prompt: "继续", Mode: mode}
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status='pending',version=version+1,error_code='',needs_clarification=0,resume=?,updated_at=? WHERE id=? AND version=? AND status='failed'", JSON(resume), Now(), t.ID, t.Version)
	if err != nil {
		return t, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return t, Fail("conflict", "task changed before continuation was queued")
	}
	if err = tx.staleRuntimeTaskWork(ctx, t.ID); err != nil {
		return t, err
	}
	if _, err = tx.SetRuntimeStatus(ctx, t.RuntimeID, "running", ""); err != nil {
		return t, err
	}
	return ReadRuntimeTask(ctx, tx.Conn, t.ID)
}

// Persist before starting Claude so recovery can identify an interrupted call.
func (tx *Tx) RecordRuntimeAgentSession(ctx context.Context, taskID string, version int, attemptID string, session RuntimeAgentSession) error {
	if _, err := uuid.Parse(session.ID); err != nil || session.PolicyDigest == "" || session.ContextDigest == "" {
		return Fail("invalid_input", "Agent session and policy/context digests are required")
	}
	if err := RuntimeAttemptPolicyCurrent(ctx, tx.Conn, attemptID, taskID, version); err != nil {
		return err
	}
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_attempts SET agent_session=? WHERE id=? AND task_id=? AND task_version=? AND status='running'", JSON(session), attemptID, taskID, version)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return Fail("conflict", "attempt changed before session persistence")
	}
	return nil
}
