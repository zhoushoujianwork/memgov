package channel

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// Collector binds an adapter to the store. It owns the transaction boundaries so
// an adapter never writes to the database directly.
type Collector struct {
	Store   *core.Store
	Adapter Adapter
	// Conversations optionally narrows a receive session to an explicit subset
	// of the channel's active routes. Nil keeps the channel CLI behavior of
	// receiving every active route.
	Conversations    []string
	CollectAllDirect bool
	DirectSourceID   string
	EarliestSentAt   time.Time
	OnReady          func(map[string]any)
	// OnIntake runs only after a fresh event has committed. It may wake local
	// consumers, but it must never be required for durability or recovery.
	OnIntake func(core.IntakeResult)
}

// PullResult reports one backfill window honestly: how much was written, whether
// the range was fully covered, and what remains.
type PullResult struct {
	Channel      string          `json:"channel"`
	Conversation string          `json:"conversation_id"`
	Start        string          `json:"start_at"`
	End          string          `json:"end_at"`
	Events       int             `json:"events"`
	Applied      int             `json:"applied"`
	Duplicates   int             `json:"duplicates"`
	Complete     bool            `json:"complete"`
	StopReason   string          `json:"stop_reason,omitempty"`
	NextCursor   string          `json:"next_cursor,omitempty"`
	Watermark    core.Watermark  `json:"watermark"`
	Rejected     []RejectedEvent `json:"rejected,omitempty"`
	Note         string          `json:"note"`
}

// Pull backfills one half-open window and records its coverage. Each event is
// written in its own transaction so an interrupted pull keeps what it already
// committed, and the coverage record states exactly how far reading got.
func (c Collector) Pull(ctx context.Context, req core.Request, channelValue, conversation string, start, end time.Time, pageLimit, maxItems int) (PullResult, error) {
	return c.PullCursor(ctx, req, channelValue, conversation, start, end, "", pageLimit, maxItems)
}

func (c Collector) PullCursor(ctx context.Context, req core.Request, channelValue, conversation string, start, end time.Time, cursor string, pageLimit, maxItems int) (PullResult, error) {
	out := PullResult{Conversation: conversation, Start: start.UTC().Format(time.RFC3339), End: end.UTC().Format(time.RFC3339)}
	stored, err := core.ReadChannel(ctx, c.Store.DB, channelValue)
	if err != nil {
		return out, err
	}
	out.Channel = stored.Name
	// Reading history requires a verified history capability. An unverified
	// capability is treated as absent rather than attempted hopefully.
	if !stored.Capabilities.Verified["history"] {
		return out, core.Fail("unavailable", "channel %s has no verified history capability; run channel probe first", stored.Name)
	}
	route, err := core.RouteFor(ctx, c.Store.DB, stored.ID, conversation)
	if err != nil {
		return out, err
	}
	cfg := ConfigFor(stored)
	window, err := c.Adapter.ReadWindow(ctx, cfg, Window{ConversationID: conversation, ConversationType: route.ConversationType, Start: start, End: end, Cursor: cursor, PageLimit: pageLimit, MaxItems: maxItems})
	out.Events = len(window.Events)
	out.Complete, out.StopReason, out.NextCursor = window.Complete, window.StopReason, window.NextCursor
	if err != nil {
		// A failed read is still recorded as an unresolved gap, so a failure never
		// looks like an empty range.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, recordErr := c.recordCoverage(cleanup, req, stored.ID, conversation, out, "read_failed: "+core.ErrorCode(err)); recordErr != nil {
			return out, recordErr
		}
		return out, err
	}
	for _, e := range window.Events {
		if !c.EarliestSentAt.IsZero() {
			if sent, parseErr := time.Parse(time.RFC3339, e.SentAt); parseErr == nil && sent.Before(c.EarliestSentAt) {
				continue
			}
		}
		result, err := c.intake(ctx, req, stored.Name, e)
		if err != nil {
			out.StopReason = "intake_failed: " + core.ErrorCode(err)
			out.Complete = false
			if _, recordErr := c.recordCoverage(ctx, req, stored.ID, conversation, out, out.StopReason); recordErr != nil {
				return out, recordErr
			}
			return out, err
		}
		if result.Duplicate {
			out.Duplicates++
			continue
		}
		out.Applied++
	}
	mark, err := c.recordCoverage(ctx, req, stored.ID, conversation, out, out.StopReason)
	if err != nil {
		return out, err
	}
	out.Watermark = mark
	if out.Complete {
		out.Note = "window was read completely; covered_until advanced"
	} else {
		out.Note = "window is partial and recorded as a gap; this is not the end of the history"
	}
	return out, nil
}

func (c Collector) intake(ctx context.Context, req core.Request, channelName string, e core.NormalizedEvent) (core.IntakeResult, error) {
	// Each event carries its own idempotency key so a re-run of the same pull is
	// a no-op instead of a duplicate write.
	scoped := req
	scoped.Command = "channel.intake"
	scoped.Key = channelName + "|" + e.ConversationID + "|" + e.Kind + "|" + e.ProviderMessageID + "|" + e.ProviderEventID
	if e.ConversationType == "direct" {
		scoped.Key += "|" + e.ParseVersion
	}
	var result core.IntakeResult
	out, err := c.Store.Mutate(ctx, scoped, func(tx *core.Tx) (any, error) {
		var err error
		if e.ConversationType == "direct" && c.DirectSourceID != "" {
			if _, err = tx.EnsureDataSourceDirectConversation(ctx, c.DirectSourceID, e); err != nil {
				return nil, err
			}
		}
		result, err = tx.Intake(ctx, channelName, e)
		if err != nil {
			return nil, err
		}
		if e.Kind != core.EventRecall && result.Status != "ignored" {
			if err = tx.ObserveMessage(ctx, result.ChannelID, e.ConversationID, e.SentAt); err != nil {
				return nil, err
			}
		}
		return result, nil
	})
	return decodeIntake(result, out, err)
}

// decodeIntake reads the recorded result when the request was already served.
// A cached request does not run the closure, so the previous outcome is the
// authoritative answer and must not be mistaken for a fresh write.
func decodeIntake(result core.IntakeResult, out core.Result, err error) (core.IntakeResult, error) {
	if err != nil {
		return result, err
	}
	if !out.Cached {
		return result, nil
	}
	var cached core.IntakeResult
	if len(out.Data) > 0 && json.Unmarshal(out.Data, &cached) == nil {
		cached.Duplicate = true
		return cached, nil
	}
	result.Duplicate = true
	return result, nil
}

func (c Collector) recordCoverage(ctx context.Context, req core.Request, channelID, conversation string, out PullResult, reason string) (core.Watermark, error) {
	scoped := req
	scoped.Command = "channel.coverage"
	state := out.NextCursor
	if out.Complete {
		state = "complete"
	}
	scoped.Key = channelID + "|" + conversation + "|" + out.Start + "|" + out.End + "|" + state
	var mark core.Watermark
	_, err := c.Store.Mutate(ctx, scoped, func(tx *core.Tx) (any, error) {
		var err error
		mark, err = tx.RecordCoverage(ctx, core.CoverageWindow{ChannelID: channelID, ConversationID: conversation,
			StartAt: out.Start, EndAt: out.End, Complete: out.Complete, Messages: out.Events,
			StopReason: reason, Cursor: out.NextCursor})
		return mark, err
	})
	return mark, err
}

// ReceiveResult reports one live receive session. An event that could not be
// persisted is never counted as received.
type ReceiveResult struct {
	Channel   string          `json:"channel"`
	Listening bool            `json:"listening"`
	Ready     map[string]any  `json:"ready,omitempty"`
	Applied   int             `json:"applied"`
	Duplicate int             `json:"duplicates"`
	Rejected  []RejectedEvent `json:"rejected,omitempty"`
	Lease     core.Lease      `json:"lease"`
	StoppedBy string          `json:"stopped_by,omitempty"`
	Note      string          `json:"note"`
}

// Receive runs one live session under a channel lease, so two receivers cannot
// both consume the same channel. Every event is committed before it counts as
// received; a write failure stops the loop rather than acknowledging a loss.
func (c Collector) Receive(ctx context.Context, req core.Request, channelValue string, ttl time.Duration) (ReceiveResult, error) {
	out := ReceiveResult{Rejected: []RejectedEvent{}}
	stored, err := core.ReadChannel(ctx, c.Store.DB, channelValue)
	if err != nil {
		return out, err
	}
	out.Channel = stored.Name
	if !stored.Capabilities.Verified["receive"] {
		return out, core.Fail("unavailable", "channel %s has no verified receive capability; run channel probe first", stored.Name)
	}
	if len(stored.Routes) == 0 {
		return out, core.Fail("invalid_input", "channel %s has no bound conversation to receive from", stored.Name)
	}
	cfg := ConfigFor(stored)
	if c.Conversations != nil {
		cfg.Conversations = append([]string{}, c.Conversations...)
		selected := map[string]bool{}
		for _, conversation := range cfg.Conversations {
			selected[conversation] = true
		}
		direct := []string{}
		for _, conversation := range cfg.DirectConversations {
			if selected[conversation] {
				direct = append(direct, conversation)
			}
		}
		cfg.DirectConversations = direct
		if len(cfg.Conversations) == 0 && !c.CollectAllDirect {
			return out, core.Fail("invalid_input", "receive conversation subset cannot be empty")
		}
		for _, conversation := range cfg.Conversations {
			if _, err = core.RouteFor(ctx, c.Store.DB, stored.ID, conversation); err != nil {
				return out, err
			}
		}
	}
	cfg.CollectAllDirect = c.CollectAllDirect
	cfg.CollectGroups = len(cfg.Conversations) > len(cfg.DirectConversations)
	if cfg.CollectAllDirect && len(cfg.Conversations) == 0 {
		// The adapter can discover new direct conversations from the enterprise
		// stream without a pre-existing route.
		cfg.CollectGroups = false
	}
	lease := req
	lease.Command = "channel.lease"
	// A receive session performs several durable transactions. The caller's
	// request ID identifies the session, not each write to requests(id).
	lease.ID = ""
	// Acquiring a lease deliberately carries no idempotency key. Each session must
	// really take the lease and receive its own fence; replaying a cached grant
	// would hand a new receiver a token that no longer holds the channel.
	lease.Key = ""
	if _, err = c.Store.Mutate(ctx, lease, func(tx *core.Tx) (any, error) {
		var err error
		out.Lease, err = tx.AcquireLease(ctx, stored.ID, req.Actor+"@receiver", ttl)
		return out.Lease, err
	}); err != nil {
		return out, err
	}
	opts := ReceiverOptions{
		ConfirmCard: func(ctx context.Context, cb core.RuntimeCardCallback) (string, error) {
			write := req
			write.ID, write.Key = "", ""
			write.Command = "channel.card.confirm"
			var status string
			_, err := c.Store.Mutate(ctx, write, func(tx *core.Tx) (any, error) {
				if err := core.CheckLease(ctx, tx.Conn, stored.ID, out.Lease.Token, out.Lease.Fence); err != nil {
					return nil, core.Fail("unavailable", "card receiver no longer holds its channel lease")
				}
				var err error
				status, err = tx.ConfirmRuntimeCard(ctx, stored.ID, cb)
				return status, err
			})
			return status, err
		},
		Ready: func(detail map[string]any) {
			out.Listening = true
			out.Ready = detail
			if c.OnReady != nil {
				c.OnReady(detail)
			}
		},
		Handle: func(ctx context.Context, e core.NormalizedEvent) error {
			// The lease is re-checked inside the write, so a receiver that lost the
			// channel cannot keep committing events.
			result, err := c.intakeFenced(ctx, req, stored, e, out.Lease)
			if err != nil {
				return err
			}
			if result.Duplicate {
				out.Duplicate++
				return nil
			}
			out.Applied++
			if c.OnIntake != nil {
				c.OnIntake(result)
			}
			return nil
		},
		Reject: func(ctx context.Context, r RejectedEvent) error {
			out.Rejected = append(out.Rejected, r)
			write := req
			write.ID, write.Key = "", ""
			write.Command = "channel.reject"
			_, err := c.Store.Mutate(ctx, write, func(tx *core.Tx) (any, error) {
				if err := core.CheckLease(ctx, tx.Conn, stored.ID, out.Lease.Token, out.Lease.Fence); err != nil {
					return nil, err
				}
				return nil, tx.RecordRejectedEvent(ctx, stored.ID, r.Reason, r.Detail, r.Payload)
			})
			return err
		},
	}
	receiverCtx, cancelReceiver := context.WithCancel(ctx)
	renewDone := make(chan error, 1)
	go func() {
		currentLease := out.Lease
		interval := ttl / 3
		if interval < 100*time.Millisecond {
			interval = 100 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-receiverCtx.Done():
				renewDone <- nil
				return
			case <-ticker.C:
				next, renewErr := renewReceiverLease(receiverCtx, currentLease, func(renewCtx context.Context) (core.Lease, error) {
					renew := req
					renew.Command = "channel.lease.renew"
					renew.ID = ""
					renew.Key = ""
					var renewed core.Lease
					_, err := c.Store.Mutate(renewCtx, renew, func(tx *core.Tx) (any, error) {
						var err error
						renewed, err = tx.RenewLease(renewCtx, stored.ID, currentLease.Token, currentLease.Fence, ttl)
						return renewed, err
					})
					return renewed, err
				})
				if renewErr != nil {
					// The parent or adapter may close the receiver while a renewal is
					// in flight. That is a normal shutdown, not a lease failure.
					if receiverCtx.Err() != nil {
						renewDone <- nil
						return
					}
					cancelReceiver()
					renewDone <- renewErr
					return
				}
				currentLease = next
			}
		}
	}()
	runErr := c.Adapter.RunReceiver(receiverCtx, cfg, opts)
	cancelReceiver()
	renewErr := <-renewDone
	// Shutdown commonly arrives through a canceled parent context. Use a short
	// independent context so a clean stop can hand the lease over immediately.
	releaseCtx, cancelRelease := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRelease()
	release := req
	release.Command = "channel.lease.release"
	release.ID = ""
	release.Key = ""
	if _, err = c.Store.Mutate(releaseCtx, release, func(tx *core.Tx) (any, error) {
		return nil, tx.ReleaseLease(releaseCtx, stored.ID, out.Lease.Token)
	}); err != nil {
		return out, err
	}
	if renewErr != nil {
		out.StoppedBy = core.ErrorCode(renewErr)
		out.Note = "receiving stopped because its lease could not be renewed"
		return out, renewErr
	}
	if runErr != nil {
		out.StoppedBy = core.ErrorCode(runErr)
		out.Note = "receiving stopped; recover by pulling an overlapping window before trusting the watermark"
		return out, runErr
	}
	out.StoppedBy = "stream_closed"
	out.Note = "the event stream closed; overlap the next pull with covered_until because delivery is not transactional"
	return out, nil
}

// renewReceiverLease tolerates transient SQLite contention while the current
// fence is still valid. Long-running source writes must not tear down the bot
// Stream after one busy timeout; a replaced fence and an actually expiring
// lease still stop the receiver immediately.
func renewReceiverLease(ctx context.Context, current core.Lease, renew func(context.Context) (core.Lease, error)) (core.Lease, error) {
	expires, err := time.Parse(time.RFC3339Nano, current.Until)
	if err != nil {
		return current, core.Fail("internal", "receiver lease has an invalid expiry")
	}
	retryDelay := 250 * time.Millisecond
	for {
		if !time.Now().Before(expires) {
			return current, core.Fail("unavailable", "receiver lease expired before renewal")
		}
		attemptCtx, cancel := context.WithDeadline(ctx, expires)
		next, renewErr := renew(attemptCtx)
		cancel()
		if renewErr == nil {
			return next, nil
		}
		if ctx.Err() != nil {
			return current, ctx.Err()
		}
		if core.ErrorCode(renewErr) != "unavailable" || time.Until(expires) <= retryDelay {
			return current, renewErr
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return current, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c Collector) intakeFenced(ctx context.Context, req core.Request, stored core.Channel, e core.NormalizedEvent, lease core.Lease) (core.IntakeResult, error) {
	scoped := req
	scoped.Command = "channel.receive"
	scoped.ID = ""
	scoped.Key = stored.Name + "|" + e.Kind + "|" + e.ProviderMessageID + "|" + e.ProviderEventID
	var result core.IntakeResult
	out, err := c.Store.Mutate(ctx, scoped, func(tx *core.Tx) (any, error) {
		if err := core.CheckLease(ctx, tx.Conn, stored.ID, lease.Token, lease.Fence); err != nil {
			return nil, err
		}
		var err error
		if !c.EarliestSentAt.IsZero() {
			sent, parseErr := time.Parse(time.RFC3339, e.SentAt)
			if parseErr != nil || sent.Before(c.EarliestSentAt) {
				return nil, core.Fail("denied", "message is outside the enabled retention window")
			}
		}
		if e.ConversationType == "direct" && c.DirectSourceID != "" {
			if _, err = tx.EnsureDataSourceDirectConversation(ctx, c.DirectSourceID, e); err != nil {
				return nil, err
			}
		}
		result, err = tx.Intake(ctx, stored.Name, e)
		if err != nil {
			return nil, err
		}
		if e.Kind != core.EventRecall {
			if err = tx.ObserveMessage(ctx, stored.ID, e.ConversationID, e.SentAt); err != nil {
				return nil, err
			}
		}
		return result, nil
	})
	return decodeIntake(result, out, err)
}

// recordRejection keeps a malformed or unsupported event visible instead of
// dropping it, so a parser gap can be found and replayed later.
func (c Collector) recordRejection(ctx context.Context, req core.Request, stored core.Channel, r RejectedEvent) error {
	scoped := req
	scoped.Command = "channel.reject"
	scoped.ID = ""
	scoped.Key = "reject|" + stored.ID + "|" + core.Hash([]byte(r.Payload+r.Reason+r.Detail))
	_, err := c.Store.Mutate(ctx, scoped, func(tx *core.Tx) (any, error) {
		return nil, tx.RecordRejectedEvent(ctx, stored.ID, r.Reason, r.Detail, r.Payload)
	})
	return err
}
