package runtime

import (
	"context"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

func (s *Service) reconcileFailureNotices(ctx context.Context, cfg core.RuntimeConfig) {
	if cfg.ApplicationMode != "direct" && cfg.ApplicationMode != "group_mention" {
		return
	}
	taskIDs, err := core.RuntimeFailureNoticeTaskIDs(ctx, s.Store.DB, cfg.ID)
	if err != nil {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "delivery", Event: "failure_notices_scan_failed", ErrorCode: core.ErrorCode(err), Summary: "异常任务结果反馈对账未完成，后续启动将再次核对"})
		return
	}
	for _, taskID := range taskIDs {
		s.deliverFailureNotice(ctx, cfg, taskID)
	}
}

func (s *Service) deliverFailureNotice(ctx context.Context, cfg core.RuntimeConfig, taskID string) bool {
	if cfg.ApplicationMode != "direct" && cfg.ApplicationMode != "group_mention" {
		return false
	}
	c, err := core.ReadChannel(ctx, s.Store.DB, cfg.ChannelID)
	if err != nil || !c.Capabilities.Verified["send"] {
		return false
	}
	var out core.OutboxView
	err = s.mutate(ctx, "global", "runtime.failure-notice.prepare", func(tx *core.Tx) (any, error) {
		var prepareErr error
		out, prepareErr = tx.PrepareTaskFailureNotice(ctx, taskID)
		return out, prepareErr
	})
	if err != nil || out.State != "ready" {
		return false
	}
	var attempt int
	err = s.mutate(ctx, "global", "runtime.failure-notice.begin", func(tx *core.Tx) (any, error) {
		var beginErr error
		attempt, beginErr = tx.BeginDelivery(ctx, out.ID)
		return attempt, beginErr
	})
	if err != nil {
		return false
	}
	target := out.ConversationID
	if out.Transport == "bot_dm" {
		target = cfg.OwnerIDValue
	}
	result, sendErr := s.Adapter.Send(ctx, channel.ConfigFor(c), channel.SendRequest{ConversationID: target, Transport: out.Transport, Content: out.Content, Format: out.Format, ReplyTo: out.ReplyTo, IdempotencyKey: out.ID})
	state := result.State
	if sendErr != nil {
		state = "unknown"
		if core.ErrorCode(sendErr) == "denied" {
			state = "failed"
		}
	}
	if !containsState(state) {
		state = "unknown"
	}
	finishErr := s.mutate(ctx, "global", "runtime.failure-notice.finish", func(tx *core.Tx) (any, error) {
		return nil, tx.FinishDelivery(ctx, out.ID, attempt, state, result.Receipt)
	})
	if finishErr != nil {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: taskID, Level: "error", Component: "delivery", Event: "record_failed", ErrorCode: core.ErrorCode(finishErr), Summary: "异常任务结果反馈状态记录失败"})
		return false
	}
	level, summary := "info", "异常任务已有结果已反馈"
	if state != "accepted" {
		level, summary = "warn", "异常任务结果反馈未确认成功"
	}
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: taskID, Level: level, Component: "delivery", Event: "failure_notified", Status: state, ErrorCode: func() string {
		if sendErr != nil {
			return core.ErrorCode(sendErr)
		}
		return ""
	}(), Summary: summary})
	return state == "accepted"
}
