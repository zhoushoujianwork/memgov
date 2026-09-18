package dingtalkapp

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// SessionOptions is what one live Stream session reports back. Handle returns an
// error when the frame was not durably stored, and that error must reach the
// platform as a failed acknowledgement: a frame this process could not persist
// must not be confirmed as received.
type SessionOptions struct {
	Ready      func(detail map[string]any)
	Handle     func(ctx context.Context, raw []byte) error
	CardHandle func(ctx context.Context, eventID string, raw []byte) (any, error)
}

// Frames is the seam that keeps this adapter testable offline. Production dials
// the official Stream SDK; a test supplies recorded frames. The secret is passed
// as an argument and never stored on the adapter, so it lives only as long as
// the session that needs it.
type Frames func(ctx context.Context, cfg channel.Config, secret string, opts SessionOptions) error

// Secrets resolves the channel's credential reference. It is injectable so a
// test never needs a real keychain entry.
type Secrets func(ctx context.Context, ref string) (string, error)

// Adapter receives application-bot messages. Long-lived credentials are
// resolved per call. Short-lived reply webhooks stay only in this process and
// are keyed to the exact callback they came from; they never enter storage.
type Adapter struct {
	Frames  Frames
	Secrets Secrets
	// ProbeTimeout bounds how long a capability probe waits for the platform's own
	// readiness signal. Readiness is never inferred from a sleep.
	ProbeTimeout time.Duration
	HTTP         *http.Client
	APIBase      string
	Now          func() time.Time

	tokenMu     sync.Mutex
	token       string
	tokenClient string
	tokenSecret string
	tokenExpiry time.Time

	replyMu       sync.Mutex
	replySessions map[replySessionKey]replySession
}

func New() *Adapter {
	return &Adapter{Frames: streamFrames, Secrets: resolveSecret, ProbeTimeout: 20 * time.Second, HTTP: &http.Client{Timeout: 15 * time.Second}, APIBase: "https://api.dingtalk.com"}
}

func (a *Adapter) Name() string { return "dingtalk_app" }

func (a *Adapter) frames() (Frames, error) {
	if a.Frames == nil {
		return nil, core.Fail("unavailable", "the dingtalk_app adapter has no Stream implementation configured")
	}
	return a.Frames, nil
}

func (a *Adapter) secret(ctx context.Context, cfg channel.Config) (string, error) {
	resolve := a.Secrets
	if resolve == nil {
		resolve = resolveSecret
	}
	return resolve(ctx, cfg.CredentialRef)
}

// ProbeCapabilities connects with the application's own credential and records
// only what the connection actually established. Receiving is verified when the
// platform accepted the subscription; history and sending are separate
// capabilities this adapter does not have, and they stay unverified rather than
// being borrowed from the personal dws account.
func (a *Adapter) ProbeCapabilities(ctx context.Context, cfg channel.Config) (core.Capabilities, error) {
	caps := core.Capabilities{Verified: map[string]bool{}, Unverified: []string{}, Tool: "dingtalk_stream_sdk"}
	if cfg.Identity.ClientID == "" {
		return caps, core.Fail("invalid_input", "a dingtalk_app probe requires identity.client_id")
	}
	frames, err := a.frames()
	if err != nil {
		return caps, err
	}
	secret, err := a.secret(ctx, cfg)
	if err != nil {
		return caps, err
	}
	timeout := a.ProbeTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var ready bool
	var detail map[string]any
	err = frames(ctx, cfg, secret, SessionOptions{
		Ready: func(d map[string]any) {
			ready, detail = true, d
			// The probe wants proof of connection, not traffic. Once the platform
			// confirmed the subscription the session is closed again.
			cancel()
		},
		// A probe must not consume business messages: it would acknowledge frames
		// that this process is not storing.
		Handle: func(context.Context, []byte) error {
			return core.Fail("unavailable", "a probe session does not accept messages")
		}})
	if !ready {
		if err == nil {
			err = core.Fail("unavailable", "the Stream connection closed before the platform confirmed the subscription")
		}
		return caps, err
	}
	caps.Verified["receive"] = true
	caps.Verified["send"] = true
	caps.Unverified = append(caps.Unverified, "history", "group_history", "recall_events")
	if detail != nil {
		caps.Tool = "dingtalk_stream_sdk"
	}
	return caps, nil
}

// ReadWindow is deliberately unsupported. A bot has no verified history read, and
// substituting the personal account's history would mean answering with material
// the application was never authorized to see.
func (a *Adapter) ReadWindow(context.Context, channel.Config, channel.Window) (channel.WindowResult, error) {
	return channel.WindowResult{Events: []core.NormalizedEvent{}},
		channel.Unsupported("dingtalk_app adapter", "history backfill")
}

// RunReceiver holds one live Stream session. A frame is parsed, then handed to
// the caller for a committed write; only a successful write allows the session to
// acknowledge the frame. A frame that cannot be parsed is isolated through Reject
// and acknowledged once that record is committed, so it is visible rather than
// lost.
func (a *Adapter) RunReceiver(ctx context.Context, cfg channel.Config, opts channel.ReceiverOptions) error {
	if opts.Handle == nil {
		return core.Fail("invalid_input", "receiver requires a handler")
	}
	frames, err := a.frames()
	if err != nil {
		return err
	}
	secret, err := a.secret(ctx, cfg)
	if err != nil {
		return err
	}
	session := SessionOptions{
		CardHandle: func(ctx context.Context, eventID string, raw []byte) (any, error) {
			cb, err := ParseCardCallback(cfg, eventID, raw)
			if err == nil && opts.ConfirmCard != nil {
				var status string
				status, err = opts.ConfirmCard(ctx, cb)
				if err == nil {
					return cardStatusResponse(status), nil
				}
			} else if err == nil {
				return nil, core.Fail("unavailable", "card confirmation handler is not configured")
			}
			// Refusals change no shared card state. Only ACK after recording the
			// refusal; storage/lease failures stay unacknowledged for redelivery.
			if containsCardRefusal(core.ErrorCode(err)) && opts.Reject != nil {
				if recordErr := opts.Reject(ctx, channel.RejectedEvent{Reason: core.ErrorCode(err), Detail: err.Error()}); recordErr != nil {
					return nil, recordErr
				}
				return map[string]any{}, nil
			}
			return nil, err
		},
		Ready: func(detail map[string]any) {
			if opts.Ready != nil {
				opts.Ready(detail)
			}
		},
		Handle: func(ctx context.Context, raw []byte) error {
			e, parseErr := ParseFrame(cfg, raw)
			if parseErr != nil {
				if opts.Reject == nil {
					// Without a place to record it, the frame would disappear. Failing
					// the acknowledgement keeps the platform's copy authoritative.
					return parseErr
				}
				payload := ""
				if core.ErrorCode(parseErr) != "denied" {
					// A frame from another tenant or robot is recorded by reason only.
					// Storing its body would import another audience's content.
					payload = redact(raw)
				}
				return opts.Reject(ctx, channel.RejectedEvent{Reason: core.ErrorCode(parseErr),
					Detail: parseErr.Error(), Payload: payload})
			}
			if err := opts.Handle(ctx, e); err != nil {
				return err
			}
			// The callback is now durable. Keep its short-lived bearer credential
			// only in memory so a later outbox delivery can use DingTalk's native
			// reply/@ path without exposing the webhook to SQLite or the Agent.
			a.rememberReplySession(cfg, e, raw)
			return nil
		}}
	err = frames(ctx, cfg, secret, session)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

var _ channel.Adapter = (*Adapter)(nil)
var _ channel.Reactor = (*Adapter)(nil)
