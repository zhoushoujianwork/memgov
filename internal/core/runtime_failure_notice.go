package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// RuntimeFailureNoticeTaskIDs returns failed interactive tasks that still need
// a textual closeout. Direct tasks always receive one. Group tasks receive one
// when processing produced a result, so a failure reaction never hides a useful
// conclusion that can still be reported safely.
func RuntimeFailureNoticeTaskIDs(ctx context.Context, q Queryer, runtimeID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT t.id FROM runtime_tasks t
JOIN runtime_configs c ON c.id=t.runtime_id
WHERE t.runtime_id=? AND t.status IN ('failed','action_failed','action_unknown')
AND c.application_mode IN ('direct','group_mention')
AND (c.application_mode='direct' OR trim(t.result)<>'' OR trim(t.result_summary)<>'' OR EXISTS(
 SELECT 1 FROM runtime_action_attempts aa WHERE aa.task_id=t.id AND (trim(aa.result)<>'' OR trim(aa.summary)<>'')
))
AND EXISTS(SELECT 1 FROM outbox receipt WHERE receipt.job_id=t.id AND receipt.reason=? AND receipt.state='accepted')
AND NOT EXISTS(SELECT 1 FROM outbox notice WHERE notice.job_id=t.id AND notice.reason=? AND notice.state<>'ready')
ORDER BY t.updated_at,t.id`, runtimeID, RuntimeReceiptPurpose, RuntimeFailureNoticePurpose)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RuntimeDirectFailureNoticeTaskIDs remains as a compatibility alias for
// callers compiled against the former direct-only name.
func RuntimeDirectFailureNoticeTaskIDs(ctx context.Context, q Queryer, runtimeID string) ([]string, error) {
	return RuntimeFailureNoticeTaskIDs(ctx, q, runtimeID)
}

func (tx *Tx) PrepareTaskFailureNotice(ctx context.Context, taskID string) (OutboxView, error) {
	var out OutboxView
	task, err := ReadRuntimeTask(ctx, tx.Conn, taskID)
	if err != nil {
		return out, err
	}
	if !contains([]string{"failed", "action_failed", "action_unknown"}, task.Status) {
		return out, Fail("conflict", "task has not failed")
	}
	current, err := runtimeTaskMessagesCurrent(ctx, tx.Conn, taskID)
	if err != nil {
		return out, err
	}
	if !current {
		return out, Fail("conflict", "task source messages changed before failure notice")
	}
	config, err := ReadRuntime(ctx, tx.Conn, task.RuntimeID)
	if err != nil {
		return out, err
	}
	if config.Status != "running" || !contains([]string{"direct", "group_mention"}, config.ApplicationMode) || RuntimeCompletionPolicy(config) == "record_only" {
		return out, Fail("denied", "only an active interactive runtime may publish a failure notice")
	}
	if config.ApplicationMode == "group_mention" && !runtimeTaskHasReportableResult(task) {
		return out, Fail("conflict", "group task has no result to report")
	}
	admitted, err := runtimeTaskTriggerAdmitted(ctx, tx.Conn, config, task)
	if err != nil {
		return out, err
	}
	if !admitted {
		return out, Fail("conflict", "task trigger route left the runtime processing scope")
	}
	if config.ApplicationMode == "direct" {
		if _, err = RuntimeOwnerDirectProcessingRoute(ctx, tx.Conn, config, task.RouteID); err != nil {
			return out, err
		}
		for _, message := range task.Messages {
			verified, verifyErr := RuntimeOwnerDirectSenderCurrent(ctx, tx.Conn, config, message.ID)
			if verifyErr != nil {
				return out, verifyErr
			}
			if !verified {
				return out, Fail("denied", "private sender is no longer the verified owner")
			}
		}
	}
	routeID := config.DeliveryRouteID
	transport := "bot_dm"
	if config.ApplicationMode == "group_mention" {
		routeID = task.RouteID
		transport = "bot_group"
	}
	route, err := ReadRoute(ctx, tx.Conn, routeID)
	if err != nil {
		return out, err
	}
	if config.ApplicationMode == "direct" {
		if route.ChannelID != config.ChannelID || route.Status != "active" || route.SendPolicy != "dispatch_only" || route.ConversationType != "direct" || route.ConversationID != config.OwnerIDValue {
			return out, Fail("denied", "runtime delivery route no longer permits owner failure notice")
		}
	} else if route.ChannelID != config.ChannelID || route.Status != "active" || route.SendPolicy != "reply_to_trigger" || route.ConversationType != "group" || route.Mode != "assistant" || !contains(route.Triggers, "mention") {
		return out, Fail("denied", "group Agent route no longer permits a failure result reply")
	}
	content := formatRuntimeDelivery(task, config, runtimeFailureNotice(task))
	format := "markdown"
	var replyTo string
	if config.ApplicationMode == "group_mention" {
		for i := len(task.Messages) - 1; i >= 0; i-- {
			if task.Messages[i].ProviderMessageID != "" {
				replyTo = task.Messages[i].ProviderMessageID
				break
			}
		}
		if replyTo == "" {
			return out, Fail("denied", "group failure notice requires the original message")
		}
		card := RuntimeCard{Text: content, TaskVersion: task.Version}
		card.Mentions, err = runtimeCardMentions(ctx, tx.Conn, config, task, false)
		if err != nil {
			return out, err
		}
		card.Text = formatRuntimeMentionsAtEnd(card.Text, card.Mentions)
		content = JSON(card)
		format = "group_markdown"
	}
	digest := Digest(map[string]any{"task": task.ID, "version": task.Version, "result": content})
	err = tx.Conn.QueryRowContext(ctx, "SELECT id,channel_id,route_id,conversation_id,transport,content,format,state,input_digest FROM outbox WHERE route_id=? AND input_digest=?", route.ID, digest).
		Scan(&out.ID, &out.ChannelID, &out.RouteID, &out.ConversationID, &out.Transport, &out.Content, &out.Format, &out.State, &out.InputDigest)
	if err == nil {
		out.ReplyTo = replyTo
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out = OutboxView{ID: NewID(), ChannelID: config.ChannelID, RouteID: route.ID, ConversationID: route.ConversationID, Transport: transport, Content: content, Format: format, State: "ready", InputDigest: digest, ReplyTo: replyTo}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,sender_identity,transport,content,format,input_digest,send_policy,state,reason,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", out.ID, out.ChannelID, out.RouteID, route.Version, task.ID, out.ConversationID, route.AudienceKey, "bot", out.Transport, out.Content, out.Format, out.InputDigest, route.SendPolicy, out.State, RuntimeFailureNoticePurpose, Now(), Now())
	return out, err
}

func runtimeTaskHasReportableResult(task RuntimeTask) bool {
	if strings.TrimSpace(task.Result) != "" || strings.TrimSpace(task.ResultSummary) != "" {
		return true
	}
	for _, action := range task.Actions {
		for _, attempt := range action.Attempts {
			if strings.TrimSpace(attempt.Result) != "" || strings.TrimSpace(attempt.Summary) != "" {
				return true
			}
		}
	}
	return false
}

func runtimeFailureNotice(task RuntimeTask) string {
	result := strings.TrimSpace(task.Result)
	if result == "" {
		result = strings.TrimSpace(task.ResultSummary)
	}
	for i := len(task.Actions) - 1; i >= 0; i-- {
		for j := len(task.Actions[i].Attempts) - 1; j >= 0; j-- {
			actionResult := strings.TrimSpace(task.Actions[i].Attempts[j].Result)
			if actionResult == "" {
				actionResult = strings.TrimSpace(task.Actions[i].Attempts[j].Summary)
			}
			if actionResult != "" && !strings.Contains(result, actionResult) {
				if result != "" {
					result += "\n\n"
				}
				result += actionResult
			}
		}
	}
	details := runtimeFailureDetails(task)
	if result == "" {
		base := ""
		switch task.Status {
		case "action_unknown":
			base = "服务恢复后发现刚才的外部操作结果无法确认，任务已标记为结果未知。请先检查目标系统，不要直接重试。"
		case "action_failed":
			base = "刚才的外部操作未完成，任务已标记为失败。请检查后再决定是否重试。"
		}
		if base == "" {
			if task.ErrorCode == "runtime_restarted" {
				base = "刚才服务中断，任务未能完成。服务现已恢复，这个任务已标记为失败；请重新发送原请求。"
			} else {
				base = "这次处理未能完成，任务已标记为失败。请重新发送原请求；系统不会自动重试，以避免重复执行。"
			}
		}
		return details + "\n\n" + base
	}
	status := ""
	switch task.Status {
	case "action_unknown":
		status = "后续外部操作的最终状态无法确认。请先检查目标系统，不要直接重试。"
	case "action_failed":
		status = "后续外部操作未完成。以上是本次已经得到的结果，请检查后再决定是否重试。"
	default:
		if task.ErrorCode == "runtime_restarted" {
			status = "刚才服务中断，处理未完整完成。服务现已恢复；以上是本次中断前已经得到的结果。"
		} else {
			status = "这次处理未完整完成。以上是本次已经得到的结果；系统不会自动重试，以避免重复执行。"
		}
	}
	return result + "\n\n" + details + "\n\n" + status
}

// runtimeFailureDetails gives the owner a useful, non-sensitive diagnosis.
// Error messages from an Agent, provider SDK, or subprocess are deliberately
// not persisted in RuntimeTask, because they can contain credentials or
// request payloads. The stable error code and this safe category are enough to
// explain what happened in the conversation and to correlate it with logs.
func runtimeFailureDetails(task RuntimeTask) string {
	code := safeRuntimeErrorCode(task.ErrorCode)
	reason := map[string]string{
		"unavailable":       "Agent、模型或本地运行进程当前不可用，可能是连接失败、连接被拒绝、超时或进程异常退出。",
		"timeout":           "处理超过允许的时间上限。",
		"deadline_exceeded": "处理超过允许的时间上限。",
		"context_deadline":  "处理超过允许的时间上限。",
		"denied":            "请求被当前运行策略或权限拒绝。",
		"invalid_input":     "请求或 Agent 返回的数据格式未通过校验。",
		"conflict":          "任务状态在处理期间发生变化，系统为避免重复执行而停止。",
		"runtime_restarted": "服务在处理期间重启，未能完成本次任务。",
		"internal":          "运行时发生内部错误。",
		"cancelled":         "任务被取消，未能完成。",
		"action_failed":     "确认后的外部操作执行失败。",
		"action_unknown":    "确认后的外部操作结果无法确认。",
	}[code]
	if reason == "" {
		reason = "运行时返回了未分类错误，请提供此错误代码以便排查。"
	}
	return fmt.Sprintf("错误代码：`%s`\n错误原因：%s", code, reason)
}

// safeRuntimeErrorCode allows only the compact code vocabulary used in task
// state. Anything else becomes internal so arbitrary provider text never
// reaches an owner chat.
func safeRuntimeErrorCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" || len(code) > 64 {
		return "internal"
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return "internal"
		}
	}
	if _, ok := map[string]struct{}{
		"access_token": {}, "analysis_timeout": {}, "cancelled": {}, "conflict": {},
		"context_deadline": {}, "deadline_exceeded": {}, "denied": {},
		"evidence_unavailable": {}, "forbidden": {}, "history_cursor_stalled": {},
		"internal": {}, "invalid_input": {}, "not_found": {},
		"owner_rejected": {}, "process_cleanup_failed": {}, "rate_limited": {},
		"resource_busy": {}, "runtime_restarted": {}, "timeout": {}, "unavailable": {},
		"action_failed": {}, "action_unknown": {},
	}[code]; !ok {
		return "internal"
	}
	return code
}
