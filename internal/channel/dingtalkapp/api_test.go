package dingtalkapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func apiAdapter(t *testing.T, handler http.HandlerFunc) (*Adapter, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Adapter{Secrets: testSecret, HTTP: server.Client(), APIBase: server.URL}, server
}

func TestActiveDirectSendUsesAppCredentialAndReturnsSafeReceipt(t *testing.T) {
	var tokenCalls, sendCalls int
	a, _ := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case accessTokenPath:
			tokenCalls++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["appKey"] != "cli-1" || body["appSecret"] != "app-secret" {
				t.Fatalf("wrong token identity: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "token-value", "expireIn": 7200})
		case directSendPath:
			sendCalls++
			if r.Header.Get("x-acs-dingtalk-access-token") != "token-value" {
				t.Fatal("missing access token")
			}
			var body struct {
				RobotCode string   `json:"robotCode"`
				UserIDs   []string `json:"userIds"`
				MsgKey    string   `json:"msgKey"`
				MsgParam  string   `json:"msgParam"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.RobotCode != "bot-1" || len(body.UserIDs) != 1 || body.UserIDs[0] != "owner" || body.MsgKey != "sampleMarkdown" {
				t.Fatalf("wrong send request: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"processQueryKey": "provider-receipt", "invalidStaffIdList": []string{}, "filteredStaffIdList": []string{}, "flowControlledStaffIdList": []string{}})
		default:
			http.NotFound(w, r)
		}
	})
	result, err := a.Send(context.Background(), config(), channel.SendRequest{ConversationID: "owner", Transport: "bot_dm", Content: "**done**", Format: "markdown"})
	if err != nil || result.State != "accepted" || result.Receipt == "" || result.Receipt == "provider-receipt" {
		t.Fatalf("send result=%+v err=%v", result, err)
	}
	if tokenCalls != 1 || sendCalls != 1 {
		t.Fatalf("token calls=%d send calls=%d", tokenCalls, sendCalls)
	}
}

func TestActiveGroupSendUsesBoundConversation(t *testing.T) {
	a, _ := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == accessTokenPath {
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "token-value", "expireIn": 7200})
			return
		}
		if r.URL.Path != groupSendPath {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["robotCode"] != "bot-1" || body["openConversationId"] != "cid-group" || body["msgKey"] != "sampleMarkdown" {
			t.Fatalf("wrong group send request: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"processQueryKey": "group-receipt"})
	})
	result, err := a.Send(context.Background(), config(), channel.SendRequest{ConversationID: "cid-group", Transport: "bot_group", Content: "**answer**", ReplyTo: "question"})
	if err != nil || result.State != "accepted" || result.Receipt == "" || result.Receipt == "group-receipt" {
		t.Fatalf("group send result=%+v err=%v", result, err)
	}
}

func TestGroupMarkdownFallbackUsesOneVisibleNameWithoutFakeAtFields(t *testing.T) {
	a, _ := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == accessTokenPath {
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "token-value", "expireIn": 7200})
			return
		}
		if r.URL.Path != groupSendPath {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		var param map[string]string
		_ = json.Unmarshal([]byte(body["msgParam"].(string)), &param)
		if body["msgKey"] != "sampleMarkdown" || param["text"] != "answer\n\n@Requester" || strings.Count(param["text"], "@Requester") != 1 {
			t.Fatalf("wrong mentioned markdown request: body=%+v param=%+v", body, param)
		}
		if _, exists := body["atUserIds"]; exists {
			t.Fatalf("proactive group endpoint silently ignores @ fields: %+v", body)
		}
		if _, exists := body["atOpenDingTalkIds"]; exists {
			t.Fatalf("proactive group endpoint silently ignores @ fields: %+v", body)
		}
		if _, card := body["cardTemplateId"]; card {
			t.Fatalf("plain group reply unexpectedly used a card: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"processQueryKey": "group-receipt"})
	})
	message := core.RuntimeCard{Text: "answer\n\n@Requester", TaskVersion: 1, Mentions: []core.RuntimeCardMention{{IDType: "user_id", IDValue: "user-1", Name: "Requester"}}}
	result, err := a.Send(context.Background(), config(), channel.SendRequest{ConversationID: "cid-group", Transport: "bot_group", Content: core.JSON(message), Format: "group_markdown", IdempotencyKey: "outbox", ReplyTo: "question"})
	if err != nil || result.State != "accepted" {
		t.Fatalf("group markdown result=%+v err=%v", result, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRecordedCallbackSendsNativeMentionThroughSessionWebhook(t *testing.T) {
	now := time.UnixMilli(1757000000000)
	stream := &fakeStream{frames: []string{frame("")}}
	var gotURL string
	var gotBody map[string]any
	a := adapter(stream)
	a.Now = func() time.Time { return now }
	a.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"errcode":0,"errmsg":"ok"}`)), Header: make(http.Header)}, nil
	})}
	if err := a.RunReceiver(context.Background(), config(), channel.ReceiverOptions{Handle: func(context.Context, core.NormalizedEvent) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	message := core.RuntimeCard{Text: "answer\n\n@Alice", TaskVersion: 1, Mentions: []core.RuntimeCardMention{{IDType: "user_id", IDValue: "staff-alice", Name: "Alice"}}}
	result, err := a.Send(context.Background(), config(), channel.SendRequest{ConversationID: "cid:group2", Transport: "bot_group", Content: core.JSON(message), Format: "group_markdown", IdempotencyKey: "outbox-native", ReplyTo: "m1"})
	if err != nil || result.State != "accepted" || result.Receipt == "" {
		t.Fatalf("native mention result=%+v err=%v", result, err)
	}
	if !strings.HasPrefix(gotURL, "https://oapi.dingtalk.com/robot/sendBySession?") {
		t.Fatalf("reply did not use callback webhook: %s", gotURL)
	}
	markdown, _ := gotBody["markdown"].(map[string]any)
	if text, _ := markdown["text"].(string); text != "answer\n\n@staff-alice" || strings.Count(text, "@") != 1 {
		t.Fatalf("native mention body=%q", text)
	}
	at, _ := gotBody["at"].(map[string]any)
	ids, _ := at["atUserIds"].([]any)
	if len(ids) != 1 || ids[0] != "staff-alice" {
		t.Fatalf("native mention ids=%v", at["atUserIds"])
	}
}

func TestFailedCallbackWriteDoesNotRetainSessionWebhook(t *testing.T) {
	stream := &fakeStream{frames: []string{frame("")}}
	a := adapter(stream)
	a.Now = func() time.Time { return time.UnixMilli(1757000000000) }
	writeErr := core.Fail("unavailable", "write failed")
	if err := a.RunReceiver(context.Background(), config(), channel.ReceiverOptions{Handle: func(context.Context, core.NormalizedEvent) error { return writeErr }}); err != nil {
		t.Fatal(err)
	}
	if len(stream.results) != 1 || !errors.Is(stream.results[0], writeErr) {
		t.Fatalf("failed callback was acknowledged: %v", stream.results)
	}
	a.replyMu.Lock()
	defer a.replyMu.Unlock()
	if len(a.replySessions) != 0 {
		t.Fatal("an uncommitted callback retained its reply credential")
	}
}

func TestUnknownSessionReplyDoesNotFallBackToProactiveSend(t *testing.T) {
	stream := &fakeStream{frames: []string{frame("")}}
	var calls int
	a := adapter(stream)
	a.Now = func() time.Time { return time.UnixMilli(1757000000000) }
	a.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("connection reset after request")
	})}
	if err := a.RunReceiver(context.Background(), config(), channel.ReceiverOptions{Handle: func(context.Context, core.NormalizedEvent) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	message := core.RuntimeCard{Text: "answer\n\n@Alice", Mentions: []core.RuntimeCardMention{{IDType: "user_id", IDValue: "staff-alice", Name: "Alice"}}}
	result, err := a.Send(context.Background(), config(), channel.SendRequest{ConversationID: "cid:group2", Transport: "bot_group", Content: core.JSON(message), Format: "group_markdown", IdempotencyKey: "outbox-unknown", ReplyTo: "m1"})
	if err == nil || result.State != "unknown" || calls != 1 {
		t.Fatalf("unknown reply was retried through another endpoint: result=%+v calls=%d err=%v", result, calls, err)
	}
}

func TestReactionLifecycleUsesOriginalMessage(t *testing.T) {
	paths := []string{}
	a, _ := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == accessTokenPath {
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "token-value", "expireIn": 7200})
			return
		}
		paths = append(paths, r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["openConversationId"] != "cid-direct" || body["openMsgId"] != "message-1" || body["emotionName"] != "暗中观察" {
			t.Fatalf("wrong reaction request: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	})
	req := channel.ReactionRequest{ConversationID: "cid-direct", MessageID: "message-1", Emoji: "暗中观察"}
	if err := a.AddReaction(context.Background(), config(), req); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveReaction(context.Background(), config(), req); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != reactionAddPath || paths[1] != reactionDelPath {
		t.Fatalf("reaction paths: %v", paths)
	}
}
