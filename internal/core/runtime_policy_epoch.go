package core

import (
	"context"
	"encoding/json"
	"strings"
)

// RuntimeAttemptPolicyCurrent checks the immutable declaration epoch recorded
// at claim time. A change between claim and an external tool call must be
// observed before the tool runs, not only when its result is delivered.
func RuntimeAttemptPolicyCurrent(ctx context.Context, q Queryer, attemptID, taskID string, taskVersion int) error {
	if err := CheckWorkLease(ctx, q, attemptID); err != nil {
		return err
	}
	var claimedVersion, version int
	var status string
	if err := q.QueryRowContext(ctx, `SELECT task_version,applied_config_version,status FROM runtime_attempts
WHERE id=? AND task_id=?`, attemptID, taskID).Scan(&version, &claimedVersion, &status); err != nil {
		return err
	}
	if version != taskVersion || status != "running" {
		return Fail("conflict", "runtime attempt changed before executing")
	}
	return checkAppliedPolicyEpoch(ctx, q, claimedVersion)
}

// Scheduling is operational, not authorization. Every other declaration
// change still fences the old attempt, including permission reductions.
func checkAppliedPolicyEpoch(ctx context.Context, q Queryer, version int) error {
	latest, err := ReadAppliedConfig(ctx, q, 0)
	if err != nil {
		return err
	}
	if latest.Version == version {
		return nil
	}
	if version > 0 {
		prior, e := ReadAppliedConfig(ctx, q, version)
		if e != nil {
			return e
		}
		strip := func(raw json.RawMessage) string {
			var v map[string]any
			if json.Unmarshal(raw, &v) != nil {
				return "invalid"
			}
			if apps, ok := v["applications"].(map[string]any); ok {
				if p, ok := apps["proactive"].(map[string]any); ok {
					for _, k := range []string{"concurrency", "analysis_concurrency", "execution_concurrency", "analysis_timeout_seconds", "execution_timeout_seconds", "review_timeout_seconds", "batch"} {
						delete(p, k)
					}
				}
			}
			return Digest(v)
		}
		if prior.SchemaVersion == latest.SchemaVersion && strip(prior.Declaration) == strip(latest.Declaration) {
			return nil
		}
	}
	return Fail("conflict", "applied permission configuration changed during work")
}

func (tx *Tx) CheckRuntimeAttemptPolicy(ctx context.Context, attemptID, taskID string, taskVersion int) error {
	return RuntimeAttemptPolicyCurrent(ctx, tx.Conn, attemptID, taskID, taskVersion)
}

func RuntimeActionAttemptPolicyCurrent(ctx context.Context, q Queryer, attemptID, actionID string, taskVersion int) error {
	if err := CheckWorkLease(ctx, q, attemptID); err != nil {
		return err
	}
	var claimedVersion, version int
	var status string
	if err := q.QueryRowContext(ctx, `SELECT task_version,applied_config_version,status FROM runtime_action_attempts
WHERE id=? AND action_id=?`, attemptID, actionID).Scan(&version, &claimedVersion, &status); err != nil {
		return err
	}
	if version != taskVersion || status != "running" {
		return Fail("conflict", "runtime action attempt changed before executing")
	}
	err := checkAppliedPolicyEpoch(ctx, q, claimedVersion)
	if err != nil {
		return err
	}
	var origin, runtimeID string
	if err = q.QueryRowContext(ctx, `SELECT a.confirmation_origin,t.runtime_id FROM runtime_pending_actions a JOIN runtime_tasks t ON t.id=a.task_id WHERE a.id=?`, actionID).Scan(&origin, &runtimeID); err != nil {
		return err
	}
	if strings.HasPrefix(origin, "dingtalk_message:") {
		var action RuntimePendingAction
		if err = q.QueryRowContext(ctx, "SELECT id,task_id,kind,payload_digest,confirmation_origin FROM runtime_pending_actions WHERE id=?", actionID).Scan(&action.ID, &action.TaskID, &action.Kind, &action.PayloadDigest, &action.ConfirmationOrigin); err != nil {
			return err
		}
		if err = destructiveApprovalCurrent(ctx, q, action); err != nil {
			return err
		}
		if action.Kind == RuntimeBotForwardAction {
			if err = runtimeBotForwardApprovalCurrent(ctx, q, actionID); err != nil {
				return err
			}
		}
	}
	if strings.HasPrefix(origin, "dingtalk_card:") {
		config, err := ReadRuntime(ctx, q, runtimeID)
		if err != nil {
			return err
		}
		taskID := ""
		if err = q.QueryRowContext(ctx, `SELECT task_id FROM runtime_pending_actions WHERE id=?`, actionID).Scan(&taskID); err != nil {
			return err
		}
		task, err := ReadRuntimeTask(ctx, q, taskID)
		if err != nil {
			return err
		}
		found := false
		for _, action := range task.Actions {
			if action.ID == actionID {
				found = true
				err = runtimeCardActionCurrent(ctx, q, config, action)
				break
			}
		}
		if !found {
			return Fail("conflict", "approved card action disappeared")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (tx *Tx) CheckRuntimeActionAttemptPolicy(ctx context.Context, attemptID, actionID string, taskVersion int) error {
	return RuntimeActionAttemptPolicyCurrent(ctx, tx.Conn, attemptID, actionID, taskVersion)
}
