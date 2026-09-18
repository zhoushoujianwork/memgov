package runtime

import (
	"context"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

// Resolve provider message identifiers before intake so an Agent's own message
// is retained as background instead of becoming a new observation task. This
// queries an existing send receipt only; it never dispatches or retries a send.
func (s *Service) reconcileOwnerMessages(ctx context.Context, cfg core.RuntimeConfig) {
	if cfg.ApplicationMode != "proactive" {
		return
	}
	reader, ok := s.Adapter.(channel.OwnerMessageStatusReader)
	if !ok {
		return
	}
	actions, err := core.UnresolvedRuntimeMessageActions(ctx, s.Store.DB, cfg.ChannelID)
	if err != nil || len(actions) == 0 {
		return
	}
	c, err := core.ReadChannel(ctx, s.Store.DB, cfg.ChannelID)
	if err != nil {
		return
	}
	queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for _, action := range actions {
		if queryCtx.Err() != nil {
			return
		}
		// Reconciliation spends no sending permission. A metadata/configuration
		// version change must not strand a valid receipt, but the authenticated
		// account namespace must remain exactly the account that sent it.
		if (c.Status != "configured" && c.Status != "active") || c.Tenant != action.OwnerTenant || c.Identity.Profile != action.OwnerProfile || c.Identity.ExpectedUserID != action.OwnerUserID {
			continue
		}
		result, err := reader.ReadOwnerMessageStatus(queryCtx, channel.ConfigFor(c), action.Receipt)
		if err != nil || result.Receipt == "" {
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: action.TaskID, Level: "warn", Component: "communication", Event: "receipt_unverified", Status: "unknown", Summary: "本人消息回执尚未核验，关联回流保持等待；未重发"})
			continue
		}
		err = s.mutate(ctx, "global", "runtime.message.reconcile", func(tx *core.Tx) (any, error) {
			return tx.ReconcileRuntimeMessageResult(ctx, action.ID, result.State, result.Receipt, result.Detail)
		})
		if err != nil {
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: action.TaskID, Level: "warn", Component: "communication", Event: "receipt_record_failed", ErrorCode: core.ErrorCode(err), Summary: "本人消息回执状态暂未核对完成，未重发"})
		}
	}
}
