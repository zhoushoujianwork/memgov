package dws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func testConfig() channel.Config {
	return channel.Config{ChannelID: "ch1", ChannelName: "dws-main", Kind: core.ChannelDwsPersonal,
		Tenant: "corp1", IDNamespace: "dws:corp1", Conversations: []string{"cid:group1"},
		Identity: core.ChannelIdentity{Profile: "corp1:user1", ExpectedCorpID: "corp1", ExpectedUserID: "user1"}}
}

func TestReceiverRunsGroupAndDirectConsumersOnSharedBus(t *testing.T) {
	cfg := testConfig()
	cfg.CollectGroups, cfg.CollectAllDirect = true, true
	cfg.Conversations = append(cfg.Conversations, "cid:peer-new")
	groupLine := `{"event_type":"user_im_message_receive_group","event_id":"group-1","event_corp_id":"corp1","data":"{\"msgId\":\"gm-1\",\"conversationId\":\"cid:group1\",\"senderId\":\"alice\",\"text\":{\"content\":\"群消息\"}}"}` + "\n"
	directLine := `{"event_type":"user_im_message_receive_single","event_id":"direct-new","event_corp_id":"corp1","data":"{\"msgId\":\"dm-new\",\"conversationId\":\"cid:peer-new\",\"senderStaffId\":\"peer\",\"text\":{\"content\":\"私聊消息\"}}"}` + "\n"
	var mu sync.Mutex
	kinds := []string{}
	a := &Adapter{Timeout: time.Second, Stream: func(ctx context.Context, args ...string) (io.ReadCloser, io.ReadCloser, func() error, error) {
		joined := strings.Join(args, " ")
		kind, body := "all-group", groupLine
		if strings.Contains(joined, "--kind all-direct") {
			kind, body = "all-direct", directLine
		}
		mu.Lock()
		kinds = append(kinds, kind)
		mu.Unlock()
		return fakeStream(body, "[event] ready\n", 0, nil)(ctx, args...)
	}}
	ready := false
	handled := map[string]core.NormalizedEvent{}
	err := a.RunReceiver(context.Background(), cfg, channel.ReceiverOptions{Ready: func(detail map[string]any) { ready = detail["group"] == true && detail["direct"] == true }, Handle: func(_ context.Context, e core.NormalizedEvent) error { handled[e.ProviderMessageID] = e; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !ready || len(kinds) != 2 || len(handled) != 2 || handled["dm-new"].ConversationID != "cid:peer-new" || handled["dm-new"].ConversationType != "direct" {
		t.Fatalf("dual receive ready=%t kinds=%v handled=%+v", ready, kinds, handled)
	}
}

func envelope(result string) []byte {
	return []byte(`{"success":true,"result":` + result + `}`)
}

func TestDirectHistoryVerifiesOwnerIdentityAndKeepsBothSides(t *testing.T) {
	start := time.Date(2026, 9, 16, 7, 0, 0, 500000000, time.UTC)
	var identityCalls int
	a := &Adapter{Timeout: time.Second, Run: func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--conversation-id cid:peer") {
			t.Fatalf("wrong direct target: %s", joined)
		}
		if strings.Contains(joined, "--sender user1") {
			identityCalls++
			return envelope(`{"resolvedFilters":{"senders":[{"status":"resolved","selected":{"userId":"user1","openDingTalkId":"owner-open"}}]}}`), nil
		}
		return envelope(`{"messages":[{"messageId":"before","senderId":"peer-open","time":"2026-09-16T07:00:00Z","text":"before enable"},{"messageId":"incoming","senderId":"peer-open","senderName":"same name","time":"2026-09-16T07:00:01Z","text":"request"},{"messageId":"outgoing","senderId":"owner-open","senderName":"same name","time":"2026-09-16T07:00:02Z","text":"done"}],"complete":true,"stopReason":"source_complete"}`), nil
	}}
	w := channel.Window{ConversationID: "cid:peer", ConversationType: "direct", Start: start, End: start.Add(time.Minute)}
	for i := 0; i < 2; i++ {
		out, err := a.ReadWindow(context.Background(), testConfig(), w)
		if err != nil || !out.Complete || len(out.Events) != 2 || out.Events[0].Sender.SelfAuthor || !out.Events[1].Sender.SelfAuthor || out.Events[0].Sender.IDType != "open_id" {
			t.Fatalf("direct two-sided history: %+v %v", out, err)
		}
	}
	if identityCalls != 1 {
		t.Fatalf("owner identity reads: %d", identityCalls)
	}
}

// The probe refuses to collect when the logged-in dws identity is not the one the
// channel was bound to, instead of quietly reading another account's messages.
func TestProbeRefusesIdentityMismatch(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		output string
		code   string
	}{
		{"other corp", `{"currentProfile":"corp1:user1","profiles":[{"profile":"corp1:user1","corpId":"corp9","userId":"user1"}]}`, "denied"},
		{"other user", `{"currentProfile":"corp1:user1","profiles":[{"profile":"corp1:user1","corpId":"corp1","userId":"user9"}]}`, "denied"},
		{"not logged in", `{"currentProfile":"other","profiles":[{"profile":"other","corpId":"corp1","userId":"user1"}]}`, "denied"},
	}
	for _, tc := range cases {
		a := &Adapter{Run: func(ctx context.Context, args ...string) ([]byte, error) { return envelope(tc.output), nil }, Timeout: time.Second}
		if _, err := a.ProbeCapabilities(ctx, testConfig()); core.ErrorCode(err) != tc.code {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	ok := `{"currentProfile":"corp1:user1","profiles":[{"profile":"corp1:user1","corpId":"corp1","userId":"user1"}]}`
	a := &Adapter{Run: func(ctx context.Context, args ...string) ([]byte, error) { return envelope(ok), nil }, Timeout: time.Second}
	caps, err := a.ProbeCapabilities(ctx, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Verified["history"] || !caps.Verified["receive"] {
		t.Fatalf("probe did not record what it verified: %+v", caps)
	}
	// Sending must not become verified just because the CLI has the subcommand.
	if caps.Verified["send"] {
		t.Fatal("send was claimed without being exercised")
	}
	if !strings.Contains(strings.Join(caps.Unverified, ","), "send") {
		t.Fatalf("send must be listed unverified: %+v", caps.Unverified)
	}
}

// Reaching a page or item cap is partial with a stop reason, never "no more
// messages", so a truncated backfill cannot look complete.
func TestReadWindowReportsTruncationHonestly(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	var captured []string
	page := fmt.Sprintf(`{"messages":[{"messageId":"m1","text":"第一条","time":"2026-09-14T08:10:00Z","senderId":"alice","senderName":"Alice"}],"complete":false,"hasMore":true,"nextPage":{"direction":"newer","nextCursor":%d,"time":"2026-09-14T08:10:00Z"},"stopReason":""}`, start.Add(10*time.Minute).UnixMilli())
	a := &Adapter{Timeout: time.Second, Run: func(ctx context.Context, args ...string) ([]byte, error) {
		captured = args
		return envelope(page), nil
	}}
	out, err := a.ReadWindow(ctx, testConfig(), channel.Window{ConversationID: "cid:group1", Start: start, End: end, PageLimit: 2, MaxItems: 10})
	if err != nil {
		t.Fatal(err)
	}
	if out.Complete {
		t.Fatal("a page with hasMore was reported complete")
	}
	if out.StopReason == "" || out.NextCursor == "" {
		t.Fatalf("truncation not reported: %+v", out)
	}
	if len(out.Events) != 1 || out.Events[0].Origin != "history" || out.Events[0].ProviderEventID != "" {
		t.Fatalf("history event shape: %+v", out.Events)
	}
	if out.Events[0].ParseVersion != historyParseVersion {
		t.Fatal("parse version must travel with the event")
	}
	// The range must be passed as an explicit half-open window.
	joined := strings.Join(captured, " ")
	for _, want := range []string{"--group cid:group1", "--start 2026-09-14T08:00:00Z", "--end 2026-09-14T09:00:00Z", "--page-limit 2", "--max-items 10", "--profile corp1:user1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
	// A complete page with nothing left is the only case that claims completeness.
	full := `{"messages":[{"messageId":"m2","text":"完整","time":"2026-09-14T08:20:00Z","senderId":"alice"}],"complete":true,"hasMore":false}`
	a.Run = func(ctx context.Context, args ...string) ([]byte, error) { return envelope(full), nil }
	if out, err = a.ReadWindow(ctx, testConfig(), channel.Window{ConversationID: "cid:group1", Start: start, End: end}); err != nil {
		t.Fatal(err)
	}
	if !out.Complete || out.StopReason != "" {
		t.Fatalf("a fully read window was not reported complete: %+v", out)
	}
	// The dws paginator uses source_complete when it reached the source boundary.
	sourceComplete := `{"messages":[],"complete":false,"hasMore":false,"stopReason":"source_complete"}`
	a.Run = func(ctx context.Context, args ...string) ([]byte, error) { return envelope(sourceComplete), nil }
	if out, err = a.ReadWindow(ctx, testConfig(), channel.Window{ConversationID: "cid:group1", Start: start, End: end}); err != nil {
		t.Fatal(err)
	}
	if !out.Complete || out.StopReason != "" {
		t.Fatalf("source boundary was not treated as complete: %+v", out)
	}
	// A message without an identity stops the window instead of being stored.
	broken := `{"messages":[{"messageId":"","text":"无 ID"}],"complete":true}`
	a.Run = func(ctx context.Context, args ...string) ([]byte, error) { return envelope(broken), nil }
	if out, err = a.ReadWindow(ctx, testConfig(), channel.Window{ConversationID: "cid:group1", Start: start, End: end}); err != nil {
		t.Fatal(err)
	}
	if out.Complete || out.StopReason != "message_without_id" {
		t.Fatalf("a message without an ID was accepted: %+v", out)
	}
	// An empty or inverted range is refused rather than guessed.
	if _, err = a.ReadWindow(ctx, testConfig(), channel.Window{ConversationID: "cid:group1", Start: end, End: start}); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("inverted window accepted: %v", err)
	}
}

func TestDirectDiscoveryExcludesRobotConversationsAndReportsPartial(t *testing.T) {
	calls := 0
	a := &Adapter{Timeout: time.Second, Run: func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--conversation-type single") || !strings.Contains(joined, "--start") || !strings.Contains(joined, "--end") {
			t.Fatalf("unbounded direct discovery: %s", joined)
		}
		if strings.Contains(joined, "--only-robot") {
			return envelope(`{"messages":[{"conversationId":"cid:bot","sender":"机器人"}],"complete":true}`), nil
		}
		return envelope(`{"messages":[{"conversationId":"cid:peer","sender":"同事"},{"conversationId":"cid:bot","sender":"机器人"}],"complete":false,"hasMore":true,"stopReason":"page_limit"}`), nil
	}}
	start := time.Now().Add(-time.Hour)
	conversations, complete, reason, err := a.ListDirectConversations(context.Background(), testConfig(), start, time.Now(), 2, 10)
	if err != nil || complete || reason != "page_limit" || calls != 2 || len(conversations) != 1 || conversations[0].ID != "cid:peer" {
		t.Fatalf("direct discovery=%+v complete=%t reason=%q calls=%d err=%v", conversations, complete, reason, calls, err)
	}
}

// A dws error is surfaced as a typed failure, and a refused call is denied rather
// than reported as an empty history.
func TestHistoryErrorsAreTyped(t *testing.T) {
	ctx := context.Background()
	start := time.Now().Add(-time.Hour)
	for _, tc := range []struct{ output, code string }{
		{`{"error":{"code":"permission_denied","message":"no access"}}`, "denied"},
		{`{"error":{"code":"rate_limited","message":"slow down"}}`, "unavailable"},
		{`not json at all`, "unavailable"},
		{`{"success":false}`, "unavailable"},
	} {
		a := &Adapter{Timeout: time.Second, Run: func(ctx context.Context, args ...string) ([]byte, error) { return []byte(tc.output), nil }}
		out, err := a.ReadWindow(ctx, testConfig(), channel.Window{ConversationID: "cid:group1", Start: start, End: time.Now()})
		if core.ErrorCode(err) != tc.code {
			t.Fatalf("%s: %v", tc.output, err)
		}
		if len(out.Events) != 0 {
			t.Fatal("a failed call must not produce events")
		}
	}
}

// Event normalization keeps platform identity, isolates unsupported types and
// refuses events from another tenant.
func TestParseEventNormalizesAndIsolates(t *testing.T) {
	cfg := testConfig()
	line := `{"event_type":"user_im_message_receive_group","event_id":"evt1","event_corp_id":"corp1","event_born_time":1757843400000,"seq":7,"data":"{\"msgId\":\"m1\",\"conversationId\":\"cid:group1\",\"senderId\":\"alice\",\"senderNick\":\"Alice\",\"msgtype\":\"text\",\"text\":{\"content\":\"部署前先备份\"},\"createAt\":1757843400000,\"isInAtList\":true}"}`
	e, err := ParseEvent(cfg, []byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != core.EventMessage || e.ProviderMessageID != "m1" || e.ConversationID != "cid:group1" {
		t.Fatalf("normalized event: %+v", e)
	}
	if e.Sender.IDType != "union_id" || e.Sender.IDValue != "alice" || e.Sender.DisplayName != "Alice" {
		t.Fatalf("sender identity lost: %+v", e.Sender)
	}
	if e.Body != "部署前先备份" || !e.Mentioned || e.Ordering != "7" {
		t.Fatalf("event content: %+v", e)
	}
	if e.SentAt == "" || e.EventAt == "" {
		t.Fatal("platform time must be preserved")
	}
	if e.Payload == "" {
		t.Fatal("the raw line must be kept as evidence of what arrived")
	}
	projected := `{"type":"user_im_message_receive_group_all","event_id":"evt-flat","timestamp":1789384568507,"message_id":"m-flat","conversation_id":"cid:group1","sender":"Alert Bot","sender_open_dingtalk_id":"sender-open","content":"需要检查告警","create_time":"2026-09-14 19:16:07"}`
	flat, err := ParseEvent(cfg, []byte(projected))
	if err != nil {
		t.Fatal(err)
	}
	if flat.ProviderMessageID != "m-flat" || flat.ConversationID != "cid:group1" || flat.Body != "需要检查告警" {
		t.Fatalf("projected event: %+v", flat)
	}
	if flat.Sender.IDType != "union_id" || flat.Sender.IDValue != "sender-open" || flat.Sender.DisplayName != "Alert Bot" {
		t.Fatalf("projected sender: %+v", flat.Sender)
	}
	if flat.SentAt == "" || flat.EventAt == "" {
		t.Fatalf("projected times: %+v", flat)
	}
	recall := `{"event_type":"user_im_message_recall_group","event_id":"evt2","event_corp_id":"corp1","data":"{\"recalledMsgId\":\"m1\",\"conversationId\":\"cid:group1\"}"}`
	r, err := ParseEvent(cfg, []byte(recall))
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != core.EventRecall || r.ProviderMessageID != "m1" {
		t.Fatalf("recall event: %+v", r)
	}
	for _, tc := range []struct{ line, code string }{
		{`{"event_type":"user_im_message_receive_group","event_corp_id":"corp9","data":"{}"}`, "denied"},
		{`{"event_type":"user_im_reaction_add","event_corp_id":"corp1","data":"{}"}`, "invalid_input"},
		{`{"event_corp_id":"corp1","data":"{}"}`, "invalid_input"},
		{`not json`, "invalid_input"},
		{`{"event_type":"user_im_message_receive_group","event_corp_id":"corp1","data":"not json"}`, "invalid_input"},
		{`{"event_type":"user_im_message_receive_group","event_corp_id":"corp1","data":"{\"conversationId\":\"cid:group1\"}"}`, "invalid_input"},
	} {
		if _, err := ParseEvent(cfg, []byte(tc.line)); core.ErrorCode(err) != tc.code {
			t.Fatalf("%s: %v", tc.line, err)
		}
	}
}

// fakeStream serves scripted stdout and stderr, so readiness ordering and
// interleaving can be tested without any real dws process.
func fakeStream(stdout, stderr string, delay time.Duration, exit error) Streamer {
	return func(ctx context.Context, args ...string) (io.ReadCloser, io.ReadCloser, func() error, error) {
		outR, outW := io.Pipe()
		errR, errW := io.Pipe()
		go func() {
			// Events are written before the ready marker on purpose: a receiver must
			// not depend on ordering between the two streams.
			outW.Write([]byte(stdout))
			outW.Close()
		}()
		go func() {
			time.Sleep(delay)
			errW.Write([]byte(stderr))
			errW.Close()
		}()
		return outR, errR, func() error { return exit }, nil
	}
}

// Readiness comes from the platform's own marker, never from a timer, and both
// streams are consumed even when the ready line arrives late.
func TestReceiverWaitsForRealReadyMarkerAndDrainsBothStreams(t *testing.T) {
	ctx := context.Background()
	stdout := `{"event_type":"user_im_message_receive_group","event_id":"evt1","event_corp_id":"corp1","data":"{\"msgId\":\"m1\",\"conversationId\":\"cid:group1\",\"senderId\":\"alice\",\"text\":{\"content\":\"第一条\"}}"}` + "\n" +
		`{"event_type":"user_im_reaction_add","event_id":"evt2","event_corp_id":"corp1","data":"{}"}` + "\n" +
		`{"event_type":"user_im_message_receive_group","event_id":"evt3","event_corp_id":"corp1","data":"{\"msgId\":\"m2\",\"conversationId\":\"cid:group1\",\"senderId\":\"bob\",\"text\":{\"content\":\"第二条\"}}"}` + "\n"
	baseStream := fakeStream(stdout, "[event] ready event_count=2 bus_pid=4242\n", 40*time.Millisecond, nil)
	var streamArgs []string
	a := &Adapter{Timeout: time.Second, Stream: func(ctx context.Context, args ...string) (io.ReadCloser, io.ReadCloser, func() error, error) {
		streamArgs = append([]string{}, args...)
		return baseStream(ctx, args...)
	}}
	var ready map[string]any
	var handled []string
	var rejected []channel.RejectedEvent
	err := a.RunReceiver(ctx, testConfig(), channel.ReceiverOptions{
		Ready: func(detail map[string]any) { ready = detail },
		Handle: func(ctx context.Context, e core.NormalizedEvent) error {
			handled = append(handled, e.ProviderMessageID)
			return nil
		},
		Reject: func(ctx context.Context, r channel.RejectedEvent) error { rejected = append(rejected, r); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	joinedArgs := strings.Join(streamArgs, " ")
	for _, want := range []string{"event +listen-im", "--kind all-group", "--events message", "--profile corp1:user1"} {
		if !strings.Contains(joinedArgs, want) {
			t.Fatalf("receiver missing %q in %q", want, joinedArgs)
		}
	}
	if ready == nil {
		t.Fatal("readiness was never reported from the dws marker")
	}
	if ready["event_count"] != 2 || ready["bus_pid"] != 4242 {
		t.Fatalf("ready detail not parsed: %+v", ready)
	}
	if len(handled) != 2 || handled[0] != "m1" || handled[1] != "m2" {
		t.Fatalf("events handled: %+v", handled)
	}
	// An unsupported event is isolated, not turned into an empty message.
	if len(rejected) != 1 || rejected[0].Reason != "invalid_input" {
		t.Fatalf("unsupported event was not isolated: %+v", rejected)
	}
}

// A receiver that never sees the ready marker does not claim to be listening,
// and a handler failure stops the loop instead of dropping the event.
func TestReceiverDoesNotClaimReadinessAndStopsOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	line := `{"event_type":"user_im_message_receive_group","event_id":"evt1","event_corp_id":"corp1","data":"{\"msgId\":\"m1\",\"conversationId\":\"cid:group1\",\"senderId\":\"alice\",\"text\":{\"content\":\"内容\"}}"}` + "\n"
	a := &Adapter{Timeout: time.Second, Stream: fakeStream(line, "[event] starting subscription\n", 0, nil)}
	readyCalled := false
	err := a.RunReceiver(ctx, testConfig(), channel.ReceiverOptions{
		Ready:  func(map[string]any) { readyCalled = true },
		Handle: func(ctx context.Context, e core.NormalizedEvent) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if readyCalled {
		t.Fatal("readiness was claimed without the dws ready marker")
	}
	failing := &Adapter{Timeout: time.Second, Stream: fakeStream(line+line, "[event] ready event_count=1\n", 0, nil)}
	calls := 0
	err = failing.RunReceiver(ctx, testConfig(), channel.ReceiverOptions{
		Handle: func(ctx context.Context, e core.NormalizedEvent) error {
			calls++
			return core.Fail("internal", "database write failed")
		},
	})
	if core.ErrorCode(err) != "internal" {
		t.Fatalf("a write failure was swallowed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the loop continued after a failed write: %d calls", calls)
	}
	// A receiver requires a handler; there is no silent discard mode.
	if err = a.RunReceiver(ctx, testConfig(), channel.ReceiverOptions{}); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("receiver ran without a handler: %v", err)
	}
}

func TestReceiverOpensDirectSubscriptionAndRoutesOnlyTheOwner(t *testing.T) {
	cfg := testConfig()
	cfg.Conversations = append(cfg.Conversations, "owner-user-id")
	cfg.DirectConversations = []string{"owner-user-id"}
	cfg.Identity.ExpectedUserID = "owner-user-id"
	ownerLine := `{"event_type":"user_im_message_receive_single","event_id":"direct-1","event_corp_id":"corp1","data":"{\"msgId\":\"dm-1\",\"conversationId\":\"cid:real-direct\",\"senderStaffId\":\"owner-user-id\",\"text\":{\"content\":\"帮我查一下\"}}"}` + "\n"
	otherLine := `{"event_type":"user_im_message_receive_single","event_id":"direct-2","event_corp_id":"corp1","data":"{\"msgId\":\"dm-2\",\"conversationId\":\"cid:other-direct\",\"senderStaffId\":\"someone-else\",\"text\":{\"content\":\"不属于 owner\"}}"}` + "\n"
	a := &Adapter{Timeout: time.Second, Stream: fakeStream(ownerLine+otherLine, "[event] ready event_count=2\n", 0, nil)}
	var handled []core.NormalizedEvent
	if err := a.runReceiverKind(context.Background(), cfg, channel.ReceiverOptions{Handle: func(_ context.Context, event core.NormalizedEvent) error {
		handled = append(handled, event)
		return nil
	}}, "all-direct"); err != nil {
		t.Fatal(err)
	}
	if len(handled) != 1 || handled[0].ProviderMessageID != "dm-1" || handled[0].ConversationID != "owner-user-id" {
		t.Fatalf("direct owner routing = %+v", handled)
	}

	var streamArgs []string
	a.Stream = func(ctx context.Context, args ...string) (io.ReadCloser, io.ReadCloser, func() error, error) {
		streamArgs = append([]string{}, args...)
		return fakeStream("", "[event] ready\n", 0, nil)(ctx, args...)
	}
	if err := a.RunReceiver(context.Background(), cfg, channel.ReceiverOptions{Handle: func(context.Context, core.NormalizedEvent) error { return nil }}); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("mixed DWS subscription must be rejected: %v", err)
	}
	if len(streamArgs) != 0 {
		t.Fatal("mixed receiver opened a DWS stream")
	}
	cfg.Conversations = []string{"owner-user-id"}
	if err := a.RunReceiver(context.Background(), cfg, channel.ReceiverOptions{Handle: func(context.Context, core.NormalizedEvent) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(streamArgs, " ")
	for _, want := range []string{"event +listen-im", "--kind all-direct", "--profile corp1:user1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("direct receiver missing %q in %q", want, joined)
		}
	}
}

// An oversized event line is isolated and stops the stream, because the reader
// cannot resynchronize past it without risking a partial event.
func TestOversizedEventIsIsolated(t *testing.T) {
	ctx := context.Background()
	huge := `{"event_type":"user_im_message_receive_group","event_id":"big","event_corp_id":"corp1","data":"` + strings.Repeat("x", maxEventBytes+16) + `"}` + "\n"
	a := &Adapter{Timeout: time.Second, Stream: fakeStream(huge, "[event] ready\n", 0, nil)}
	var rejected []channel.RejectedEvent
	err := a.RunReceiver(ctx, testConfig(), channel.ReceiverOptions{
		Handle: func(ctx context.Context, e core.NormalizedEvent) error {
			t.Fatal("oversized event was handled")
			return nil
		},
		Reject: func(ctx context.Context, r channel.RejectedEvent) error { rejected = append(rejected, r); return nil },
	})
	if core.ErrorCode(err) != "unavailable" {
		t.Fatalf("oversized line did not stop the stream: %v", err)
	}
	if len(rejected) != 1 || rejected[0].Reason != "oversized_event" {
		t.Fatalf("oversized line not isolated: %+v", rejected)
	}
}

// Unsupported transports are refused before any platform call.
func TestSendIsRefusedUntilVerified(t *testing.T) {
	a := New()
	out, err := a.Send(context.Background(), testConfig(), channel.SendRequest{ConversationID: "cid:group1", Content: "hi"})
	if core.ErrorCode(err) != "unavailable" {
		t.Fatalf("send was attempted: %v", err)
	}
	if out.State != "blocked" {
		t.Fatalf("send state %q", out.State)
	}
}

func TestProbeVerifiesConfiguredBotAndSendUsesOwnerDirectTarget(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.DeliveryRobotCode = "robot-1"
	var calls [][]string
	a := &Adapter{Timeout: time.Second, Run: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{}, args...))
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "profile list") {
			return envelope(`{"currentProfile":"corp1:user1","profiles":[{"profile":"corp1:user1","corpId":"corp1","userId":"user1"}]}`), nil
		}
		if strings.Contains(joined, "chat bot search") {
			return envelope(`{"items":[{"robotCode":"robot-1"}]}`), nil
		}
		return envelope(`{"succeededCount":1,"failedCount":0,"processQueryKey":"receipt"}`), nil
	}}
	caps, err := a.ProbeCapabilities(context.Background(), cfg)
	if err != nil || !caps.Verified["send"] {
		t.Fatalf("send capability: %+v %v", caps, err)
	}
	result, err := a.Send(context.Background(), cfg, channel.SendRequest{ConversationID: "owner-user-id", Transport: "bot_dm", Content: "result"})
	if err != nil || result.State != "accepted" {
		t.Fatalf("send: %+v %v", result, err)
	}
	joined := strings.Join(calls[len(calls)-1], " ")
	for _, want := range []string{"chat +messages-send", "--as bot", "--robot-code robot-1", "--users owner-user-id", "--markdown result", "--profile corp1:user1", "--yes"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
}

func TestReactionUsesExactMessageAndConversation(t *testing.T) {
	var calls [][]string
	a := &Adapter{Timeout: time.Second, Run: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{}, args...))
		return envelope(`{"updated":true}`), nil
	}}
	req := channel.ReactionRequest{ConversationID: "cid:group1", MessageID: "msg-1", Emoji: "暗中观察"}
	if err := a.AddReaction(context.Background(), testConfig(), req); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveReaction(context.Background(), testConfig(), req); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("reaction calls = %d", len(calls))
	}
	for i, action := range []string{"add-emoji", "remove-emoji"} {
		joined := strings.Join(calls[i], " ")
		for _, want := range []string{"chat message " + action, "--conversation-id cid:group1", "--message-id msg-1", "--emoji 暗中观察", "--profile corp1:user1"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("reaction missing %q in %q", want, joined)
			}
		}
	}
}

func TestProbeVerifiesExplicitEnterpriseBotWithDryRun(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.DeliveryRobotCode = "enterprise-app"
	var previewed bool
	a := &Adapter{Timeout: time.Second, Run: func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "profile list"):
			return envelope(`{"currentProfile":"corp1:user1","profiles":[{"profile":"corp1:user1","corpId":"corp1","userId":"user1"}]}`), nil
		case strings.Contains(joined, "chat bot search"):
			return envelope(`{"items":[]}`), nil
		case strings.Contains(joined, "chat +messages-send"):
			previewed = true
			for _, want := range []string{"--as bot", "--robot-code enterprise-app", "--users user1", "--dry-run"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("missing %q in %q", want, joined)
				}
			}
			return []byte(`{"dry_run":true,"executed":false,"actionCount":1}`), nil
		default:
			t.Fatalf("unexpected call: %s", joined)
			return nil, nil
		}
	}}
	caps, err := a.ProbeCapabilities(context.Background(), cfg)
	if err != nil || !caps.Verified["send"] || !previewed {
		t.Fatalf("dry-run send capability: %+v previewed=%v err=%v", caps, previewed, err)
	}
}

func TestBotSendNeverRetriesAnUnclearCall(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.DeliveryRobotCode = "robot-1"
	calls := 0
	a := &Adapter{Timeout: time.Second, Run: func(context.Context, ...string) ([]byte, error) {
		calls++
		return nil, context.DeadlineExceeded
	}}
	result, err := a.Send(context.Background(), cfg, channel.SendRequest{ConversationID: "owner", Transport: "bot_dm", Content: "result"})
	if err == nil || result.State != "unknown" || calls != 1 {
		t.Fatalf("unclear send was retried or misreported: calls=%d result=%+v err=%v", calls, result, err)
	}
}

func TestBotSendRequiresExplicitPlatformSuccess(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.DeliveryRobotCode = "robot-1"
	a := &Adapter{Timeout: time.Second, Run: func(context.Context, ...string) ([]byte, error) {
		return envelope(`{"items":[]}`), nil
	}}
	result, err := a.Send(context.Background(), cfg, channel.SendRequest{ConversationID: "owner", Transport: "bot_dm", Content: "result"})
	if err != nil || result.State != "unknown" {
		t.Fatalf("unconfirmed platform result was accepted: %+v %v", result, err)
	}
}

func TestBotSendAcceptsCurrentDWSCompositeReceipt(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.DeliveryRobotCode = "robot-1"
	a := &Adapter{Timeout: time.Second, Run: func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"ok":true,"identity":{},"tool":"chat.send","result":{"success":true,"errorCode":0,"errorMessage":"","result":{"processQueryKey":"receipt","invalidStaffIdList":[],"flowControlledStaffIdList":[]}}}`), nil
	}}
	result, err := a.Send(context.Background(), cfg, channel.SendRequest{ConversationID: "owner", Transport: "bot_dm", Content: "result"})
	if err != nil || result.State != "accepted" {
		t.Fatalf("current dws receipt was not accepted: %+v %v", result, err)
	}
}

// The adapter normalizes the platform's varied time encodings into RFC3339 UTC so
// ordering stays comparable across history and live events.
func TestTimeNormalization(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2026-09-14T08:00:00Z", "2026-09-14T08:00:00Z"},
		{"2026-09-14 08:00:00", "2026-09-14T08:00:00Z"},
		{"1757843400000", "2025-09-14T09:50:00Z"},
		{"", ""},
		{"not a time", ""},
	} {
		if got := normalizeTime(tc.in); got != tc.want {
			t.Fatalf("normalizeTime(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
	var number any
	if err := json.Unmarshal([]byte(`1757843400000`), &number); err != nil {
		t.Fatal(err)
	}
	if eventTime(number, 0) == "" {
		t.Fatal("epoch milliseconds must be accepted from JSON numbers")
	}
}
