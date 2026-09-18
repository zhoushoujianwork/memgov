package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type RuntimeReview struct {
	ID, TaskID, RuntimeID, ExecutionID, DeadlineAt, CandidateID string
	TaskVersion                                                 int
	Candidate                                                   *CandidateInput
}

func (tx *Tx) QueueRuntimeReview(ctx context.Context, task RuntimeTask, attemptID string, candidate *CandidateInput) error {
	var deadline string
	if err := tx.Conn.QueryRowContext(ctx, "SELECT deadline_at FROM runtime_work_leases WHERE id=?", attemptID).Scan(&deadline); err != nil {
		return err
	}
	_, err := tx.Conn.ExecContext(ctx, `INSERT INTO runtime_reviews(id,task_id,task_version,runtime_id,source_attempt_id,candidate_input,deadline_at,created_at) VALUES(?,?,?,?,?,?,?,?)`, NewID(), task.ID, task.Version, task.RuntimeID, attemptID, JSON(candidate), deadline, Now())
	if err != nil {
		return err
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET memory_status='pending',memory_error_code='' WHERE id=? AND version=?", task.ID, task.Version)
	return err
}

func RuntimeReviewReady(ctx context.Context, q Queryer, runtimeID string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM runtime_reviews WHERE runtime_id=? AND status='pending'", runtimeID).Scan(&count)
	return count > 0, err
}

func (tx *Tx) ClaimRuntimeReview(ctx context.Context, c RuntimeConfig) (RuntimeReview, error) {
	var r RuntimeReview
	var err error
	c, err = ReadRuntime(ctx, tx.Conn, c.ID)
	if err != nil {
		return r, err
	}
	if c.Status != "running" {
		return r, nil
	}
	available, err := PoolAvailable(ctx, tx.Conn, c, "execution")
	if err != nil || !available {
		return r, err
	}
	var busy int
	err = tx.Conn.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM runtime_tasks t JOIN runtime_configs c ON c.id=t.runtime_id WHERE c.application_mode='proactive' AND c.status='running' AND t.status='pending' AND t.kind<>'memory')+
 CASE WHEN (SELECT count(*) FROM runtime_work_leases l JOIN runtime_tasks t ON t.id=l.task_id WHERE l.released=0 AND (t.kind='memory' OR l.phase='review'))>=2 THEN 1 ELSE 0 END`).Scan(&busy)
	if err != nil || busy > 0 {
		return r, err
	}
	var raw string
	err = tx.Conn.QueryRowContext(ctx, `SELECT id,task_id,task_version,runtime_id,candidate_input,deadline_at,candidate_id FROM runtime_reviews r WHERE runtime_id=? AND status='pending'
 AND NOT EXISTS(SELECT 1 FROM runtime_work_leases l WHERE l.task_id=r.task_id AND l.released=0) ORDER BY created_at,id LIMIT 1`, c.ID).Scan(&r.ID, &r.TaskID, &r.TaskVersion, &r.RuntimeID, &raw, &r.DeadlineAt, &r.CandidateID)
	if err == sql.ErrNoRows {
		return RuntimeReview{}, nil
	}
	if err != nil {
		return r, err
	}
	var currentVersion int
	if err = tx.Conn.QueryRowContext(ctx, "SELECT version FROM runtime_tasks WHERE id=?", r.TaskID).Scan(&currentVersion); err != nil {
		return r, err
	}
	if currentVersion != r.TaskVersion {
		_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_reviews SET status='failed',error_code='memory_version_conflict',finished_at=? WHERE id=?", Now(), r.ID)
		return RuntimeReview{}, err
	}
	deadline, e := time.Parse(time.RFC3339Nano, r.DeadlineAt)
	if e != nil || !deadline.After(time.Now()) {
		_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_reviews SET status='failed',error_code='review_budget_exhausted',finished_at=? WHERE id=?", Now(), r.ID)
		if err == nil {
			_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET memory_status='failed',memory_error_code='review_budget_exhausted' WHERE id=? AND version=?", r.TaskID, r.TaskVersion)
		}
		return RuntimeReview{}, err
	}
	if err = json.Unmarshal([]byte(raw), &r.Candidate); err != nil {
		return r, err
	}
	r.ExecutionID = NewID()
	if err = tx.claimWork(ctx, c, "execution", r.ExecutionID, "", r.TaskID, r.TaskVersion); err != nil {
		return r, err
	}
	if limit := time.Now().Add(time.Duration(c.ReviewTimeoutSeconds) * time.Second); limit.Before(deadline) {
		deadline = limit
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_work_leases SET deadline_at=?,phase='review' WHERE id=?", deadline.UTC().Format(time.RFC3339Nano), r.ExecutionID)
	if err != nil {
		return r, err
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_reviews SET status='running',execution_id=?,started_at=? WHERE id=? AND status='pending'", r.ExecutionID, Now(), r.ID)
	if err == nil {
		_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET memory_status='reviewing' WHERE id=? AND version=?", r.TaskID, r.TaskVersion)
	}
	return r, err
}

func CheckRuntimeReview(ctx context.Context, q Queryer, r RuntimeReview) error {
	if err := CheckWorkLease(ctx, q, r.ExecutionID); err != nil {
		return err
	}
	var valid int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_reviews r JOIN runtime_tasks t ON t.id=r.task_id WHERE r.id=? AND r.execution_id=? AND r.status='running' AND r.task_version=t.version AND t.version=?`, r.ID, r.ExecutionID, r.TaskVersion).Scan(&valid)
	if err != nil {
		return err
	}
	if valid != 1 {
		return Fail("conflict", "memory review version changed")
	}
	current, err := runtimeTaskMessagesCurrent(ctx, q, r.TaskID)
	if err != nil {
		return err
	}
	if !current {
		return Fail("evidence_unavailable", "memory evidence expired or changed")
	}
	return nil
}

func (tx *Tx) FinishRuntimeReview(ctx context.Context, r RuntimeReview, candidateID, status, code string, usage any) error {
	if status == "applied" || status == "rejected" {
		if err := CheckRuntimeReview(ctx, tx.Conn, r); err != nil {
			return err
		}
	}
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_reviews SET candidate_id=?,status=?,error_code=?,usage=?,finished_at=? WHERE id=? AND execution_id=? AND status='running'", candidateID, status, code, JSON(usage), Now(), r.ID, r.ExecutionID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Fail("conflict", "review already finished")
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET candidate_id=?,memory_status=?,memory_error_code=? WHERE id=? AND version=?", candidateID, status, code, r.TaskID, r.TaskVersion)
	return err
}
