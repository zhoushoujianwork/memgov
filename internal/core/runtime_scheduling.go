package core

import (
	"context"
	"database/sql"
	"github.com/zhoushoujianwork/memgov/internal/processtree"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Scheduling is shared by the durable runtime and declarative configuration.
// Concurrency in legacy input is an alias for ExecutionConcurrency.
type inMemoryCapacityKey struct{}

// WithInMemoryCapacity marks a runtime-owned claim as already reserved by the
// process-local scheduler. Core callers that do not use that scheduler retain
// the durable compatibility capacity check.
func WithInMemoryCapacity(ctx context.Context) context.Context {
	return context.WithValue(ctx, inMemoryCapacityKey{}, true)
}

func inMemoryCapacity(ctx context.Context) bool {
	v, _ := ctx.Value(inMemoryCapacityKey{}).(bool)
	return v
}

type Scheduling struct {
	AnalysisConcurrency     int `json:"analysis_concurrency,omitempty" yaml:"analysis_concurrency,omitempty"`
	ExecutionConcurrency    int `json:"execution_concurrency,omitempty" yaml:"execution_concurrency,omitempty"`
	AnalysisTimeoutSeconds  int `json:"analysis_timeout_seconds,omitempty" yaml:"analysis_timeout_seconds,omitempty"`
	ExecutionTimeoutSeconds int `json:"execution_timeout_seconds,omitempty" yaml:"execution_timeout_seconds,omitempty"`
	ReviewTimeoutSeconds    int `json:"review_timeout_seconds,omitempty" yaml:"review_timeout_seconds,omitempty"`
}

// Virtual ready time advances on each dispatch, so a large old backlog cannot
// monopolize slots ahead of another ready conversation. Order inside each
// conversation remains local receive order; deferred retries occupy no worker.
func eligibleAnalysisRoutes(ctx context.Context, q Queryer, c RuntimeConfig, at time.Time) ([]string, error) {
	type entry struct{ id, first string }
	entries := []entry{}
	for _, route := range runtimeProcessingRouteIDs(c) {
		var busy int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_work_leases WHERE runtime_id=? AND kind='analysis' AND route_id=? AND released=0`, c.ID, route).Scan(&busy); err != nil {
			return nil, err
		}
		if busy > 0 {
			continue
		}
		var first, next string
		err := q.QueryRowContext(ctx, `SELECT first_seen_at,next_run_at FROM runtime_message_states WHERE runtime_id=? AND route_id=? AND state='pending' ORDER BY first_seen_at,message_id LIMIT 1`, c.ID, route).Scan(&first, &next)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		if next != "" {
			t, e := time.Parse(time.RFC3339Nano, next)
			if e != nil {
				return nil, e
			}
			if t.After(at) {
				continue
			}
		}
		var last string
		if err = q.QueryRowContext(ctx, `SELECT coalesce(max(started_at),'') FROM runtime_batches WHERE runtime_id=? AND route_id=?`, c.ID, route).Scan(&last); err != nil {
			return nil, err
		}
		if last > first {
			first = last
		}
		entries = append(entries, entry{route, first})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].first == entries[j].first {
			return entries[i].id < entries[j].id
		}
		return entries[i].first < entries[j].first
	})
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.id)
	}
	return out, nil
}

func (s *Scheduling) Normalize(legacy int) error {
	if legacy != 0 && s.ExecutionConcurrency != 0 && legacy != s.ExecutionConcurrency {
		return Fail("invalid_input", "concurrency conflicts with execution_concurrency")
	}
	if s.ExecutionConcurrency == 0 {
		s.ExecutionConcurrency = legacy
	}
	if s.ExecutionConcurrency == 0 {
		s.ExecutionConcurrency = 4
	}
	if s.AnalysisConcurrency == 0 {
		s.AnalysisConcurrency = 8
	}
	if s.AnalysisTimeoutSeconds == 0 {
		s.AnalysisTimeoutSeconds = 120
	}
	if s.ExecutionTimeoutSeconds == 0 {
		s.ExecutionTimeoutSeconds = 900
	}
	if s.ReviewTimeoutSeconds == 0 {
		s.ReviewTimeoutSeconds = 120
	}
	if s.AnalysisConcurrency < 1 || s.AnalysisConcurrency > 32 || s.ExecutionConcurrency < 1 || s.ExecutionConcurrency > 32 {
		return Fail("invalid_input", "concurrency must be 1..32")
	}
	for _, n := range []int{s.AnalysisTimeoutSeconds, s.ExecutionTimeoutSeconds, s.ReviewTimeoutSeconds} {
		if n < 1 || n > 86400 {
			return Fail("invalid_input", "stage timeout must be 1..86400 seconds")
		}
	}
	return nil
}

func (tx *Tx) saveScheduling(ctx context.Context, id string, s Scheduling) error {
	_, err := tx.Conn.ExecContext(ctx, `UPDATE runtime_configs SET analysis_concurrency=?,analysis_timeout_seconds=?,execution_timeout_seconds=?,review_timeout_seconds=? WHERE id=?`, s.AnalysisConcurrency, s.AnalysisTimeoutSeconds, s.ExecutionTimeoutSeconds, s.ReviewTimeoutSeconds, id)
	return err
}

// PoolAvailable counts unreaped workers too: cancelling a task does not mean
// its process has exited. The smallest live declaration caps the shared pool.
func PoolAvailable(ctx context.Context, q Queryer, c RuntimeConfig, kind string) (bool, error) {
	if c.ApplicationMode != "proactive" {
		return true, nil
	}
	column := "concurrency"
	if kind == "analysis" {
		column = "analysis_concurrency"
	}
	var cap, active int
	err := q.QueryRowContext(ctx, `SELECT coalesce(min(`+column+`),1) FROM runtime_configs WHERE application_mode='proactive' AND status IN ('running','paused','degraded')`).Scan(&cap)
	if err != nil {
		return false, err
	}
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_work_leases WHERE kind=? AND released=0`, kind).Scan(&active)
	return active < cap, err
}

func (tx *Tx) claimWork(ctx context.Context, c RuntimeConfig, kind, id, route, task string, version int) error {
	if c.ApplicationMode != "proactive" {
		return nil
	}
	seconds := c.ExecutionTimeoutSeconds
	if kind == "analysis" {
		seconds = c.AnalysisTimeoutSeconds
	}
	now := time.Now().UTC()
	applied, err := ReadAppliedConfig(ctx, tx.Conn, 0)
	if err != nil {
		return err
	}
	_, err = tx.Conn.ExecContext(ctx, `INSERT INTO runtime_work_leases(id,runtime_id,kind,route_id,task_id,task_version,policy_version,owner,owner_pid,owner_started,deadline_at,heartbeat_at,lease_until) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, c.ID, kind, route, task, version, applied.Version, id, os.Getpid(), processStarted, now.Add(time.Duration(seconds)*time.Second).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Add(time.Duration(seconds)*time.Second).Format(time.RFC3339Nano))
	if err == nil {
		_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_work_leases SET runtime_policy_digest=? WHERE id=?", runtimePolicyDigest(c), id)
	}
	return err
}

func runtimePolicyDigest(c RuntimeConfig) string {
	c.Scheduling = Scheduling{}
	c.Concurrency, c.ItemThreshold, c.MaxWaitSeconds, c.ReconcileSeconds, c.Version = 0, 0, 0, 0, 0
	c.UpdatedAt, c.LastStartedAt, c.LastStoppedAt, c.Status, c.DegradedReason = "", "", "", "", ""
	return Digest(c)
}

func CheckWorkLease(ctx context.Context, q Queryer, id string) error {
	var invalid int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_work_leases l WHERE l.id=? AND (l.released=1 OR julianday(l.deadline_at)<=julianday('now') OR julianday(l.lease_until)<=julianday('now') OR (l.task_id<>'' AND NOT EXISTS(SELECT 1 FROM runtime_tasks t WHERE t.id=l.task_id AND t.version=l.task_version)))`, id).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return Fail("conflict", "work lease expired or policy changed")
	}
	var version int
	err = q.QueryRowContext(ctx, "SELECT policy_version FROM runtime_work_leases WHERE id=?", id).Scan(&version)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var runtimeID, policy string
	if err = q.QueryRowContext(ctx, "SELECT runtime_id,runtime_policy_digest FROM runtime_work_leases WHERE id=?", id).Scan(&runtimeID, &policy); err != nil {
		return err
	}
	if policy != "" {
		c, e := ReadRuntime(ctx, q, runtimeID)
		if e != nil {
			return e
		}
		if policy != runtimePolicyDigest(c) {
			return Fail("conflict", "runtime permission policy changed during work")
		}
	}
	return checkAppliedPolicyEpoch(ctx, q, version)
}

func schedulingOnlyChange(c RuntimeConfig, in RuntimeConfigInput, channel, owner string) bool {
	if c.ApplicationMode == "proactive" {
		c.DeliveryRouteID = in.DeliveryRouteID
	}
	return c.ChannelID == channel && c.OwnerPrincipalID == owner && c.OwnerIDType == in.Owner.IDType && c.OwnerIDValue == in.Owner.IDValue && c.DeliveryRouteID == in.DeliveryRouteID && Digest(c.RouteIDs) == Digest(in.RouteIDs) && c.ClaudeProfile == in.ClaudeProfile && c.AnalysisModel == in.AnalysisModel && c.ExecutionModel == in.ExecutionModel && c.AgentPreset == in.AgentPreset && c.ApplicationMode == in.ApplicationMode && c.ContextChannelID == in.ContextChannel && Digest(c.AgentCapabilities) == Digest(in.AgentCapabilities) && c.MemoryScope == in.MemoryScope && c.AgentBash == *in.AgentBash && c.ExternalActions == in.ExternalActions
}

func (tx *Tx) RenewWorkLease(ctx context.Context, id string) error {
	if err := CheckWorkLease(ctx, tx.Conn, id); err != nil {
		return err
	}
	_, err := tx.Conn.ExecContext(ctx, `UPDATE runtime_work_leases SET heartbeat_at=?,lease_until=? WHERE id=? AND released=0`, Now(), time.Now().UTC().Add(30*time.Second).Format(time.RFC3339Nano), id)
	return err
}

func (tx *Tx) ReleaseWorkLease(ctx context.Context, id string) error {
	if _, err := tx.Conn.ExecContext(ctx, "DELETE FROM runtime_resource_locks WHERE lease_id=?", id); err != nil {
		return err
	}
	_, err := tx.Conn.ExecContext(ctx, `UPDATE runtime_work_leases SET released=1 WHERE id=?`, id)
	return err
}

var processStarted, _ = processtree.Fingerprint(os.Getpid())

func (tx *Tx) RecordWorkProcess(ctx context.Context, id string, pid int, started string) error {
	if err := CheckWorkLease(ctx, tx.Conn, id); err != nil {
		return err
	}
	_, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_work_leases SET process_pid=?,process_started=?,activity_at=? WHERE id=? AND released=0", pid, started, Now(), id)
	return err
}

func (tx *Tx) SetWorkPhase(ctx context.Context, id, phase string) error {
	_, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_work_leases SET phase=?,activity_at=? WHERE id=? AND released=0", phase, Now(), id)
	return err
}

// Resolve symlinks before opening the transaction. Parent/child roots conflict,
// while siblings and read-only work require no shared write lock.
func CanonicalWriteRoots(roots []string) ([]string, error) {
	out := []string{}
	for _, root := range roots {
		p, e := filepath.Abs(root)
		if e != nil {
			return nil, e
		}
		p, e = filepath.EvalSymlinks(p)
		if e != nil {
			return nil, e
		}
		out = append(out, filepath.Clean(p))
	}
	sort.Strings(out)
	return out, nil
}

func (tx *Tx) LockWorkRoots(ctx context.Context, id string, roots []string) error {
	if err := CheckWorkLease(ctx, tx.Conn, id); err != nil {
		return err
	}
	for _, root := range roots {
		var busy int
		err := tx.Conn.QueryRowContext(ctx, `SELECT count(*) FROM runtime_resource_locks WHERE lease_id<>? AND (root=? OR substr(root,1,length(?)+1)=?||'/' OR substr(?,1,length(root)+1)=root||'/')`, id, root, root, root, root).Scan(&busy)
		if err != nil {
			return err
		}
		if busy > 0 {
			return Fail("resource_busy", "shared write directory is in use")
		}
		if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO runtime_resource_locks(root,lease_id) VALUES(?,?)", root, id); err != nil {
			return err
		}
	}
	return nil
}
