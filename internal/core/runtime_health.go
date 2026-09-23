package core

import (
	"context"
	"time"
)

type RuntimeWorkStatus struct {
	AnalysisActive       int    `json:"analysis_active"`
	ExecutionActive      int    `json:"execution_active"`
	AnalysisLimit        int    `json:"analysis_limit"`
	ExecutionLimit       int    `json:"execution_limit"`
	QueuedTasks          int    `json:"queued_tasks"`
	OldestWaitingAt      string `json:"oldest_waiting_at"`
	OldestWaitingSeconds int64  `json:"oldest_waiting_seconds"`
	LastAnalysisAt       string `json:"last_analysis_at"`
	RetryBatches         int    `json:"retry_batches"`
	AnalysisGaps         int    `json:"analysis_gaps"`
	TimedOut             int    `json:"timed_out"`
	AnalysisHealth       string `json:"analysis_health"`
	TaskHealth           string `json:"task_health"`
}

func ReadRuntimeWorkStatus(ctx context.Context, q Queryer, c RuntimeConfig) (RuntimeWorkStatus, error) {
	all, err := readRuntimeWorkStatuses(ctx, q)
	return all[c.ID], err
}

func readRuntimeWorkStatuses(ctx context.Context, q Queryer) (map[string]RuntimeWorkStatus, error) {
	out := map[string]RuntimeWorkStatus{}
	rows, err := q.QueryContext(ctx, `WITH execution_workers AS (
 SELECT id,runtime_id FROM runtime_work_leases WHERE released=0 AND kind='execution'
 UNION SELECT a.id,t.runtime_id FROM runtime_attempts a JOIN runtime_tasks t ON t.id=a.task_id
 JOIN runtime_configs r ON r.id=t.runtime_id WHERE a.status='running' AND r.application_mode='group_mention'
 UNION SELECT a.id,t.runtime_id FROM runtime_action_attempts a JOIN runtime_tasks t ON t.id=a.task_id
 JOIN runtime_configs r ON r.id=t.runtime_id WHERE a.status='running' AND r.application_mode='group_mention'
)
SELECT c.id,c.max_wait_seconds,
 (SELECT count(*) FROM runtime_work_leases l JOIN runtime_configs r ON r.id=l.runtime_id WHERE l.released=0 AND l.kind='analysis'
 AND ((c.application_mode='proactive' AND r.application_mode='proactive') OR (c.application_mode<>'proactive' AND l.runtime_id=c.id))),
 (SELECT count(*) FROM execution_workers w JOIN runtime_configs r ON r.id=w.runtime_id
 WHERE (c.application_mode='proactive' AND r.application_mode='proactive') OR (c.application_mode<>'proactive' AND w.runtime_id=c.id)),
 (SELECT count(*) FROM runtime_tasks WHERE runtime_id=c.id AND status='pending'),
 (SELECT coalesce(min(first_seen_at),'') FROM runtime_message_states WHERE runtime_id=c.id AND state='pending'),
 (SELECT coalesce(max(finished_at),'') FROM runtime_batches WHERE runtime_id=c.id AND status='completed'),
 (SELECT count(*) FROM runtime_message_states WHERE runtime_id=c.id AND state='pending' AND retry_count>0),
 (SELECT count(*) FROM runtime_message_states WHERE runtime_id=c.id AND state='analysis_failed'),
 (SELECT count(*) FROM runtime_batches WHERE runtime_id=c.id AND error_code='analysis_timeout')+
 (SELECT count(*) FROM runtime_attempts a JOIN runtime_tasks t ON t.id=a.task_id WHERE t.runtime_id=c.id AND a.error_code='execution_timeout'),
 CASE WHEN c.application_mode='proactive' THEN (SELECT coalesce(min(analysis_concurrency),8) FROM runtime_configs WHERE application_mode='proactive' AND status IN ('running','paused','degraded')) ELSE c.analysis_concurrency END,
 CASE WHEN c.application_mode='proactive' THEN (SELECT coalesce(min(concurrency),1) FROM runtime_configs WHERE application_mode='proactive' AND status IN ('running','paused','degraded')) ELSE c.concurrency END,
 (SELECT count(*) FROM runtime_tasks WHERE runtime_id=c.id AND (status IN ('failed','blocked','clarification','awaiting_confirmation','action_unknown','action_failed','stale'))),
 (SELECT count(*) FROM runtime_tasks WHERE runtime_id=c.id AND status='running')
 FROM runtime_configs c`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		s := RuntimeWorkStatus{AnalysisHealth: "idle", TaskHealth: "idle"}
		var id string
		var maxWait, blocked, running int
		if err = rows.Scan(&id, &maxWait, &s.AnalysisActive, &s.ExecutionActive, &s.QueuedTasks, &s.OldestWaitingAt, &s.LastAnalysisAt, &s.RetryBatches, &s.AnalysisGaps, &s.TimedOut, &s.AnalysisLimit, &s.ExecutionLimit, &blocked, &running); err != nil {
			return nil, err
		}
		if t, e := time.Parse(time.RFC3339Nano, s.OldestWaitingAt); e == nil {
			s.OldestWaitingSeconds = max(0, int64(time.Since(t).Seconds()))
			s.AnalysisHealth = "collecting"
			if s.OldestWaitingSeconds > int64(maxWait+5) {
				s.AnalysisHealth = "delayed"
			}
		} else if s.LastAnalysisAt != "" {
			s.AnalysisHealth = "up_to_date"
		}
		if s.AnalysisGaps > 0 {
			s.AnalysisHealth = "coverage_gap"
		}
		if running > 0 {
			s.TaskHealth = "running"
		}
		if s.QueuedTasks > 0 {
			s.TaskHealth = "queued"
		}
		if blocked > 0 {
			s.TaskHealth = "needs_attention"
		}
		out[id] = s
	}
	return out, rows.Err()
}
