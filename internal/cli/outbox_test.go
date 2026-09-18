package cli

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// replyFixture registers a channel, formalizes a memory, publishes it to the
// conversation and opens a trusted context, which is the state a reply needs.
func replyFixture(t *testing.T, home string) (memoryID, contextID string) {
	t.Helper()
	invoke(t, home, "", "init")
	add := `{"name":"bot-main","kind":"dingtalk_app","identity":{"expected_corp_id":"corp1","client_id":"cli-1","robot_code":"bot-1"},` +
		`"credential_ref":"keychain://memgov/bot","route":{"conversation_id":"cid:group2","conversation_type":"group"}}`
	if code, value := invoke(t, home, add, "channel", "add", "--input", "-"); code != 0 {
		t.Fatalf("channel add: %+v", value)
	}
	memoryID = formalMemory(t, home)
	if code, value := invoke(t, home, "", "audience", "publish", "bot-main", memoryID, "--conversation", "cid:group2", "--reason", "群里问到该流程"); code != 0 {
		t.Fatalf("publish: %+v", value)
	}
	open := `{"conversation_id":"cid:group2","sender":{"id_type":"union_id","id_value":"alice"},"query":"上线前要注意什么","trigger_message_id":"m1"}`
	code, opened := invoke(t, home, open, "reply", "open", "bot-main", "--input", "-")
	if code != 0 {
		t.Fatalf("reply open: %+v", opened)
	}
	return memoryID, data(t, opened)["id"].(string)
}

// The whole reply path runs offline: a draft is stored, displayed with its target
// and evidence, and refuses to be dispatched while sending is not enabled.
func TestReplyDraftIsDisplayedButNeverSent(t *testing.T) {
	home := t.TempDir()
	memoryID, contextID := replyFixture(t, home)
	draft := core.JSON(map[string]any{"request_context_id": contextID,
		"content": "上线前必须先备份数据库。", "citations": []string{memoryID}})
	code, created := invoke(t, home, draft, "reply", "draft", "--input", "-")
	if code != 0 {
		t.Fatalf("reply draft: %+v", created)
	}
	out := data(t, created)
	if out["sent"] != false {
		t.Fatalf("drafting claimed a send: %+v", out)
	}
	id := out["draft"].(map[string]any)["id"].(string)
	code, previewed := invoke(t, home, "", "outbox", "preview", id)
	if code != 0 {
		t.Fatalf("preview: %+v", previewed)
	}
	p := data(t, previewed)
	approval := p["approval"].(map[string]any)
	if approval["mode"] != "display_only" || approval["status"] != "not_evaluated" {
		t.Fatalf("the approval contract changed: %+v", approval)
	}
	if p["target"].(map[string]any)["conversation_id"] != "cid:group2" {
		t.Fatalf("target: %+v", p["target"])
	}
	if len(p["citations"].([]any)) != 1 {
		t.Fatalf("citations were not displayed: %+v", p["citations"])
	}
	if p["sendable"] != false {
		t.Fatalf("a draft_only route produced a sendable draft: %+v", p["checks"])
	}
	if !strings.Contains(p["note"].(string), "sends nothing") {
		t.Fatalf("note: %+v", p["note"])
	}
	digest := p["display_digest"].(string)
	// Dispatching with the correct digest is still refused, because display is
	// not permission: the route is draft_only and send was never verified.
	code, refused := invoke(t, home, "", "outbox", "dispatch", id, "--expected-digest", digest)
	if code != 6 {
		t.Fatalf("a draft_only draft was dispatched: %d %+v", code, refused)
	}
	// The draft did not move, and no delivery attempt was recorded.
	code, after := invoke(t, home, "", "outbox", "preview", id)
	if code != 0 {
		t.Fatalf("preview after refusal: %+v", after)
	}
	if data(t, after)["draft"].(map[string]any)["state"] != "draft" {
		t.Fatalf("a refused dispatch moved the state: %+v", data(t, after)["draft"])
	}
	if _, ok := data(t, after)["attempts"]; ok {
		t.Fatalf("a refused dispatch recorded an attempt: %+v", data(t, after)["attempts"])
	}
	// A dispatch without the digest is refused before anything else.
	if code, _ = invoke(t, home, "", "outbox", "dispatch", id); code != 2 {
		t.Fatalf("a dispatch without a digest was accepted: %d", code)
	}
	// Listing never sends either.
	code, listed := invoke(t, home, "", "outbox", "list", "bot-main")
	if code != 0 {
		t.Fatalf("outbox list: %+v", listed)
	}
	if !strings.Contains(data(t, listed)["note"].(string), "never sends") {
		t.Fatalf("list note: %+v", data(t, listed)["note"])
	}
}

// A reply may not cite material the conversation is not allowed to see, and the
// context alone decides the audience of a recall.
func TestReplyRecallAndCitationsStayInsideTheContextAudience(t *testing.T) {
	home := t.TempDir()
	memoryID, contextID := replyFixture(t, home)
	code, recalled := invoke(t, home, "", "reply", "recall", contextID, "上线前要注意什么")
	if code != 0 {
		t.Fatalf("reply recall: %+v", recalled)
	}
	// Revoking the publication makes both the recall and a new citation refuse.
	if code, value := invoke(t, home, "", "audience", "unpublish", "bot-main", memoryID, "--conversation", "cid:group2", "--reason", "收回"); code != 0 {
		t.Fatalf("unpublish: %+v", value)
	}
	draft := core.JSON(map[string]any{"request_context_id": contextID,
		"content": "上线前必须先备份数据库。", "citations": []string{memoryID}})
	code, refused := invoke(t, home, draft, "reply", "draft", "--input", "-")
	if code != 6 {
		t.Fatalf("an unpublished memory was cited: %d %+v", code, refused)
	}
	// An unknown context is not found rather than answered with an empty result.
	if code, _ = invoke(t, home, "", "reply", "recall", "00000000-0000-4000-8000-000000000000", "问题"); code != 4 {
		t.Fatalf("an unknown context was answered: %d", code)
	}
}

// Once sending is deliberately enabled and verified, a dispatch records the
// attempt, and an unknown outcome stays unknown instead of being retried.
func TestDispatchRecordsAttemptAndKeepsUnknownDeliveryHonest(t *testing.T) {
	home := t.TempDir()
	memoryID, contextID := replyFixture(t, home)
	// Sending is enabled on the route and the capability is recorded by a probe
	// against the stub, so nothing here contacts a platform. This happens before
	// the draft is produced, because a policy change stales earlier drafts.
	_, shown := invoke(t, home, "", "channel", "show", "bot-main")
	route := data(t, shown)["routes"].([]any)[0].(map[string]any)
	enable := `{"conversation_id":"cid:group2","send_policy":"dispatch_only"}`
	if code, value := invoke(t, home, enable, "channel", "route", "update", route["id"].(string),
		"--input", "-", "--expected-version", "1", "--reason", "开启本地操作者显式发送"); code != 0 {
		t.Fatalf("enable dispatch: %+v", value)
	}
	adapter := &stubAdapter{caps: core.Capabilities{Verified: map[string]bool{"send": true}}}
	if code, value := invokeWithApp(t, adapter, home, "", "channel", "probe", "bot-main"); code != 0 {
		t.Fatalf("probe: %+v", value)
	}
	// The context was opened under route version 1, so the reply flow re-opens one
	// under the current version rather than replying against a stale policy.
	open := `{"conversation_id":"cid:group2","sender":{"id_type":"union_id","id_value":"alice"},"query":"上线前要注意什么"}`
	code, reopened := invoke(t, home, open, "reply", "open", "bot-main", "--input", "-")
	if code != 0 {
		t.Fatalf("reply open: %+v", reopened)
	}
	// The stale context from the fixture is refused, which is the check under test.
	stale := core.JSON(map[string]any{"request_context_id": contextID, "content": "旧策略下的回复"})
	if code, refused := invoke(t, home, stale, "reply", "draft", "--input", "-"); code != 3 {
		t.Fatalf("a draft was produced against a stale route version: %d %+v", code, refused)
	}
	draft := core.JSON(map[string]any{"request_context_id": data(t, reopened)["id"].(string),
		"content": "上线前必须先备份数据库。", "citations": []string{memoryID}})
	code, created := invoke(t, home, draft, "reply", "draft", "--input", "-")
	if code != 0 {
		t.Fatalf("reply draft: %+v", created)
	}
	id := data(t, created)["draft"].(map[string]any)["id"].(string)
	code, previewed := invoke(t, home, "", "outbox", "preview", id)
	if code != 0 {
		t.Fatalf("preview: %+v", previewed)
	}
	p := data(t, previewed)
	if p["sendable"] != true {
		t.Fatalf("an enabled draft was not sendable: %+v", p["checks"])
	}
	// A stale digest is refused even now.
	if code, _ = invoke(t, home, "", "outbox", "dispatch", id, "--expected-digest", "0000"); code != 3 {
		t.Fatalf("a stale digest was accepted: %d", code)
	}
	code, dispatched := invoke(t, home, "", "outbox", "dispatch", id, "--expected-digest", p["display_digest"].(string))
	if code != 0 {
		t.Fatalf("dispatch: %+v", dispatched)
	}
	if data(t, dispatched)["state"] != "sending" {
		t.Fatalf("dispatch state: %+v", data(t, dispatched))
	}
	// accepted without a receipt is refused; an unknown result is not retry-safe.
	if code, _ = invoke(t, home, "", "outbox", "result", id, "accepted"); code != 2 {
		t.Fatalf("accepted was recorded without a receipt: %d", code)
	}
	code, unknown := invoke(t, home, "", "outbox", "result", id, "delivery_unknown", "--detail", "请求发出后超时")
	if code != 0 {
		t.Fatalf("result: %+v", unknown)
	}
	if data(t, unknown)["retry_safe"] != false {
		t.Fatalf("an unknown delivery was declared retry-safe: %+v", data(t, unknown))
	}
	// A local cancel does not withdraw a message that may already be out.
	if code, _ = invoke(t, home, "", "outbox", "cancel", id, "--reason", "放弃"); code != 3 {
		t.Fatalf("an unknown delivery was cancelled: %d", code)
	}
	// Reconciling needs the evidence that was checked.
	if code, _ = invoke(t, home, "", "outbox", "reconcile", id, "--outcome", "accepted", "--receipt", "r1"); code != 2 {
		t.Fatalf("a reconciliation without evidence was accepted: %d", code)
	}
	code, settled := invoke(t, home, "", "outbox", "reconcile", id, "--outcome", "accepted", "--receipt", "r1", "--evidence", "会话中查到同一条 messageId")
	if code != 0 {
		t.Fatalf("reconcile: %+v", settled)
	}
	if data(t, settled)["state"] != "accepted" {
		t.Fatalf("reconciled state: %+v", data(t, settled))
	}
}
