package dingtalkapp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// fakeStream replays recorded frames instead of dialing the platform. It records
// the per-frame outcome, which is exactly what the real SDK turns into an
// acknowledgement, so the ACK contract is observable offline.
type fakeStream struct {
	frames  []string
	ready   bool
	results []error
}

func (f *fakeStream) run(ctx context.Context, _ channel.Config, secret string, opts SessionOptions) error {
	if secret == "" {
		return core.Fail("denied", "the fake stream was started without a secret")
	}
	if opts.Ready != nil {
		f.ready = true
		opts.Ready(map[string]any{"transport": "stream"})
	}
	for _, raw := range f.frames {
		err := opts.Handle(ctx, []byte(raw))
		f.results = append(f.results, err)
	}
	return nil
}

func testSecret(context.Context, string) (string, error) { return "app-secret", nil }

func adapter(f *fakeStream) *Adapter {
	return &Adapter{Frames: f.run, Secrets: testSecret}
}

// A frame that was stored is acknowledged; a frame whose write failed is not. The
// platform's copy stays authoritative when this process could not persist it.
func TestReceiverOnlyAcknowledgesWhatWasStored(t *testing.T) {
	stream := &fakeStream{frames: []string{frame(""), frame("")}}
	var stored int
	writeErr := core.Fail("unavailable", "database is locked")
	err := adapter(stream).RunReceiver(context.Background(), config(), channel.ReceiverOptions{
		Handle: func(_ context.Context, e core.NormalizedEvent) error {
			stored++
			if stored == 2 {
				return writeErr
			}
			if e.ProviderMessageID != "m1" {
				t.Fatalf("unexpected event: %+v", e)
			}
			return nil
		}})
	if err != nil {
		t.Fatalf("receiver: %v", err)
	}
	if !stream.ready {
		t.Fatal("readiness was never reported")
	}
	if len(stream.results) != 2 || stream.results[0] != nil {
		t.Fatalf("a stored frame was not acknowledged: %+v", stream.results)
	}
	if !errors.Is(stream.results[1], writeErr) {
		t.Fatalf("a failed write was acknowledged as received: %+v", stream.results[1])
	}
}

func TestReceiverAcceptsMixedRichTextAfterWritingUnreadableParts(t *testing.T) {
	stream := &fakeStream{frames: []string{richTextFrame([]any{
		map[string]any{"text": "请看图"},
		map[string]any{"type": "picture", "downloadCode": "MEDIA_SECRET"},
		map[string]any{"text": "链接", "url": "https://example.com/PRIVATE_TOKEN"},
	})}}
	var received []core.NormalizedEvent
	var rejected int
	err := adapter(stream).RunReceiver(context.Background(), config(), channel.ReceiverOptions{
		Handle: func(_ context.Context, e core.NormalizedEvent) error {
			received = append(received, e)
			return nil
		},
		Reject: func(context.Context, channel.RejectedEvent) error { rejected++; return nil },
	})
	if err != nil || rejected != 0 || len(received) != 1 || len(stream.results) != 1 || stream.results[0] != nil {
		t.Fatalf("mixed message was rejected or not acknowledged: received=%d rejected=%d results=%v err=%v", len(received), rejected, stream.results, err)
	}
	if !strings.Contains(received[0].Body, "内容未读取") || !strings.Contains(received[0].Body, "目标未读取") ||
		strings.Contains(received[0].Body, "PRIVATE_TOKEN") || strings.Contains(received[0].Payload, "MEDIA_SECRET") {
		t.Fatalf("unreadable parts were hidden or credentials leaked: %q", received[0].Body)
	}
}

// An unparseable frame is recorded through Reject and then acknowledged: it is
// visible in the inbox, so redelivering it would only duplicate the record.
func TestReceiverIsolatesUnparseableFramesInsteadOfLosingThem(t *testing.T) {
	bad := strings.Replace(frame(""), `"msgtype":"text"`, `"msgtype":"picture"`, 1)
	stream := &fakeStream{frames: []string{bad}}
	var rejected []channel.RejectedEvent
	err := adapter(stream).RunReceiver(context.Background(), config(), channel.ReceiverOptions{
		Handle: func(context.Context, core.NormalizedEvent) error {
			t.Fatal("an unsupported frame reached the message handler")
			return nil
		},
		Reject: func(_ context.Context, r channel.RejectedEvent) error {
			rejected = append(rejected, r)
			return nil
		}})
	if err != nil {
		t.Fatalf("receiver: %v", err)
	}
	if len(rejected) != 1 || rejected[0].Reason != "invalid_input" {
		t.Fatalf("the frame was not isolated: %+v", rejected)
	}
	if !strings.Contains(rejected[0].Payload, "[redacted]") {
		t.Fatalf("the isolated payload was not redacted: %s", rejected[0].Payload)
	}
	if stream.results[0] != nil {
		t.Fatalf("a recorded rejection was not acknowledged: %v", stream.results[0])
	}
}

// A frame from another tenant is isolated by reason only. Storing its body would
// import content this channel was never authorized to see.
func TestReceiverRecordsAForeignFrameWithoutStoringItsBody(t *testing.T) {
	foreign := strings.Replace(frame(""), `"chatbotCorpId":"corp1"`, `"chatbotCorpId":"corp9"`, 1)
	stream := &fakeStream{frames: []string{foreign}}
	var rejected []channel.RejectedEvent
	if err := adapter(stream).RunReceiver(context.Background(), config(), channel.ReceiverOptions{
		Handle: func(context.Context, core.NormalizedEvent) error { return nil },
		Reject: func(_ context.Context, r channel.RejectedEvent) error {
			rejected = append(rejected, r)
			return nil
		}}); err != nil {
		t.Fatalf("receiver: %v", err)
	}
	if len(rejected) != 1 || rejected[0].Reason != "denied" || rejected[0].Payload != "" {
		t.Fatalf("a foreign frame's body was stored: %+v", rejected)
	}
}

// If the rejection itself cannot be recorded, the frame is not acknowledged. It
// must not vanish between a failed record and a successful ACK.
func TestReceiverRefusesToAcknowledgeAnUnrecordableRejection(t *testing.T) {
	bad := strings.Replace(frame(""), `"msgtype":"text"`, `"msgtype":"picture"`, 1)
	stream := &fakeStream{frames: []string{bad}}
	if err := adapter(stream).RunReceiver(context.Background(), config(), channel.ReceiverOptions{
		Handle: func(context.Context, core.NormalizedEvent) error { return nil }}); err != nil {
		t.Fatalf("receiver: %v", err)
	}
	if stream.results[0] == nil {
		t.Fatal("a frame that could not be recorded was acknowledged")
	}
}

// History is refused rather than borrowed from the personal account.
func TestAdapterRefusesHistoryItDoesNotHave(t *testing.T) {
	a := adapter(&fakeStream{})
	if _, err := a.ReadWindow(context.Background(), config(), channel.Window{ConversationID: "cid:group2"}); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("history was not refused: %v", err)
	}
}

// A probe records only receiving, and it never consumes business messages: a
// probe session that accepted a frame would acknowledge a message it did not
// store.
func TestProbeVerifiesReceiveAndActiveSendWithoutConsumingMessages(t *testing.T) {
	stream := &fakeStream{frames: []string{frame("")}}
	caps, err := adapter(stream).ProbeCapabilities(context.Background(), config())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !caps.Verified["receive"] {
		t.Fatalf("receive was not verified: %+v", caps)
	}
	if !caps.Verified["send"] || caps.Verified["history"] {
		t.Fatalf("probe capability set is wrong: %+v", caps)
	}
	if len(stream.results) != 1 || stream.results[0] == nil {
		t.Fatalf("the probe consumed a message: %+v", stream.results)
	}
}

// A connection that closed before the platform confirmed the subscription is
// unavailable, not a verified capability.
func TestProbeRefusesToClaimReceiveWithoutReadiness(t *testing.T) {
	silent := func(context.Context, channel.Config, string, SessionOptions) error { return nil }
	a := &Adapter{Frames: silent, Secrets: testSecret}
	if _, err := a.ProbeCapabilities(context.Background(), config()); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("an unconfirmed connection was accepted: %v", err)
	}
}

// An unresolvable credential stops the session instead of connecting as some
// other identity.
func TestSessionRefusesWithoutAResolvableCredential(t *testing.T) {
	a := &Adapter{Frames: (&fakeStream{}).run, Secrets: func(context.Context, string) (string, error) {
		return "", core.Fail("denied", "the keychain entry is missing")
	}}
	if err := a.RunReceiver(context.Background(), config(), channel.ReceiverOptions{
		Handle: func(context.Context, core.NormalizedEvent) error { return nil }}); core.ErrorCode(err) != "denied" {
		t.Fatalf("a session started without a credential: %v", err)
	}
}

// The credential reference forms the channel schema accepts are resolved from the
// environment and from a file; anything else is refused rather than guessed.
func TestResolveSecretReadsOnlyReferencedSources(t *testing.T) {
	t.Setenv("MEMGOV_TEST_SECRET", "from-env")
	got, err := resolveSecret(context.Background(), "env://MEMGOV_TEST_SECRET")
	if err != nil || got != "from-env" {
		t.Fatalf("env reference: %q %v", got, err)
	}
	if _, err = resolveSecret(context.Background(), "env://MEMGOV_TEST_MISSING"); core.ErrorCode(err) != "denied" {
		t.Fatalf("an empty variable was accepted: %v", err)
	}
	if _, err = resolveSecret(context.Background(), ""); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("a missing reference was accepted: %v", err)
	}
	if _, err = resolveSecret(context.Background(), "https://example.com/secret"); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("an unsupported scheme was accepted: %v", err)
	}
	if _, err = resolveSecret(context.Background(), "keychain://service-without-account"); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("a malformed keychain reference was accepted: %v", err)
	}
}
