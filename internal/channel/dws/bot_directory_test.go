package dws

import (
	"context"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestBotDirectoryUsesBoundProfileAndRequiresStableUser(t *testing.T) {
	var calls [][]string
	a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{}, args...))
		return []byte(`{"success":true,"result":{"profile":{"userId":"user-1","orgUserId":"user-1","orgUserName":"Wang"}}}`), nil
	}}
	cfg := channel.Config{Identity: core.ChannelIdentity{Profile: "bound-profile"}}
	user, err := a.ResolveBotUser(context.Background(), cfg, "Wang")
	if err != nil || user.ID != "user-1" || user.Name != "Wang" {
		t.Fatalf("resolved user=%+v error=%v", user, err)
	}
	joined := strings.Join(calls[0], " ")
	if !strings.Contains(joined, "contact +lookup --name Wang") || !strings.Contains(joined, "--profile bound-profile") || strings.Contains(joined, "messages-send") {
		t.Fatalf("recipient lookup used the wrong identity or sent a message: %s", joined)
	}
	a.Run = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"success":true,"result":{"profile":{"userId":"user-1","orgUserId":"user-2"}}}`), nil
	}
	if _, err = a.ResolveBotUser(context.Background(), cfg, "Wang"); core.ErrorCode(err) != "denied" {
		t.Fatalf("mismatched identities accepted: %v", err)
	}
}

func TestBotGroupDirectoryRejectsPartialResults(t *testing.T) {
	a := &Adapter{Run: func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"success":true,"result":{"groups":[{"openConversationId":"cid:a","name":"Team"}],"complete":true}}`), nil
	}}
	cfg := channel.Config{Identity: core.ChannelIdentity{Profile: "bound-profile"}}
	groups, err := a.ListBotGroups(context.Background(), cfg)
	if err != nil || len(groups) != 1 || groups[0].ID != "cid:a" {
		t.Fatalf("groups=%+v error=%v", groups, err)
	}
	a.Run = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"success":true,"result":{"groups":[],"complete":false,"partial":true}}`), nil
	}
	if _, err = a.ListBotGroups(context.Background(), cfg); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("partial group directory accepted: %v", err)
	}
}
