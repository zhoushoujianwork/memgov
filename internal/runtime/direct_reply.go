package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

// Direct receipts preserve each owner message without semantic classification.
func directTurnReceipts(cfg core.RuntimeConfig, batch core.RuntimeBatch) core.RuntimeAnalysis {
	analysis := core.RuntimeAnalysis{}
	if !directRouteEligible(cfg, batch) {
		return analysis
	}
	for _, m := range batch.Messages {
		if m.Sender != cfg.OwnerPrincipalID || m.SelfAuthored || !m.Addressed || !newerThanBootstrap(m.SentAt, cfg.BootstrapAt) || strings.TrimSpace(m.Body) == "" {
			continue
		}
		analysis.Decisions = append(analysis.Decisions, core.RuntimeDecision{Kind: "task", CanonicalKey: "direct:" + m.ID, Title: "本人会话", Instructions: "原始用户消息见关联消息。", MessageIDs: []string{m.ID}})
	}
	return analysis
}

// Admission checks transport identity and route only, never the user's intent.
func directRouteEligible(cfg core.RuntimeConfig, batch core.RuntimeBatch) bool {
	return cfg.ApplicationMode == "direct" && batch.Mode == "direct" && containsRoute(cfg.RouteIDs, batch.RouteID) && cfg.OwnerPrincipalID != ""
}

func (s *Service) queueDirectTurn(ctx context.Context, cfg core.RuntimeConfig, batch core.RuntimeBatch) {
	start := s.now()
	analysis := directTurnReceipts(cfg, batch)
	actionable := map[string]bool{}
	for _, d := range analysis.Decisions {
		if d.Kind == "task" {
			for _, id := range d.MessageIDs {
				actionable[id] = true
			}
		}
	}
	var tasks []core.RuntimeTask
	err := s.mutate(ctx, "global", "runtime.direct.complete", func(tx *core.Tx) (any, error) {
		current, e := core.ReadRuntime(ctx, tx.Conn, cfg.ID)
		if e != nil {
			return nil, e
		}
		if current.Status != "running" || current.OwnerPrincipalID != cfg.OwnerPrincipalID || !directRouteEligible(current, batch) {
			return nil, core.Fail("conflict", "owner private route or request changed before direct intake")
		}
		route, e := core.RuntimeOwnerDirectProcessingRoute(ctx, tx.Conn, current, batch.RouteID)
		if e != nil {
			return nil, e
		}
		for _, message := range batch.Messages {
			if !actionable[message.ID] {
				continue
			}
			verified, verifyErr := core.RuntimeOwnerDirectSenderCurrent(ctx, tx.Conn, current, message.ID)
			if verifyErr != nil {
				return nil, verifyErr
			}
			if message.ConversationID != route.ConversationID || !verified {
				return nil, core.Fail("conflict", "owner private sender or conversation changed before direct intake")
			}
		}
		if _, e = tx.Conn.ExecContext(ctx, "UPDATE runtime_batches SET model='direct_agent' WHERE id=? AND runtime_id=? AND status='analyzing'", batch.ID, current.ID); e != nil {
			return nil, e
		}
		tasks, e = tx.CompleteRuntimeBatch(ctx, batch, analysis)
		if e != nil {
			return nil, e
		}
		for _, item := range tasks {
			t, err := core.ReadRuntimeTask(ctx, tx.Conn, item.ID)
			if err != nil {
				return nil, err
			}
			if _, err = tx.BindRuntimeDirectTurn(ctx, current, t); err != nil {
				return nil, err
			}
		}
		return tasks, nil
	})
	if err != nil {
		_ = s.mutate(ctx, "global", "runtime.direct.fail", func(tx *core.Tx) (any, error) { return nil, tx.FailRuntimeBatch(ctx, batch.ID, core.ErrorCode(err)) })
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, BatchID: batch.ID, Level: "warn", Component: "intake", Event: "turn_queue_failed", ErrorCode: core.ErrorCode(err), DurationMS: s.now().Sub(start).Milliseconds(), Summary: "本人会话接入未通过状态校验"})
		return
	}
	for _, task := range tasks {
		s.acknowledge(ctx, cfg, task.ID)
	}
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, BatchID: batch.ID, Level: "info", Component: "intake", Event: "turn_queued", Status: "completed", DurationMS: s.now().Sub(start).Milliseconds(), Summary: fmt.Sprintf("已接收 %d 条本人会话消息", len(tasks))})
}

func containsRoute(ids []string, value string) bool {
	for _, id := range ids {
		if id == value {
			return true
		}
	}
	return false
}

func newerThanBootstrap(sentAt, bootstrapAt string) bool {
	sent, err := time.Parse(time.RFC3339Nano, sentAt)
	if err != nil {
		return false
	}
	if bootstrapAt == "" {
		return true
	}
	bootstrap, err := time.Parse(time.RFC3339Nano, bootstrapAt)
	return err == nil && sent.After(bootstrap)
}
