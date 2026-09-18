package dingtalkapp

import (
	"context"
	"sync"
	"time"

	sdk "github.com/open-dingtalk/dingtalk-stream-sdk-go/client"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/payload"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/utils"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// maxInFlight bounds how many frames may be waiting for a write at once. The SDK
// starts a goroutine per frame, so without a bound a burst would grow memory
// without limit. A saturated queue reports backpressure through a failed
// acknowledgement instead of buffering silently.
const maxInFlight = 16

// queueWait is how long a frame waits for a slot before backpressure is
// reported. It is short on purpose: the platform's own redelivery is a better
// buffer than this process's memory.
const queueWait = 5 * time.Second

// The upstream SDK's reconnect loop runs on context.Background(), including
// after an intentional Close. A bounded session lets the caller take a fresh
// lease and connection without leaving an orphaned callback subscriber.
const maxStreamSessionAge = 5 * time.Minute

type streamSessionClient interface {
	Start(context.Context) error
	Close()
}

func superviseStreamSession(ctx context.Context, cli streamSessionClient, age time.Duration, ready func()) error {
	if err := cli.Start(ctx); err != nil {
		return core.Fail("unavailable", "the Stream connection could not be established: %v", err)
	}
	defer cli.Close()
	if ready != nil {
		ready()
	}
	clock := time.NewTimer(age)
	defer clock.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-clock.C:
		// The SDK does not expose a process-loop completion signal when automatic
		// reconnect is disabled. Rotate before a dead socket can remain silent
		// indefinitely; unacknowledged frames remain with the platform.
		return nil
	}
}

// streamFrames runs one Stream session with the official SDK. It uses the
// low-level frame handler rather than the SDK's chatbot convenience wrapper,
// because the response for each frame must be decided by whether this process
// actually committed the message: the wrapper acknowledges success on its own.
func streamFrames(ctx context.Context, cfg channel.Config, secret string, opts SessionOptions) error {
	if cfg.Identity.ClientID == "" || secret == "" {
		return core.Fail("invalid_input", "a Stream session requires the application client_id and its secret")
	}
	// Writes are serialized and bounded. SQLite admits one writer anyway, and the
	// caller's counters are not written concurrently as a result.
	slots := make(chan struct{}, maxInFlight)
	var writing sync.Mutex
	handle := func(frame *payload.DataFrame, isCard bool) (*payload.DataFrameResponse, error) {
		if frame == nil || frame.Data == "" {
			// Nothing to store. Refusing is honest: this process cannot say it
			// received a message it never saw.
			return nil, core.Fail("invalid_input", "the frame carried no data")
		}
		timer := time.NewTimer(queueWait)
		defer timer.Stop()
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-timer.C:
			return nil, core.Fail("unavailable", "the receiver is saturated at %d in-flight frames; the frame was not stored", maxInFlight)
		case <-ctx.Done():
			return nil, core.Fail("unavailable", "the receiver is shutting down; the frame was not stored")
		}
		writing.Lock()
		defer writing.Unlock()
		if isCard {
			if opts.CardHandle == nil {
				return nil, core.Fail("unavailable", "this session does not accept card callbacks")
			}
			result, err := opts.CardHandle(ctx, frame.GetMessageId(), []byte(frame.Data))
			if err != nil {
				return nil, err
			}
			response := payload.NewSuccessDataFrameResponse()
			if err = response.SetJson(map[string]any{"response": result}); err != nil {
				return nil, err
			}
			return response, nil
		}
		if err := opts.Handle(ctx, []byte(frame.Data)); err != nil {
			// The write failed, so the platform must not be told the frame was
			// received. Returning the error makes the SDK send a failure response.
			return nil, err
		}
		// The frame is committed. Only now is it acknowledged, and the
		// acknowledgement means stored, not answered.
		return payload.NewSuccessDataFrameResponse(), nil
	}
	cli := sdk.NewStreamClient(
		sdk.WithAppCredential(sdk.NewAppCredentialConfig(cfg.Identity.ClientID, secret)),
		// v0.9.1 reconnects in a background context even after Close. That can
		// create a subscriber with no channel lease, including after a probe.
		sdk.WithAutoReconnect(false),
		sdk.WithSubscription(utils.SubscriptionTypeKCallback, payload.BotMessageCallbackTopic,
			func(_ context.Context, frame *payload.DataFrame) (*payload.DataFrameResponse, error) {
				return handle(frame, false)
			}))
	// A probe never subscribes to approval callbacks. Without an associated
	// template the existing message receiver needs no extra subscription.
	if cfg.Identity.ConfirmationCardTemplate != "" && opts.CardHandle != nil {
		cli.RegisterRouter(utils.SubscriptionTypeKCallback, payload.CardInstanceCallbackTopic,
			func(_ context.Context, frame *payload.DataFrame) (*payload.DataFrameResponse, error) {
				return handle(frame, true)
			})
	}
	return superviseStreamSession(ctx, cli, maxStreamSessionAge, func() {
		// Start establishes the SDK connection endpoint and WebSocket; it cannot
		// prove that a business callback has arrived. No credential or external
		// stable identifier is copied into the readiness report.
		if opts.Ready != nil {
			opts.Ready(map[string]any{"transport": "stream", "topic": payload.BotMessageCallbackTopic,
				"max_in_flight": maxInFlight})
		}
	})
}
