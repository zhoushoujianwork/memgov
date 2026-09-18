package channel

import (
	"context"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// HistoryImportWorker performs one bounded background step. The data-source
// service invokes Step in a serialized history loop alongside live receive,
// after a bounded realtime reconciliation pass. A durable claim additionally fences competing workers.
type HistoryImportWorker struct {
	Collector Collector
	Request   core.Request
}

func (w HistoryImportWorker) Step(ctx context.Context, sourceID string) (bool, error) {
	req := w.Request
	req.Key = ""
	req.Command = "data-source.history.step"
	var claim core.HistoryImport
	_, err := w.Collector.Store.Mutate(ctx, req, func(tx *core.Tx) (any, error) {
		if e := tx.EnsureHistoryImports(ctx, sourceID, time.Now().UTC()); e != nil {
			return nil, e
		}
		var e error
		claim, e = tx.ClaimHistoryImport(ctx, sourceID, time.Now().UTC())
		// Commit newly scheduled tasks even when another worker owns the next step.
		if core.ErrorCode(e) == "not_found" {
			return nil, nil
		}
		return claim, e
	})
	if err != nil {
		return false, err
	}
	if claim.ID == "" {
		return false, nil
	}
	_, err = w.Collector.PullImport(ctx, req, claim)
	return true, err
}

// PullImport is the checkpointed variant of Pull: it uses the same adapter
// window reader and Intake/Coverage stores, with one atomic bounded write.
func (c Collector) PullImport(ctx context.Context, req core.Request, claim core.HistoryImport) (core.HistoryImport, error) {
	stored, err := core.ReadChannel(ctx, c.Store.DB, claim.ChannelID)
	if err != nil {
		return claim, c.failImport(claim, req, err)
	}
	if !stored.Capabilities.Verified["history"] {
		err = core.Fail("unavailable", "history capability is not verified")
		return claim, c.failImport(claim, req, err)
	}
	route, err := core.ReadRoute(ctx, c.Store.DB, claim.RouteID)
	if err != nil {
		return claim, c.failImport(claim, req, err)
	}
	start, err := time.Parse(time.RFC3339, claim.StartAt)
	if err != nil {
		return claim, c.failImport(claim, req, err)
	}
	end, err := time.Parse(time.RFC3339, claim.EndAt)
	if err != nil {
		return claim, c.failImport(claim, req, err)
	}
	stepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	window, err := c.Adapter.ReadWindow(stepCtx, ConfigFor(stored), Window{ConversationID: route.ConversationID, Start: start, End: end, Cursor: claim.Cursor, PageLimit: 5, MaxItems: 100})
	if err != nil {
		return claim, c.failImport(claim, req, err)
	}
	if len(window.Events) > 100 {
		return claim, c.failImport(claim, req, core.Fail("invalid_input", "history adapter exceeded 100-event transaction limit"))
	}
	req.Key = ""
	req.Command = "data-source.history.commit"
	var result core.HistoryImport
	_, err = c.Store.Mutate(stepCtx, req, func(tx *core.Tx) (any, error) {
		var e error
		result, e = tx.FinishHistoryImport(stepCtx, claim, window.Events, window.Complete, window.NextCursor, window.StopReason)
		return result, e
	})
	if err != nil {
		return claim, c.failImport(claim, req, err)
	}
	if result.Status == "failed" {
		return result, core.Fail("history_cursor_stalled", "history page was saved as partial; its time cursor cannot safely advance")
	}
	return result, nil
}
func (c Collector) failImport(claim core.HistoryImport, req core.Request, cause error) error {
	// The parent may have been stopped; leave a durable retry checkpoint without
	// storing raw provider stderr or a potentially credential-bearing message.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code := core.ErrorCode(cause)
	if cause == context.DeadlineExceeded || cause == context.Canceled {
		code = "timeout"
	}
	req.Key = ""
	req.Command = "data-source.history.failure"
	_, err := c.Store.Mutate(ctx, req, func(tx *core.Tx) (any, error) { return tx.FailHistoryImport(ctx, claim, code) })
	if err != nil && core.ErrorCode(err) != "conflict" {
		return err
	}
	return cause
}
