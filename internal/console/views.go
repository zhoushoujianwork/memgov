package console

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/observation"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
	localservice "github.com/zhoushoujianwork/memgov/internal/service"
)

type RuntimeView struct {
	Work                   core.RuntimeWorkStatus `json:"work"`
	ID                     string                 `json:"id"`
	Name                   string                 `json:"name"`
	Mode                   string                 `json:"mode"`
	Status                 string                 `json:"status"`
	ProcessState           string                 `json:"process_state"`
	Runners                []observation.Runner   `json:"runners"`
	Tasks                  map[string]int         `json:"tasks"`
	PendingMessages        int                    `json:"pending_messages"`
	WaitingReceiptMessages int                    `json:"waiting_receipt_messages"`
	PendingActions         int                    `json:"pending_actions"`
	ConfigVersion          int                    `json:"config_version"`
	DegradedReason         string                 `json:"degraded_reason,omitempty"`
}

type TaskCard struct {
	ID            string `json:"id"`
	Version       int    `json:"version"`
	RuntimeID     string `json:"runtime_id"`
	RuntimeName   string `json:"runtime_name"`
	Mode          string `json:"mode"`
	Title         string `json:"title"`
	Preview       string `json:"preview"`
	ResultSummary string `json:"result_summary"`
	Status        string `json:"status"`
	MemoryStatus  string `json:"memory_status,omitempty"`
	MemoryError   string `json:"memory_error_code,omitempty"`
	WorkPhase     string `json:"work_phase,omitempty"`
	WorkDeadline  string `json:"work_deadline,omitempty"`
	WorkHeartbeat string `json:"work_heartbeat,omitempty"`
	ModelActivity string `json:"model_activity_at,omitempty"`
	RuntimeStatus string `json:"runtime_status"`
	ProcessState  string `json:"process_state"`
	Attention     bool   `json:"attention"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	Redacted      bool   `json:"redacted"`
	Command       string `json:"command"`
}

type MessageView struct {
	ID           string `json:"id"`
	SentAt       string `json:"sent_at"`
	Body         string `json:"body"`
	SourceID     string `json:"source_id"`
	Availability string `json:"availability"`
	ExpiresAt    string `json:"expires_at,omitempty"`
}

type taskCursor struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

const runtimesCacheTTL = time.Second

func (s *Server) query(ctx context.Context, r *http.Request) (any, error) {
	if r.URL.Path == "/api/v1/runtimes" {
		return s.cachedRuntimes(ctx)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	switch r.URL.Path {
	case "/api/v1/memory-workspaces":
		return memoryWorkspaces(ctx, tx)
	case "/api/v1/memories":
		return memoryList(ctx, tx, r)
	case "/api/v1/meta":
		applied, err := core.ReadAppliedConfig(ctx, tx, 0)
		if err != nil {
			return nil, err
		}
		installed := s.installedFingerprint()
		serviceState, err := localservice.Read(s.opts.Home)
		if err != nil {
			return nil, err
		}
		return map[string]any{"service": serviceState, "home": s.opts.Home, "config_path": s.opts.ConfigPath, "version": s.opts.Version, "build": s.opts.Build, "installed_build": installed, "database_schema": core.SchemaVersion, "applied_version": applied.Version, "read_only": s.opts.AgentConfig == nil && s.opts.Restart == nil && s.opts.ResumeTask == nil, "agent_editing": s.opts.AgentConfig != nil, "service_restart": s.opts.Restart != nil, "task_resume": s.opts.ResumeTask != nil}, nil
	case "/api/v1/tasks":
		return s.tasks(ctx, tx, r)
	case "/api/v1/sources":
		sources, err := core.DataSourceList(ctx, tx)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, d := range sources {
			lease, e := core.ReadLease(ctx, tx, d.ChannelID)
			if e != nil {
				return nil, e
			}
			coverage, e := core.CoverageReport(ctx, tx, d.ChannelID)
			if e != nil {
				return nil, e
			}
			out = append(out, map[string]any{"source": d, "receiver_active": lease.Held, "coverage": coverage})
		}
		return out, nil
	case "/api/v1/agents":
		return s.agents(ctx, tx)
	case "/api/v1/agent-config":
		if r.Method == http.MethodGet && s.opts.AgentConfig != nil {
			return s.opts.AgentConfig.List(ctx, tx)
		}
	case "/api/v1/agent-config/preview", "/api/v1/agent-config/save":
		if r.Method == http.MethodPost && s.opts.AgentConfig != nil {
			var input json.RawMessage
			decoder := json.NewDecoder(r.Body)
			if err := decoder.Decode(&input); err != nil {
				return nil, core.Fail("invalid_input", "invalid Agent edit request")
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				return nil, core.Fail("invalid_input", "single JSON edit required")
			}
			if r.URL.Path == "/api/v1/agent-config/preview" {
				return s.opts.AgentConfig.Preview(ctx, tx, input)
			}
			return s.opts.AgentConfig.Save(ctx, tx, input)
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/memories/") {
		return memoryDetail(ctx, tx, r)
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/tasks/") {
		part := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
		logs := strings.HasSuffix(part, "/logs")
		if logs {
			part = strings.TrimSuffix(part, "/logs")
		}
		if part == "" || strings.Contains(part, "/") {
			return nil, core.Fail("not_found", "query not found")
		}
		task, err := core.ReadRuntimeTask(ctx, tx, part)
		if err != nil {
			return nil, err
		}
		if logs {
			return s.logs(task, r)
		}
		return s.detail(ctx, tx, task)
	}
	return nil, core.Fail("not_found", "query not found")
}

func (s *Server) runtimeView(status core.RuntimeStatus, now time.Time) RuntimeView {
	c := status.Runtime
	runners := observation.Read(s.opts.Home, c.ID, now)
	state := "unknown"
	if len(runners) == 1 {
		state = "heartbeat"
	} else if len(runners) > 1 {
		state = "multiple"
	} else if c.Status == "stopped" {
		state = "stopped_record"
	}
	return RuntimeView{Work: status.Work, ID: c.ID, Name: c.Name, Mode: c.ApplicationMode, Status: c.Status, ProcessState: state, Runners: runners, Tasks: status.Tasks, PendingMessages: status.PendingMessages, WaitingReceiptMessages: status.WaitingReceiptMessages, PendingActions: status.PendingActions, ConfigVersion: c.Version, DegradedReason: c.DegradedReason}
}

func (s *Server) runtimes(ctx context.Context, q core.Queryer) ([]RuntimeView, error) {
	statuses, err := core.RuntimeStatusList(ctx, q)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]RuntimeView, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, s.runtimeView(status, now))
	}
	return out, nil
}

// cachedRuntimes coalesces concurrent refreshes and keeps the read snapshot for
// at most one second. Browser storage remains disabled by Cache-Control.
func (s *Server) cachedRuntimes(ctx context.Context) ([]RuntimeView, error) {
	s.runtimesMu.Lock()
	defer s.runtimesMu.Unlock()
	if time.Now().Before(s.runtimesExpires) {
		return s.runtimesResult, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := s.runtimes(ctx, tx)
	if err != nil {
		return nil, err
	}
	s.runtimesResult = result
	s.runtimesExpires = time.Now().Add(runtimesCacheTTL)
	return result, nil
}

func textPreview(text string, n int) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > n {
		return string(runes[:n]) + "…"
	}
	return string(runes)
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func (s *Server) command(parts ...string) string {
	values := []string{"memgov", "--home", shellQuote(s.opts.Home)}
	if s.opts.ConfigPath != "" {
		values = append(values, "--config", shellQuote(s.opts.ConfigPath))
	}
	for _, p := range parts {
		values = append(values, shellQuote(p))
	}
	return strings.Join(values, " ")
}

// Read current availability and retention independently of physical cleanup.
func messageVisibility(ctx context.Context, q core.Queryer, id string, now time.Time) (MessageView, bool, error) {
	var m MessageView
	var days int
	var state string
	var sourceRedacted int
	err := q.QueryRowContext(ctx, `SELECT m.id,m.sent_at,m.availability,coalesce(so.source_id,''),
 CASE WHEN m.availability='available' THEN mr.body ELSE '' END,
	 coalesce(sa.state,'available'),coalesce((SELECT redacted FROM sources WHERE id=so.source_id),0),
 coalesce((SELECT min(d.retention_days) FROM data_sources d WHERE d.channel_id=m.channel_id AND EXISTS
 (SELECT 1 FROM channel_routes r JOIN json_each(d.route_ids) j ON r.id=j.value WHERE r.channel_id=m.channel_id AND r.conversation_id=m.conversation_id)),0)
 FROM messages m JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision
 LEFT JOIN source_origins so ON so.message_id=m.id AND so.revision=m.current_revision
	 LEFT JOIN source_availability sa ON sa.source_id=so.source_id WHERE m.id=?`, id).Scan(&m.ID, &m.SentAt, &m.Availability, &m.SourceID, &m.Body, &state, &sourceRedacted, &days)
	if err != nil {
		return m, false, err
	}
	visible := m.Availability == "available" && state == "available" && sourceRedacted == 0
	if days > 0 {
		sent, e := time.Parse(time.RFC3339Nano, m.SentAt)
		if e != nil {
			visible = false
		} else {
			expires := sent.AddDate(0, 0, days)
			m.ExpiresAt = expires.UTC().Format(time.RFC3339Nano)
			if !now.Before(expires) {
				visible = false
				m.Availability = "expired"
			}
		}
	}
	if !visible {
		m.Body = ""
		if m.Availability == "available" {
			m.Availability = state
			if sourceRedacted > 0 {
				m.Availability = "redacted"
			}
		}
	}
	return m, visible, nil
}

func (s *Server) card(ctx context.Context, q core.Queryer, id string) (TaskCard, error) {
	var out TaskCard
	err := q.QueryRowContext(ctx, `SELECT t.id,t.version,t.runtime_id,c.name,c.application_mode,t.title,t.status,c.status,t.created_at,t.updated_at FROM runtime_tasks t JOIN runtime_configs c ON c.id=t.runtime_id WHERE t.id=?`, id).Scan(&out.ID, &out.Version, &out.RuntimeID, &out.RuntimeName, &out.Mode, &out.Title, &out.Status, &out.RuntimeStatus, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return out, err
	}
	err = q.QueryRowContext(ctx, `SELECT memory_status,memory_error_code FROM runtime_tasks WHERE id=?`, id).Scan(&out.MemoryStatus, &out.MemoryError)
	if err != nil {
		return out, err
	}
	err = q.QueryRowContext(ctx, `SELECT phase,deadline_at,heartbeat_at,model_activity_at FROM runtime_work_leases WHERE task_id=? AND released=0 ORDER BY heartbeat_at DESC LIMIT 1`, id).Scan(&out.WorkPhase, &out.WorkDeadline, &out.WorkHeartbeat, &out.ModelActivity)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	runners := observation.Read(s.opts.Home, out.RuntimeID, time.Now())
	out.ProcessState = "unknown"
	if len(runners) == 1 {
		out.ProcessState = "heartbeat"
	} else if len(runners) > 1 {
		out.ProcessState = "multiple"
	} else if out.RuntimeStatus == "stopped" {
		out.ProcessState = "stopped_record"
	}
	out.Attention = out.Status == "failed" || out.Status == "stale" || out.Status == "clarification" || out.Status == "blocked" || out.Status == "action_failed" || out.Status == "action_unknown" || out.Status == "awaiting_confirmation" || (out.Status == "running" && (out.RuntimeStatus != "running" || out.ProcessState != "heartbeat"))
	out.Attention = out.Attention || out.MemoryStatus == "failed" || out.MemoryStatus == "rejected"
	rows, err := q.QueryContext(ctx, "SELECT message_id FROM runtime_task_messages WHERE task_id=? ORDER BY rowid", id)
	if err != nil {
		return out, err
	}
	ids := []string{}
	for rows.Next() {
		var mid string
		if err = rows.Scan(&mid); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, mid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(ids) == 0 {
		out.Redacted = true
	}
	for _, mid := range ids {
		m, visible, e := messageVisibility(ctx, q, mid, time.Now())
		if e != nil {
			return out, e
		}
		if !visible {
			out.Redacted = true
		}
		if out.Preview == "" && visible {
			out.Preview = textPreview(m.Body, 120)
		}
		if m.ExpiresAt != "" && (out.ExpiresAt == "" || m.ExpiresAt < out.ExpiresAt) {
			out.ExpiresAt = m.ExpiresAt
		}
	}
	if out.Redacted {
		out.Preview = "关联原文已到期或不可用"
		out.Title = "原文不可用的任务"
	}
	if out.Title == "本人会话" && out.Preview != "" && !out.Redacted {
		out.Title = textPreview(out.Preview, 44)
	}
	current, err := core.RuntimeTaskOutputCurrent(ctx, q, id)
	if err != nil {
		return out, err
	}
	if current && !out.Redacted && out.Status != "stale" {
		err = q.QueryRowContext(ctx, `SELECT CASE WHEN coalesce((SELECT task_version FROM runtime_attempts WHERE task_id=t.id ORDER BY started_at DESC,id DESC LIMIT 1),t.version)=t.version THEN result_summary ELSE '' END FROM runtime_tasks t WHERE id=?`, id).Scan(&out.ResultSummary)
		if err != nil {
			return out, err
		}
		out.ResultSummary = textPreview(out.ResultSummary, 120)
	}
	out.Command = s.command("runtime", "task", "show", out.ID)
	return out, nil
}

func (s *Server) tasks(ctx context.Context, q core.Queryer, r *http.Request) (any, error) {
	limit := 30
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 100 {
			return nil, core.Fail("invalid_input", "limit must be 1..100")
		}
		limit = n
	}
	query := `SELECT t.id,t.created_at FROM runtime_tasks t JOIN runtime_configs c ON c.id=t.runtime_id WHERE 1=1`
	args := []any{}
	if runtime := r.URL.Query().Get("runtime"); runtime != "" {
		query += " AND (c.id=? OR c.name=?)"
		args = append(args, runtime, runtime)
	}
	status := r.URL.Query().Get("status")
	if status == "" || status == "active" {
		query += " AND t.status NOT IN ('completed','cancelled')"
	} else if status != "all" {
		valid := map[string]bool{"pending": true, "running": true, "completed": true, "failed": true, "stale": true, "cancelled": true, "blocked": true, "awaiting_confirmation": true, "clarification": true, "action_failed": true, "action_unknown": true}
		if !valid[status] {
			return nil, core.Fail("invalid_input", "unknown task status")
		}
		query += " AND t.status=?"
		args = append(args, status)
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if len(raw) > 1024 {
			return nil, core.Fail("invalid_input", "invalid cursor")
		}
		body, e := base64.RawURLEncoding.DecodeString(raw)
		var c taskCursor
		if e != nil || json.Unmarshal(body, &c) != nil || c.ID == "" || c.CreatedAt == "" {
			return nil, core.Fail("invalid_input", "invalid cursor")
		}
		query += " AND (t.created_at<? OR (t.created_at=? AND t.id<?))"
		args = append(args, c.CreatedAt, c.CreatedAt, c.ID)
	}
	query += " ORDER BY t.created_at DESC,t.id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	cursors := []taskCursor{}
	for rows.Next() {
		var c taskCursor
		if err = rows.Scan(&c.ID, &c.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		cursors = append(cursors, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	next := ""
	if len(cursors) > limit {
		cursors = cursors[:limit]
		b, _ := json.Marshal(cursors[len(cursors)-1])
		next = base64.RawURLEncoding.EncodeToString(b)
	}
	cards := []TaskCard{}
	for _, c := range cursors {
		card, e := s.card(ctx, q, c.ID)
		if e != nil {
			return nil, e
		}
		cards = append(cards, card)
	}
	return map[string]any{"tasks": cards, "next_cursor": next}, nil
}

func (s *Server) detail(ctx context.Context, q core.Queryer, t core.RuntimeTask) (any, error) {
	card, err := s.card(ctx, q, t.ID)
	if err != nil {
		return nil, err
	}
	outputCurrent, err := core.RuntimeTaskOutputCurrent(ctx, q, t.ID)
	if err != nil {
		return nil, err
	}
	messages := []MessageView{}
	for _, raw := range t.Messages {
		m, _, e := messageVisibility(ctx, q, raw.ID, time.Now())
		if e != nil {
			return nil, e
		}
		messages = append(messages, m)
	}
	attempts := []any{}
	for _, a := range t.Attempts {
		summary := a.Summary
		artifacts := a.Artifacts
		visible := outputCurrent && !card.Redacted && a.TaskVersion == t.Version && a.Status != "stale"
		if !visible {
			summary = ""
			artifacts = nil
		}
		attempts = append(attempts, map[string]any{"id": a.ID, "status": a.Status, "model": a.Model, "started_at": a.StartedAt, "finished_at": a.FinishedAt, "summary": summary, "artifacts": artifacts, "output_visible": visible, "task_version": a.TaskVersion, "error_code": a.ErrorCode, "tools": a.ToolKinds, "applied_version": a.AppliedConfigVersion})
	}
	actions := []any{}
	for _, a := range t.Actions {
		item := map[string]any{"id": a.ID, "kind": a.Kind, "status": a.Status}
		if outputCurrent && !card.Redacted && a.TaskVersion == t.Version {
			item["target"], item["payload"] = a.Target, a.Payload
			if a.Kind == core.RuntimeDestructiveAction && a.Status == "pending" {
				item["confirmation_token"] = core.ConfirmationToken(a)
			}
		}
		actions = append(actions, item)
	}
	result := t.Result
	if card.Redacted || !outputCurrent || t.Status == "stale" || (len(t.Attempts) > 0 && t.Attempts[len(t.Attempts)-1].TaskVersion != t.Version) {
		result = ""
	}
	deliveryRows, err := q.QueryContext(ctx, "SELECT id,state,created_at,updated_at,reason,transport FROM outbox WHERE job_id=? ORDER BY created_at", t.ID)
	if err != nil {
		return nil, err
	}
	deliveries := []any{}
	for deliveryRows.Next() {
		var id, status, created, updated, purpose, transport string
		if err = deliveryRows.Scan(&id, &status, &created, &updated, &purpose, &transport); err != nil {
			deliveryRows.Close()
			return nil, err
		}
		deliveries = append(deliveries, map[string]string{"id": id, "state": status, "created_at": created, "updated_at": updated, "purpose": purpose, "transport": transport})
	}
	err = deliveryRows.Err()
	deliveryRows.Close()
	if err != nil {
		return nil, err
	}
	canResume, resumeReason, resumeMode := false, "", ""
	communications, err := core.RuntimeMessageActions(ctx, q, t.ID)
	if err != nil {
		return nil, err
	}
	if s.opts.ResumeTask != nil && t.Status == "failed" && !card.Redacted {
		if err := core.RuntimeTaskResumable(ctx, q, t); err == nil {
			canResume, resumeMode = true, "replay"
			if t.Attempts[len(t.Attempts)-1].AgentSession.ID != "" {
				resumeMode = "native"
			}
		} else {
			resumeReason = err.Error()
		}
	}
	return map[string]any{"task": card, "messages": messages, "attempts": attempts, "result": result, "error_code": t.ErrorCode, "actions": actions, "deliveries": deliveries, "communications": communications, "can_view_output": outputCurrent && !card.Redacted, "can_resume": canResume, "resume_mode": resumeMode, "resume_reason": resumeReason, "resume_command": s.command("runtime", "task", "resume", t.ID), "logs_command": s.command("runtime", "logs", "show", card.RuntimeName, "--task-id", t.ID)}, nil
}

func (s *Server) agents(ctx context.Context, q core.Queryer) ([]any, error) {
	configs, err := core.RuntimeList(ctx, q)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, c := range configs {
		routes := c.RouteIDs
		if len(routes) == 0 {
			routes = []string{""}
		}
		for _, routeID := range routes {
			conversationID := ""
			if routeID != "" {
				route, err := core.ReadRoute(ctx, q, routeID)
				if err != nil {
					return nil, err
				}
				conversationID = route.ConversationID
			}
			policy := core.RuntimeAgentPolicy{Preset: c.AgentPreset, ClaudeProfile: c.ClaudeProfile, ExecutionModel: c.ExecutionModel, MemoryScope: c.MemoryScope, Capabilities: c.AgentCapabilities, BashEnabled: c.AgentBash, ExternalActions: c.ExternalActions, Skills: core.RuntimeSkillPolicy{Inherit: "none"}}
			policyError := ""
			if routeID != "" {
				p, e := core.ResolveRuntimeTaskAgent(ctx, q, c, core.RuntimeTask{RuntimeID: c.ID, RouteID: routeID})
				if e == nil {
					policy = p
				} else {
					policyError = core.ErrorCode(e)
				}
			}
			skills := policy.Skills.Resolved
			skillError := ""
			if s.opts.ResolveSkills != nil {
				resolved, e := s.opts.ResolveSkills(policy.Skills)
				if e != nil {
					skillError = e.Error()
				} else {
					skills = resolved
				}
			}
			names := []any{map[string]string{"name": "memgov-memory", "origin": "runtime_managed"}}
			for _, skill := range skills {
				names = append(names, map[string]string{"name": skill.Name, "digest": skill.Digest, "path": skill.Path})
			}
			out = append(out, map[string]any{"runtime": c.Name, "conversation_id": conversationID, "agent": policy.Agent, "preset": policy.Preset, "model": policy.ExecutionModel, "profile": policy.ClaudeProfile, "memory_scope": policy.MemoryScope, "capabilities": policy.Capabilities, "bash": policy.BashEnabled, "external_actions": policy.ExternalActions, "directories": policy.Directories, "inherit": policy.Skills.Inherit, "skills": names, "policy_error": policyError, "skill_error": skillError})
		}
	}
	return out, nil
}

func (s *Server) installedFingerprint() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	info, err := os.Stat(exe)
	if err != nil {
		return "unknown"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.installedBuild == "" || !info.ModTime().Equal(s.installedMod) || info.Size() != s.installedSize {
		s.installedBuild = observation.InstalledBuild()
		s.installedMod = info.ModTime()
		s.installedSize = info.Size()
	}
	return s.installedBuild
}

// Scan a bounded tail; never expose arbitrary log summaries or tool payloads.
func (s *Server) logs(t core.RuntimeTask, r *http.Request) (any, error) {
	files, err := runlog.Files(s.opts.Home, t.RuntimeID)
	if err != nil {
		return nil, core.Fail("unavailable", "runtime logs unavailable")
	}
	since := r.URL.Query().Get("since")
	if since != "" {
		if _, e := time.Parse(time.RFC3339Nano, since); e != nil {
			return nil, core.Fail("invalid_input", "since must be RFC3339")
		}
	}
	events := []runlog.Event{}
	budget := int64(8 << 20)
	truncated := false
	for i := len(files) - 1; i >= 0 && budget > 0; i-- {
		file := files[i]
		f, e := os.Open(filepath.Clean(file.Path))
		if e != nil {
			continue
		}
		info, e := f.Stat()
		if e != nil {
			f.Close()
			continue
		}
		size := info.Size()
		read := size
		if read > 2<<20 {
			read = 2 << 20
			truncated = true
		}
		if read > budget {
			read = budget
			truncated = true
		}
		budget -= read
		body := make([]byte, read)
		_, _ = f.ReadAt(body, size-read)
		f.Close()
		lines := strings.Split(string(body), "\n")
		if read < size && len(lines) > 0 {
			lines = lines[1:]
		}
		for _, line := range lines {
			var event runlog.Event
			if json.Unmarshal([]byte(line), &event) != nil || event.TaskID != t.ID || event.RuntimeID != t.RuntimeID || (since != "" && event.Timestamp <= since) {
				continue
			}
			// Only structural diagnostics: even a retained log can quote expired text.
			event.Summary = ""
			events = append(events, event)
		}
	}
	if budget <= 0 {
		truncated = true
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Timestamp < events[j].Timestamp })
	if len(events) > 200 {
		events = events[len(events)-200:]
		truncated = true
	}
	return map[string]any{"events": events, "truncated": truncated}, nil
}
