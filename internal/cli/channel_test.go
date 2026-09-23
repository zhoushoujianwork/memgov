package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/channel/dingtalkapp"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// stubAdapter stands in for dws so the whole command surface can be exercised
// without a real account, a real subscription or any network call.
type stubAdapter struct {
	caps    core.Capabilities
	windows []channel.WindowResult
	window  int
	events  []core.NormalizedEvent
}

func (s *stubAdapter) Name() string { return "stub" }
func (s *stubAdapter) ProbeCapabilities(context.Context, channel.Config) (core.Capabilities, error) {
	return s.caps, nil
}
func (s *stubAdapter) ReadWindow(context.Context, channel.Config, channel.Window) (channel.WindowResult, error) {
	if s.window >= len(s.windows) {
		return channel.WindowResult{Complete: true}, nil
	}
	out := s.windows[s.window]
	s.window++
	return out, nil
}
func (s *stubAdapter) RunReceiver(ctx context.Context, _ channel.Config, opts channel.ReceiverOptions) error {
	if opts.Ready != nil {
		opts.Ready(map[string]any{"marker": "[event] ready", "event_count": len(s.events)})
	}
	for _, e := range s.events {
		if err := opts.Handle(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
func (s *stubAdapter) Send(context.Context, channel.Config, channel.SendRequest) (channel.SendResult, error) {
	return channel.SendResult{State: "blocked"}, channel.Unsupported("stub", "send")
}

func TestApplicationAdapterIsSharedByReceiverAndRuntimeWorkers(t *testing.T) {
	a := &app{}
	c := core.Channel{Kind: core.ChannelDingTalkApp}
	first, err := a.adapterFor(c)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.adapterFor(c)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("application receiver and runtime worker received different adapters")
	}
}

func TestAdapterRegistrySupportsASecondPlatformWithoutCLIChanges(t *testing.T) {
	a := &app{adapterRegistry: channel.NewRegistry()}
	var constructed int
	fake := &stubAdapter{}
	if err := a.adapterRegistry.Register("matrix", func() channel.Adapter {
		constructed++
		return fake
	}); err != nil {
		t.Fatal(err)
	}
	one, err := a.adapterFor(core.Channel{Kind: "matrix"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := a.adapterFor(core.Channel{Kind: "matrix"})
	if err != nil {
		t.Fatal(err)
	}
	if one != fake || two != fake || one != two || constructed != 1 {
		t.Fatalf("custom platform adapter was not shared: one=%T two=%T constructed=%d", one, two, constructed)
	}
}

// invokeWith runs the CLI with an injected adapter, which is the only way a
// platform call happens in a test.
func invokeWith(t *testing.T, adapter channel.Adapter, home, input string, args ...string) (int, map[string]any) {
	t.Helper()
	var out, errOut bytes.Buffer
	a := &app{in: bytes.NewBufferString(input), out: &out, errOut: &errOut, requestID: core.NewID(), dwsAdapter: adapter}
	code := run(context.Background(), a, append([]string{"--home", home}, args...))
	var value map[string]any
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatalf("invalid JSON: %v: %s stderr=%s", err, out.String(), errOut.String())
	}
	return code, value
}

// invokeWithApp injects the stub as the application-bot adapter, so the app path
// is exercised without real application credentials.
func invokeWithApp(t *testing.T, adapter channel.Adapter, home, input string, args ...string) (int, map[string]any) {
	t.Helper()
	var out, errOut bytes.Buffer
	a := &app{in: bytes.NewBufferString(input), out: &out, errOut: &errOut, requestID: core.NewID(), appAdapter: adapter}
	code := run(context.Background(), a, append([]string{"--home", home}, args...))
	var value map[string]any
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatalf("invalid JSON: %v: %s stderr=%s", err, out.String(), errOut.String())
	}
	return code, value
}

func data(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	out, ok := value["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data object: %+v", value)
	}
	return out
}

func stubEvent(id, body string) core.NormalizedEvent {
	return core.NormalizedEvent{Kind: core.EventMessage, Adapter: "stub", ParseVersion: "stub/1", Origin: "history",
		ProviderMessageID: id, ConversationID: "cid:group1", Sender: core.Sender{IDType: "union_id", IDValue: "alice", DisplayName: "Alice"},
		Body: body, SentAt: "2026-09-14T08:10:00Z", EventAt: "2026-09-14T08:10:00Z"}
}

func TestChannelRegistrationStaysOfflineAndDefaultsToDraft(t *testing.T) {
	home := t.TempDir()
	invoke(t, home, "", "init")
	add := `{"name":"dws-main","kind":"dws_personal","identity":{"profile":"corp1:user1","expected_corp_id":"corp1","expected_user_id":"user1"},` +
		`"credential_ref":"keychain://memgov/dws","route":{"conversation_id":"cid:group1","conversation_type":"group"}}`
	code, value := invoke(t, home, add, "channel", "add", "--input", "-")
	if code != 0 {
		t.Fatalf("channel add: %d %+v", code, value)
	}
	stored := data(t, value)
	if stored["auth_namespace"] != "dws:corp1:user1" || stored["id_namespace"] != "dws:corp1" {
		t.Fatalf("namespaces: %+v", stored)
	}
	routes := stored["routes"].([]any)
	route := routes[0].(map[string]any)
	if route["send_policy"] != "draft_only" || route["audience_policy"] != "local_private" || route["approval_display"] != "display_only" {
		t.Fatalf("a new route was not restrictive by default: %+v", route)
	}
	// An inline secret is refused, so SQLite never holds one.
	bad := `{"name":"dws-bad","kind":"dws_personal","identity":{"profile":"corp1:user1","expected_corp_id":"corp1","expected_user_id":"user1"},"credential_ref":"token=abc"}`
	if code, _ = invoke(t, home, bad, "channel", "add", "--input", "-"); code != 2 {
		t.Fatalf("an inline secret was accepted: %d", code)
	}
	code, value = invoke(t, home, "", "channel", "plan", "dws-main")
	if code != 0 {
		t.Fatalf("plan: %+v", value)
	}
	plan := data(t, value)
	if plan["creates_subscription"] != false || plan["sends_message"] != false {
		t.Fatalf("plan must state it changes nothing: %+v", plan)
	}
	code, value = invoke(t, home, "", "channel", "doctor", "dws-main")
	if code != 0 {
		t.Fatalf("doctor: %+v", value)
	}
	report := data(t, value)
	if report["online_checked"] != false {
		t.Fatal("doctor must not claim an online check")
	}
	// Collection is refused before a probe verified the capability.
	if code, _ = invokeWith(t, &stubAdapter{}, home, "", "channel", "pull", "dws-main", "--conversation", "cid:group1",
		"--start", "2026-09-14T08:00:00Z", "--end", "2026-09-14T09:00:00Z"); code != 5 {
		t.Fatalf("pull ran without a verified capability: %d", code)
	}
}

// A backfill window that was truncated is reported as a gap through the CLI, and
// coverage only advances over a window that was actually read to the end.
func TestChannelPullReportsGapsThroughTheCLI(t *testing.T) {
	home := t.TempDir()
	invoke(t, home, "", "init")
	add := `{"name":"dws-main","kind":"dws_personal","identity":{"profile":"corp1:user1","expected_corp_id":"corp1","expected_user_id":"user1"},` +
		`"route":{"conversation_id":"cid:group1","conversation_type":"group"}}`
	invoke(t, home, add, "channel", "add", "--input", "-")
	adapter := &stubAdapter{caps: core.Capabilities{Verified: map[string]bool{"history": true}, Unverified: []string{"send"}},
		windows: []channel.WindowResult{
			{Events: []core.NormalizedEvent{stubEvent("m1", "第一条")}, Complete: false, StopReason: "page_or_item_cap_reached", NextCursor: "cursor-2"},
			{Events: []core.NormalizedEvent{stubEvent("m2", "第二条")}, Complete: true},
		}}
	code, value := invokeWith(t, adapter, home, "", "channel", "probe", "dws-main")
	if code != 0 {
		t.Fatalf("probe: %+v", value)
	}
	if caps := data(t, value)["capabilities"].(map[string]any); caps["verified"].(map[string]any)["send"] == true {
		t.Fatal("probe claimed send")
	}
	code, value = invokeWith(t, adapter, home, "", "channel", "pull", "dws-main", "--conversation", "cid:group1",
		"--start", "2026-09-14T08:00:00Z", "--end", "2026-09-14T09:00:00Z")
	if code != 0 {
		t.Fatalf("partial pull: %+v", value)
	}
	partial := data(t, value)
	if partial["complete"] != false || partial["stop_reason"] == "" {
		t.Fatalf("a truncated window looked complete: %+v", partial)
	}
	if partial["watermark"].(map[string]any)["covered_until"] != nil {
		t.Fatalf("coverage advanced over a partial window: %+v", partial["watermark"])
	}
	code, value = invokeWith(t, adapter, home, "", "channel", "pull", "dws-main", "--conversation", "cid:group1",
		"--start", "2026-09-14T09:00:00Z", "--end", "2026-09-14T10:00:00Z")
	if code != 0 {
		t.Fatalf("complete pull: %+v", value)
	}
	if data(t, value)["complete"] != true {
		t.Fatalf("a fully read window was not complete: %+v", data(t, value))
	}
	code, value = invokeWith(t, adapter, home, "", "channel", "status", "dws-main")
	if code != 0 {
		t.Fatalf("status: %+v", value)
	}
	conversations := data(t, value)["conversations"].([]any)
	first := conversations[0].(map[string]any)
	if len(first["gaps"].([]any)) == 0 {
		t.Fatalf("the earlier gap disappeared: %+v", first)
	}
	// Both messages are readable, and the recorded sender identity travelled.
	code, list := invokeWith(t, adapter, home, "", "message", "list", "dws-main", "--conversation", "cid:group1")
	if code != 0 {
		t.Fatalf("message list: %+v", list)
	}
	if len(list["data"].([]any)) != 2 {
		t.Fatalf("messages: %+v", list["data"])
	}
	code, queried := invokeWith(t, adapter, home, "", "message", "query", "dws-main", "--query", "Alice", "--since", "2026-09-14T08:00:00Z", "--until", "2026-09-14T09:00:00Z")
	if code != 0 {
		t.Fatalf("message query: %+v", queried)
	}
	queryResult := data(t, queried)
	if len(queryResult["messages"].([]any)) != 2 || len(queryResult["coverage"].([]any)) != 1 {
		t.Fatalf("message query omitted observed messages or coverage: %+v", queryResult)
	}
	// A window without a range continues from the watermark with an overlap.
	if code, value = invokeWith(t, adapter, home, "", "channel", "pull", "dws-main", "--conversation", "cid:group1"); code != 0 {
		t.Fatalf("watermark pull: %+v", value)
	}
	if data(t, value)["start_at"].(string) >= data(t, value)["end_at"].(string) {
		t.Fatalf("watermark window is not a forward range: %+v", data(t, value))
	}
	// A pull without a conversation is refused rather than guessing one.
	if code, _ = invokeWith(t, adapter, home, "", "channel", "pull", "dws-main"); code != 2 {
		t.Fatalf("pull guessed a conversation: %d", code)
	}
}

// A live session through the CLI holds the lease, counts only committed events
// and releases the lease afterwards.
func TestChannelRunCommitsBeforeCountingAndReleasesTheLease(t *testing.T) {
	home := t.TempDir()
	invoke(t, home, "", "init")
	add := `{"name":"dws-main","kind":"dws_personal","identity":{"profile":"corp1:user1","expected_corp_id":"corp1","expected_user_id":"user1"},` +
		`"route":{"conversation_id":"cid:group1","conversation_type":"group"}}`
	invoke(t, home, add, "channel", "add", "--input", "-")
	adapter := &stubAdapter{caps: core.Capabilities{Verified: map[string]bool{"receive": true}},
		events: []core.NormalizedEvent{stubEvent("m1", "第一条"), stubEvent("m1", "第一条")}}
	invokeWith(t, adapter, home, "", "channel", "probe", "dws-main")
	code, value := invokeWith(t, adapter, home, "", "channel", "run", "dws-main", "--lease-ttl", "1m")
	if code != 0 {
		t.Fatalf("run: %+v", value)
	}
	out := data(t, value)
	if out["listening"] != true {
		t.Fatalf("readiness was not reported: %+v", out)
	}
	if out["applied"] != float64(1) || out["duplicates"] != float64(1) {
		t.Fatalf("a redelivered event was double-counted: %+v", out)
	}
	if !strings.Contains(out["note"].(string), "not transactional") {
		t.Fatalf("the note must state delivery is not loss-free: %+v", out["note"])
	}
	// A second session can start, which proves the lease was released.
	if code, value = invokeWith(t, adapter, home, "", "channel", "run", "dws-main", "--lease-ttl", "1m"); code != 0 {
		t.Fatalf("the lease was not released: %d %+v", code, value)
	}
}

// Offline import keeps valid evidence and reports malformed events.
func TestIngestEvidenceThroughTheCLI(t *testing.T) {
	home := t.TempDir()
	invoke(t, home, "", "init")
	add := `{"name":"dws-main","kind":"dws_personal","identity":{"profile":"corp1:user1","expected_corp_id":"corp1","expected_user_id":"user1"},` +
		`"route":{"conversation_id":"cid:group1","conversation_type":"group"}}`
	invoke(t, home, add, "channel", "add", "--input", "-")
	link := `{"from":{"id_type":"union_id","id_value":"alice"},"to":{"id_type":"union_id","id_value":"alice"}}`
	if code, value := invoke(t, home, link, "message", "identity", "link", "dws-main", "--basis", "operator_confirmed", "--input", "-"); code != 0 {
		t.Fatalf("identity link: %+v", value)
	}
	lines := core.JSON(stubEvent("m1", "导入的内容")) + "\n" + `{"kind":"message"` + "\n"
	code, value := invokeWith(t, &stubAdapter{}, home, lines, "channel", "ingest", "dws-main", "--file", "-")
	if code != 0 {
		t.Fatalf("ingest: %+v", value)
	}
	out := data(t, value)
	if out["applied"] != float64(1) || out["rejected"] != float64(1) {
		t.Fatalf("import results: %+v", out)
	}
	// A rejected line stays visible instead of being skipped silently.
	code, value = invoke(t, home, "", "message", "inbox", "list", "dws-main")
	if code != 0 {
		t.Fatalf("inbox: %+v", value)
	}
	code, messages := invoke(t, home, "", "message", "list", "dws-main", "--conversation", "cid:group1")
	if code != 0 || len(messages["data"].([]any)) != 1 {
		t.Fatal(messages)
	}

}

// A route policy change needs the exact expected version and a reason, so a
// concurrent edit cannot silently widen an audience.
func TestRouteUpdateRequiresExpectedVersionAndReason(t *testing.T) {
	home := t.TempDir()
	invoke(t, home, "", "init")
	add := `{"name":"dws-main","kind":"dws_personal","identity":{"profile":"corp1:user1","expected_corp_id":"corp1","expected_user_id":"user1"},` +
		`"route":{"conversation_id":"cid:group1","conversation_type":"group"}}`
	_, value := invoke(t, home, add, "channel", "add", "--input", "-")
	route := data(t, value)["routes"].([]any)[0].(map[string]any)
	id := route["id"].(string)
	change := `{"conversation_id":"cid:group1","audience_policy":"conversation"}`
	if code, _ := invoke(t, home, change, "channel", "route", "update", id, "--input", "-", "--expected-version", "1"); code != 2 {
		t.Fatal("a policy change was accepted without a reason")
	}
	if code, _ := invoke(t, home, change, "channel", "route", "update", id, "--input", "-", "--expected-version", "9", "--reason", "扩大到会话受众"); code != 3 {
		t.Fatal("a stale expected version was accepted")
	}
	code, updated := invoke(t, home, change, "channel", "route", "update", id, "--input", "-", "--expected-version", "1", "--reason", "扩大到会话受众")
	if code != 0 {
		t.Fatalf("route update: %+v", updated)
	}
	next := data(t, updated)["route"].(map[string]any)
	if next["version"] != float64(2) || next["audience_key"] != "conversation:"+route["channel_id"].(string)+":cid:group1" {
		t.Fatalf("audience key was not recomputed: %+v", next)
	}
	// Repointing the binding at another conversation is refused outright.
	move := `{"conversation_id":"cid:other","audience_policy":"conversation"}`
	if code, _ := invoke(t, home, move, "channel", "route", "update", id, "--input", "-", "--expected-version", "2", "--reason", "换会话"); code == 0 {
		t.Fatal("a route was repointed at another conversation")
	}
}

// The app-bot channel is served by its own adapter, never by the personal dws
// one. Without a probe it has no verified receive capability, and it says so
// instead of attempting a connection hopefully.
func TestAppChannelUsesItsOwnAdapterAndRefusesUnverifiedReceive(t *testing.T) {
	home := t.TempDir()
	invoke(t, home, "", "init")
	add := `{"name":"bot-main","kind":"dingtalk_app","identity":{"expected_corp_id":"corp1","client_id":"cli-1","robot_code":"bot-1"},` +
		`"credential_ref":"keychain://memgov/bot","route":{"conversation_id":"cid:group2","conversation_type":"group"}}`
	if code, value := invoke(t, home, add, "channel", "add", "--input", "-"); code != 0 {
		t.Fatalf("app channel add: %d %+v", code, value)
	}
	// The dws stub is injected deliberately: an app channel must not be served by
	// it, so this run has to fail on the app path rather than succeed on the dws one.
	code, value := invokeWith(t, &stubAdapter{caps: core.Capabilities{Verified: map[string]bool{"receive": true}}}, home, "", "channel", "run", "bot-main")
	if code != 5 {
		t.Fatalf("the app receiver did not report unavailability: %d %+v", code, value)
	}
	if !strings.Contains(core.JSON(value["error"]), "verified receive capability") {
		t.Fatalf("the reason was not stated: %+v", value["error"])
	}
	// A history backfill is refused for the app kind even after a probe, because a
	// bot has no verified history read and the personal account is not substituted.
	probe := &stubAdapter{caps: core.Capabilities{Verified: map[string]bool{"receive": true, "history": true}}}
	if code, value = invokeWithApp(t, probe, home, "", "channel", "probe", "bot-main"); code != 0 {
		t.Fatalf("probe: %+v", value)
	}
	code, value = invokeWithApp(t, &dingtalkapp.Adapter{}, home, "",
		"channel", "pull", "bot-main", "--conversation", "cid:group2",
		"--start", "2025-09-01T00:00:00Z", "--end", "2025-09-02T00:00:00Z")
	if code != 5 || !strings.Contains(core.JSON(value["error"]), "history backfill") {
		t.Fatalf("an app channel backfilled history: %d %+v", code, value)
	}
}
