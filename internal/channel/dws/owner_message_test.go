package dws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestOwnerMessagesUseFixedPersonalIdentityAndNativeReceipts(t *testing.T) {
	for _, transport := range []string{"user_group", "user_dm"} {
		t.Run(transport, func(t *testing.T) {
			var sendArgs []string
			a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(joined, "profile list"):
					return envelope(`{"profiles":[{"profile":"corp1:user1","corpId":"corp1","userId":"user1"}]}`), nil
				case strings.HasPrefix(joined, "contact +me"):
					return envelope(`{"data":{"userId":"user1"}}`), nil
				case strings.HasPrefix(joined, "chat +messages-send"):
					sendArgs = args
					return envelope(`{"openTaskId":"send-task"}`), nil
				case strings.HasPrefix(joined, "chat message query-send-status"):
					if !strings.Contains(joined, "--open-task-id send-task") || !strings.Contains(joined, "--profile corp1:user1") {
						t.Fatalf("unbound status query %s", joined)
					}
					return envelope(`{"openMessageId":"provider-message","openConversationId":"cid:group1"}`), nil
				default:
					t.Fatalf("unexpected call %s", joined)
					return nil, nil
				}
			}}
			r, err := a.Send(context.Background(), testConfig(), channel.SendRequest{Transport: transport, ConversationID: "stable-target", Content: "quoted $() `literal`", Format: "markdown", IdempotencyKey: "outreach-1"})
			if err != nil || r.State != "accepted" || !strings.Contains(r.Receipt, "provider-message") {
				t.Fatalf("send %+v %v", r, err)
			}
			joined := strings.Join(sendArgs, " ")
			targetFlag := "--group"
			if transport == "user_dm" {
				targetFlag = "--user"
			}
			for _, want := range []string{"--as user", targetFlag + " stable-target", "--idempotency-key outreach-1", "--profile corp1:user1", "--ai-tag true", "--yes"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("missing %s in %s", want, joined)
				}
			}
			if strings.Contains(joined, "--robot-code") || strings.Contains(joined, "--users") {
				t.Fatalf("owner send used robot flags: %s", joined)
			}
		})
	}
}

func TestOwnerSendPreservesUnknownAndRejectsMismatchedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, user, result, want string
		sendError                bool
	}{
		{"identity mismatch", "other", "", "blocked", false},
		{"timeout", "user1", "", "unknown", true},
		{"unrecognized", "user1", `{"succeededCount":1}`, "unknown", false},
		{"failed target", "user1", `{"failedCount":1}`, "failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				if strings.HasPrefix(joined, "profile list") {
					return envelope(`{"profiles":[{"profile":"corp1:user1","corpId":"corp1","userId":"user1"}]}`), nil
				}
				if strings.HasPrefix(joined, "contact +me") {
					return envelope(`{"data":{"userId":"` + tc.user + `"}}`), nil
				}
				writes++
				if tc.sendError {
					return nil, errors.New("timeout after send")
				}
				return envelope(tc.result), nil
			}}
			r, _ := a.Send(context.Background(), testConfig(), channel.SendRequest{Transport: "user_group", ConversationID: "cid:group", Content: "hello", IdempotencyKey: "once"})
			if r.State != tc.want || writes > 1 || (tc.user != "user1" && writes != 0) {
				t.Fatalf("state=%s writes=%d", r.State, writes)
			}
		})
	}
}

func TestOwnerTargetVerificationUsesExactDirectoryUser(t *testing.T) {
	a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		for _, want := range []string{"--user stable-user", "--as user", "--dry-run", "--profile corp1:user1"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("missing %s: %s", want, joined)
			}
		}
		if strings.Contains(joined, "--user-query") || strings.Contains(joined, "--yes") {
			t.Fatal("unsafe target resolution")
		}
		return envelope(`{"dry_run":true,"executed":false,"actionCount":1}`), nil
	}}
	c := core.Channel{Kind: core.ChannelDwsPersonal, Tenant: "corp1", Identity: testConfig().Identity}
	if err := a.VerifyOwnerMessageTarget(context.Background(), c, "user", "stable-user"); err != nil {
		t.Fatal(err)
	}
}

func TestGroupDiscoveryWithoutRobotFilterKeepsActiveGroups(t *testing.T) {
	a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "chat +chat-list-all"):
			return envelope(`{"complete":true,"groups":[{"openConversationId":"cid:active","name":"Active"},{"openConversationId":"cid:inactive","name":"Inactive"}]}`), nil
		case strings.HasPrefix(joined, "chat +search-msg"):
			return envelope(`{"complete":true,"messages":[{"conversationId":"cid:active"}]}`), nil
		default:
			t.Fatalf("robot-independent discovery called %s", joined)
			return nil, nil
		}
	}}
	r, err := a.DiscoverGroupConversations(context.Background(), testConfig())
	if err != nil || !r.Complete || len(r.Groups) != 1 || r.Groups[0].ID != "cid:active" {
		t.Fatalf("discovery %+v %v", r, err)
	}
}

func TestOwnerStatusReaderDoesNotTreatFailureOrPendingAsDelivery(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"failedCount":1}`, "failed"},
		{`{"success":false}`, "failed"},
		{`{"status":2}`, "unknown"},
		{`{"pending":true}`, "unknown"},
		{`{"openMessageId":"delivered","openConversationId":"cid:group"}`, "accepted"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] != "chat" || args[1] != "message" || args[2] != "query-send-status" {
					t.Fatalf("status query dispatched a message: %v", args)
				}
				return envelope(tc.raw), nil
			}}
			result, err := a.ReadOwnerMessageStatus(context.Background(), testConfig(), `{"openTaskId":"native-task"}`)
			if err != nil || result.State != tc.want || !strings.Contains(result.Receipt, "native-task") {
				t.Fatalf("status %+v %v", result, err)
			}
		})
	}
}
