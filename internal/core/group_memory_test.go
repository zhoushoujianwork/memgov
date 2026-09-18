package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func applyGroupSharing(t *testing.T, s *Store, channel string, shared, excluded []string) {
	t.Helper()
	ctx := context.Background()
	current, err := ReadAppliedConfig(ctx, s.DB, 0)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"applications": map[string]any{"group_mention": map[string]any{"enabled": true, "channel": channel, "shared_memory_workspaces": shared, "excluded_memory_categories": excluded}}})
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "config.test.sharing"}, func(tx *Tx) (any, error) { return tx.CommitAppliedConfig(ctx, current.Version, 1, body, nil) })
	if err != nil {
		t.Fatal(err)
	}
}

func TestGroupGlobalSharingExcludesPreferencesAndRechecksLivePolicy(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	bot, route := fixtureChannel(t, s, ChannelDingTalkApp, "cid:one")
	personal, personalRoute := fixtureChannel(t, s, ChannelDwsPersonal, "cid:one")
	// Eligible group routes are already admitted by the application controller.
	if _, err := s.DB.ExecContext(ctx, "UPDATE channel_routes SET mode='assistant' WHERE id=?", route.ID); err != nil {
		t.Fatal(err)
	}
	var second, direct Route
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "route.test.extra"}, func(tx *Tx) (any, error) {
		var err error
		second, err = tx.AddRoute(ctx, bot.ID, RouteInput{ConversationID: "cid:two", ConversationType: "group", Mode: "assistant"})
		if err != nil {
			return nil, err
		}
		direct, err = tx.AddRoute(ctx, bot.ID, RouteInput{ConversationID: "cid:direct", ConversationType: "direct"})
		return direct, err
	})
	if err != nil {
		t.Fatal(err)
	}
	existing := createMemory(t, s, fixtureMemory(t, s, "global"))
	prefInput := fixtureMemory(t, s, "global")
	prefInput.Category, prefInput.Title = "preference", "默认中文回复"
	prefInput.Content = "个人偏好：发布失败时先检查权限，再查看错误日志。"
	pref := createMemory(t, s, prefInput)
	applyGroupSharing(t, s, bot.Name, []string{"global"}, []string{"preference"})
	assertVisible := func(channel, conversation, id string, want bool) {
		t.Helper()
		a, err := AudienceFor(ctx, s.DB, channel, conversation)
		if err != nil {
			t.Fatal(err)
		}
		d, err := CheckDisclosure(ctx, s.DB, a, id)
		if err != nil || d.Allowed != want {
			t.Fatalf("%s/%s: %+v %v; want %v", channel, conversation, d, err, want)
		}
	}
	for _, r := range []Route{route, second} {
		assertVisible(bot.Name, r.ConversationID, existing.ID, true)
		assertVisible(bot.Name, r.ConversationID, pref.ID, false)
	}
	assertVisible(personal.Name, personalRoute.ConversationID, existing.ID, false)
	assertVisible(bot.Name, direct.ConversationID, existing.ID, false)
	// Future global records are covered without individual publication writes.
	futureInput := fixtureMemory(t, s, "global")
	futureInput.Content += "测试环境已验证该处理流程。"
	future := createMemory(t, s, futureInput)
	assertVisible(bot.Name, second.ConversationID, future.ID, true)
	if _, err = s.DB.ExecContext(ctx, "UPDATE memories SET version=version+1 WHERE id=?", future.ID); err != nil {
		t.Fatal(err)
	}
	assertVisible(bot.Name, second.ConversationID, future.ID, true)
	futurePrefInput := fixtureMemory(t, s, "global")
	futurePrefInput.Category, futurePrefInput.Content = "preference", "个人偏好：测试环境已验证该处理流程。"
	futurePref := createMemory(t, s, futurePrefInput)
	assertVisible(bot.Name, second.ConversationID, futurePref.ID, false)
	// A publication cannot override the explicit group category exclusion.
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "memory.test.publish"}, func(tx *Tx) (any, error) {
		return tx.Publish(ctx, bot.Name, route.ConversationID, pref.ID, "old individual publication")
	})
	if err != nil {
		t.Fatal(err)
	}
	assertVisible(bot.Name, route.ConversationID, pref.ID, false)
	a, _ := AudienceFor(ctx, s.DB, bot.ID, route.ConversationID)
	if _, err = ReadAudienceMemory(ctx, s.DB, a, pref.ID); ErrorCode(err) != "denied" {
		t.Fatalf("preference body leaked: %v", err)
	}
	// Preferences cannot hide the actual latest visible memory before limit.
	if _, err = s.DB.ExecContext(ctx, "UPDATE memories SET updated_at='2099-01-01T00:00:00Z' WHERE id=?", pref.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE memories SET updated_at='2098-01-01T00:00:00Z' WHERE id=?", future.ID); err != nil {
		t.Fatal(err)
	}
	listed, err := ListAudienceMemories(ctx, s.DB, a, 1)
	if err != nil {
		t.Fatal(err)
	}
	items := listed.(map[string]any)["items"].([]AudienceMemory)
	if len(items) != 1 || items[0].ID != future.ID || items[0].Content != "" {
		t.Fatalf("latest visible: %+v", items)
	}
	item, err := ReadAudienceMemory(ctx, s.DB, a, existing.ID)
	if err != nil || item.Content == "" {
		t.Fatalf("shared body unavailable: %+v %v", item, err)
	}
	// Withdrawal still overrides workspace sharing.
	if _, err = s.DB.ExecContext(ctx, "INSERT INTO source_availability(source_id,state,reason,change_seq,updated_at) VALUES(?,'recalled','withdrawn',2,?)", existing.Evidence[0].SourceID, Now()); err != nil {
		t.Fatal(err)
	}
	assertVisible(bot.Name, route.ConversationID, existing.ID, false)
	applyGroupSharing(t, s, bot.Name, nil, []string{"preference"})
	assertVisible(bot.Name, second.ConversationID, future.ID, false)
}

func TestGroupSharingDoesNotAutomaticallyPublishOtherWorkspaces(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	bot, route := fixtureChannel(t, s, ChannelDingTalkApp, "cid:workspace")
	var workspace Workspace
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "workspace.test.private"}, func(tx *Tx) (any, error) {
		var err error
		workspace, err = tx.AddWorkspace(ctx, "group-work", "")
		return workspace, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE channel_routes SET mode='assistant',workspace_id=? WHERE id=?", workspace.ID, route.ID); err != nil {
		t.Fatal(err)
	}
	memory := createMemory(t, s, fixtureMemory(t, s, workspace.ID))
	applyGroupSharing(t, s, bot.Name, []string{"global"}, []string{"preference"})
	a, err := AudienceFor(ctx, s.DB, bot.ID, route.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if verdict, err := CheckDisclosure(ctx, s.DB, a, memory.ID); err != nil || verdict.Allowed {
		t.Fatalf("other workspace implicitly shared: %+v %v", verdict, err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "memory.test.publish.workspace"}, func(tx *Tx) (any, error) {
		return tx.Publish(ctx, bot.ID, route.ConversationID, memory.ID, "share just this version")
	})
	if err != nil {
		t.Fatal(err)
	}
	if verdict, err := CheckDisclosure(ctx, s.DB, a, memory.ID); err != nil || !verdict.Allowed {
		t.Fatalf("explicit workspace publication lost: %+v %v", verdict, err)
	}
}

func TestAudienceRecallFiltersPrivateAndPreferencesBeforeBudget(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	bot, route := fixtureChannel(t, s, ChannelDingTalkApp, "cid:recall")
	if _, err := s.DB.ExecContext(ctx, "UPDATE channel_routes SET mode='assistant' WHERE id=?", route.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		m := fixtureMemory(t, s, "global")
		m.Category = "preference"
		m.Content = fmt.Sprintf("偏好 %d：%s", i, m.Content)
		createMemory(t, s, m)
	}
	visible := createMemory(t, s, fixtureMemory(t, s, "global"))
	applyGroupSharing(t, s, bot.Name, []string{"global"}, []string{"preference"})
	a, _ := AudienceFor(ctx, s.DB, bot.ID, route.ConversationID)
	result, err := AudienceRecall(ctx, s.DB, a, "发布 权限", 4000, false)
	if err != nil {
		t.Fatal(err)
	}
	out := result.(map[string]any)
	items := out["items"].([]RecallItem)
	if len(items) != 1 || items[0].ID != visible.ID || len(items[0].Evidence) != 0 || len(out["refused"].([]Disclosure)) != 0 {
		t.Fatalf("filtered recall leaked or lost memory: %+v", out)
	}
}
