package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/channel/dingtalkapp"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// appFrame is a desensitized bot callback. It carries the session webhook the
// platform really sends, so the credential-stripping path is exercised end to end
// rather than assumed.
func appFrame(msgID, body string, mentioned bool) string {
	at := "false"
	if mentioned {
		at = "true"
	}
	return `{"conversationId":"cid:group2","conversationType":"2","msgId":"` + msgID + `","msgtype":"text",` +
		`"senderId":"union-alice","senderStaffId":"staff-alice","senderNick":"Alice","senderCorpId":"corp1",` +
		`"chatbotCorpId":"corp1","chatbotUserId":"bot-1","createAt":1757000000000,"isInAtList":` + at + `,` +
		`"sessionWebhook":"https://oapi.dingtalk.com/robot/sendBySession?session=SECRET",` +
		`"sessionWebhookExpiredTime":1757000300000,"robotCode":"bot-1","text":{"content":"` + body + `"}}`
}

// appAdapterWith builds the real application adapter over recorded frames. Only
// the platform transport and the credential lookup are substituted, so the
// parsing, redaction and acknowledgement logic under test is the production one.
func appAdapterWith(frames ...string) *dingtalkapp.Adapter {
	return &dingtalkapp.Adapter{
		Secrets: func(context.Context, string) (string, error) { return "app-secret", nil },
		Frames: func(ctx context.Context, _ channel.Config, secret string, opts dingtalkapp.SessionOptions) error {
			if secret == "" {
				return core.Fail("denied", "the session was started without a credential")
			}
			if opts.Ready != nil {
				opts.Ready(map[string]any{"transport": "stream", "topic": "/v1.0/im/bot/messages/get"})
			}
			for _, raw := range frames {
				if err := opts.Handle(ctx, []byte(raw)); err != nil {
					// The real SDK turns this into a failed acknowledgement, which is
					// what stops the session here too.
					return err
				}
			}
			return nil
		}}
}

// assertNoCredentialOnDisk scans the whole database file. A session webhook is a
// bearer token: it authorizes sending to a conversation, so it must not survive
// anywhere the database can be read, exported or backed up from.
func assertNoCredentialOnDisk(t *testing.T, home string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	// The field names may remain: they record what arrived. The values must not.
	for _, secret := range []string{"SECRET", "sendBySession", "1757000300000"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("the database holds the credential fragment %q", secret)
		}
	}
	if !bytes.Contains(raw, []byte("[redacted]")) {
		t.Fatal("no payload was redacted, so the check proved nothing")
	}
}

// appChannel registers one application-bot channel bound to one conversation.
func appChannel(t *testing.T, home string) {
	t.Helper()
	invoke(t, home, "", "init")
	add := `{"name":"bot-main","kind":"dingtalk_app","identity":{"expected_corp_id":"corp1","client_id":"cli-1","robot_code":"bot-1"},` +
		`"credential_ref":"keychain://memgov/bot","route":{"conversation_id":"cid:group2","conversation_type":"group"}}`
	if code, value := invoke(t, home, add, "channel", "add", "--input", "-"); code != 0 {
		t.Fatalf("app channel add: %+v", value)
	}
}

// A Stream session stores the bot's messages with their evidence, keeps the
// session webhook out of the database, and reports duplicates rather than
// double-counting a redelivered frame.
func TestAppStreamSessionStoresMessagesWithoutCredentials(t *testing.T) {
	home := t.TempDir()
	appChannel(t, home)
	probe := appAdapterWith()
	if code, value := invokeWithApp(t, probe, home, "", "channel", "probe", "bot-main"); code != 0 {
		t.Fatalf("probe: %+v", value)
	} else if caps := data(t, value)["capabilities"].(map[string]any); caps["verified"].(map[string]any)["send"] != true {
		t.Fatalf("the probe did not expose active robot delivery: %+v", caps)
	}
	// The same frame arrives twice, which is what a platform redelivery looks like.
	adapter := appAdapterWith(appFrame("m1", "上线前要先备份数据库", true), appFrame("m1", "上线前要先备份数据库", true),
		appFrame("m2", "随口聊两句", false))
	code, value := invokeWithApp(t, adapter, home, "", "channel", "run", "bot-main", "--lease-ttl", "1m")
	if code != 0 {
		t.Fatalf("run: %+v", value)
	}
	out := data(t, value)
	if out["listening"] != true {
		t.Fatalf("readiness was not reported: %+v", out)
	}
	if out["applied"] != float64(2) || out["duplicates"] != float64(1) {
		t.Fatalf("a redelivered frame was double-counted: %+v", out)
	}
	// Both messages are readable, and the one that did not address the bot is
	// stored too: it is history, not a request.
	code, listed := invoke(t, home, "", "message", "list", "bot-main", "--conversation", "cid:group2")
	if code != 0 {
		t.Fatalf("message list: %+v", listed)
	}
	rows := listed["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("messages: %+v", rows)
	}
	// The message is readable with its evidence.
	id := rows[0].(map[string]any)["id"].(string)
	code, shown := invoke(t, home, "", "message", "show", id)
	if code != 0 {
		t.Fatalf("message show: %+v", shown)
	}
	if len(data(t, shown)["source_ids"].([]any)) == 0 {
		t.Fatalf("the message was stored without a source: %+v", data(t, shown))
	}
	// The database as a whole holds no session webhook. This is checked against the
	// file rather than one query, because the payload snapshot, the message body and
	// any evidence copy would each be a place for a bearer credential to survive.
	assertNoCredentialOnDisk(t, home)
	// A second session can start, which proves the lease was released.
	if code, value = invokeWithApp(t, appAdapterWith(), home, "", "channel", "run", "bot-main", "--lease-ttl", "1m"); code != 0 {
		t.Fatalf("the lease was not released: %d %+v", code, value)
	}
}

// A frame this process could not store is not acknowledged, and receiving stops
// so recovery happens by watermark instead of losing the message. A frame from
// another tenant is recorded as rejected rather than written into this channel.
func TestAppStreamSessionIsolatesForeignFramesAndStopsOnAWriteFailure(t *testing.T) {
	home := t.TempDir()
	appChannel(t, home)
	if code, value := invokeWithApp(t, appAdapterWith(), home, "", "channel", "probe", "bot-main"); code != 0 {
		t.Fatalf("probe: %+v", value)
	}
	foreign := strings.Replace(appFrame("m9", "别家企业的消息", true), `"chatbotCorpId":"corp1"`, `"chatbotCorpId":"corp9"`, 1)
	unbound := strings.Replace(appFrame("m8", "未绑定会话", true), `"cid:group2"`, `"cid:other"`, 1)
	adapter := appAdapterWith(foreign, appFrame("m1", "先记下来", true), unbound)
	code, value := invokeWithApp(t, adapter, home, "", "channel", "run", "bot-main", "--lease-ttl", "1m")
	// A conversation this channel is not bound to cannot be written, so the session
	// stops there instead of acknowledging a message it did not store.
	if code != 6 || !strings.Contains(core.JSON(value["error"]), "not bound to a route") {
		t.Fatalf("an unbound conversation was accepted: %d %+v", code, value)
	}
	// What was committed before the failure is kept.
	code, listed := invoke(t, home, "", "message", "list", "bot-main", "--conversation", "cid:group2")
	if code != 0 {
		t.Fatalf("message list: %+v", listed)
	}
	if len(listed["data"].([]any)) != 1 {
		t.Fatalf("committed messages were lost or extra ones appeared: %+v", listed["data"])
	}
	// The foreign frame is visible as a rejection, by reason only.
	code, inbox := invoke(t, home, "", "message", "inbox", "list", "bot-main", "--status", "rejected")
	if code != 0 {
		t.Fatalf("inbox: %+v", inbox)
	}
	events := core.JSON(inbox)
	if !strings.Contains(events, "denied") {
		t.Fatalf("the foreign frame was not recorded as rejected: %s", events)
	}
	if strings.Contains(events, "别家企业的消息") {
		t.Fatalf("a foreign frame's body was stored: %s", events)
	}
}

// The two channel kinds keep separate audiences: publishing a memory to the
// personal channel's conversation does not disclose it to the bot's group.
func TestAudienceStaysSeparateBetweenPersonalAndAppChannels(t *testing.T) {
	home := t.TempDir()
	appChannel(t, home)
	add := `{"name":"dws-main","kind":"dws_personal","identity":{"profile":"corp1:user1","expected_corp_id":"corp1","expected_user_id":"user1"},` +
		`"route":{"conversation_id":"cid:group1","conversation_type":"group"}}`
	if code, value := invoke(t, home, add, "channel", "add", "--input", "-"); code != 0 {
		t.Fatalf("dws channel add: %+v", value)
	}
	id := formalMemory(t, home)
	if code, value := invoke(t, home, "", "audience", "publish", "dws-main", id, "--conversation", "cid:group1", "--reason", "个人会话已讨论"); code != 0 {
		t.Fatalf("publish: %+v", value)
	}
	code, value := invoke(t, home, "", "audience", "check", "bot-main", id, "--conversation", "cid:group2")
	if code != 0 {
		t.Fatalf("check: %+v", value)
	}
	if data(t, value)["allowed"] != false {
		t.Fatalf("a personal publication disclosed to the bot's group: %+v", data(t, value))
	}
	// The bot's group needs its own publication, and that one does not reach back.
	if code, value = invoke(t, home, "", "audience", "publish", "bot-main", id, "--conversation", "cid:group2", "--reason", "群里问到该流程"); code != 0 {
		t.Fatalf("publish to the bot group: %+v", value)
	}
	if code, value = invoke(t, home, "", "audience", "check", "bot-main", id, "--conversation", "cid:group2"); code != 0 || data(t, value)["allowed"] != true {
		t.Fatalf("the bot group's own publication did not permit disclosure: %+v", value)
	}
}
