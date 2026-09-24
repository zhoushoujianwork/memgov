package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

type fixtureBotDirectory struct{ attestationErr error }

func (d fixtureBotDirectory) AttestOwner(context.Context, core.Channel) error {
	return d.attestationErr
}

func (fixtureBotDirectory) ResolveBotUser(_ context.Context, _ channel.Config, name string) (channel.BotUser, error) {
	if name != "Wang" {
		return channel.BotUser{}, core.Fail("not_found", "person not found")
	}
	return channel.BotUser{ID: "user-wang", Name: name}, nil
}
func (fixtureBotDirectory) ListBotGroups(context.Context, channel.Config) ([]channel.GroupConversation, error) {
	return []channel.GroupConversation{{ID: "cid:group-a", Name: "Origin"}, {ID: "cid:group-b", Name: "Other"}}, nil
}

func TestGroupBotMCPRegistrationIsScoped(t *testing.T) {
	in := ExecutionInput{Home: t.TempDir(), Task: core.RuntimeTask{ID: "task-1"}, AttemptID: "attempt-1", ApplicationMode: "group_mention"}
	config, allowed, prompt := agentMCPConfiguration(in)
	if !strings.Contains(config, `"memgov_bot"`) || !strings.Contains(config, `"bot-mcp"`) || !strings.Contains(strings.Join(allowed, ","), "mcp__memgov_bot__forward_bot_message") || !strings.Contains(prompt, "sends immediately") {
		t.Fatalf("group bot tool not registered: %s %v %q", config, allowed, prompt)
	}
	in.ApplicationMode = "proactive"
	config, allowed, _ = agentMCPConfiguration(in)
	if strings.Contains(config, `"memgov_bot"`) || len(allowed) != 0 {
		t.Fatalf("bot tool leaked outside group mode: %s %v", config, allowed)
	}
	m := &botMCP{}
	var out bytes.Buffer
	if err := m.serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Result struct {
			Tools []botMCPTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil || len(reply.Result.Tools) != 3 {
		t.Fatalf("bot MCP catalog=%s error=%v", out.String(), err)
	}
	for _, tool := range reply.Result.Tools {
		if _, ok := tool.InputSchema["required"].([]any); !ok {
			// Locally built schemas retain []string before JSON encoding; the
			// decoded reply above is map[string]any and must be an array.
			t.Fatalf("tool %s has invalid required schema: %+v", tool.Name, tool.InputSchema)
		}
	}
}

func TestBotForwardMCPSendsWithoutConfirmation(t *testing.T) {
	s, cfg, app, group, _ := setupGroupMentionService(t)
	ctx := context.Background()
	history, err := core.ReadChannel(ctx, s.Store.DB, app.Identity.HistoryChannel)
	if err != nil {
		t.Fatal(err)
	}
	history.Identity.Profile = "bound-profile"
	if _, err = s.Store.DB.ExecContext(ctx, "UPDATE channels SET identity=? WHERE id=?", core.JSON(history.Identity), history.ID); err != nil {
		t.Fatal(err)
	}
	var task core.RuntimeTask
	var attempt core.RuntimeAttempt
	if err = s.mutate(ctx, "global", "bot.mcp.fixture", func(tx *core.Tx) (any, error) {
		if _, e := tx.Intake(ctx, app.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "forward-request", ConversationID: group.ConversationID, ConversationType: "group", Tenant: app.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "requester"}, Mentioned: true, Body: "Forward to Wang", SentAt: time.Now().UTC().Format(time.RFC3339Nano)}); e != nil {
			return nil, e
		}
		if _, e := tx.SyncRuntimeMessages(ctx, cfg.ID); e != nil {
			return nil, e
		}
		batch, e := tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now().Add(time.Minute))
		if e != nil {
			return nil, e
		}
		if _, e = tx.CompleteRuntimeBatch(ctx, batch, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "bot-forward", Title: "Forward", Instructions: "Forward to Wang", MessageIDs: []string{batch.Messages[0].ID}}}}); e != nil {
			return nil, e
		}
		task, attempt, e = tx.ClaimRuntimeTask(ctx, cfg.ID, core.NewID(), "fake", "preset", "commit", "")
		return task, e
	}); err != nil {
		t.Fatal(err)
	}
	m := &botMCP{store: s.Store, directory: fixtureBotDirectory{}, sender: s.Adapter, taskID: task.ID, attemptID: attempt.ID}
	m.directory = fixtureBotDirectory{attestationErr: core.Fail("denied", "profile identity changed")}
	if _, err = m.call(ctx, "resolve_bot_user", []byte(`{"name":"Wang"}`)); core.ErrorCode(err) != "denied" {
		t.Fatalf("unattested owner profile resolved a recipient: %v", err)
	}
	m.directory = fixtureBotDirectory{}
	user, err := m.call(ctx, "resolve_bot_user", []byte(`{"name":"Wang"}`))
	if err != nil || user.(channel.BotUser).ID != "user-wang" {
		t.Fatalf("resolved user=%+v error=%v", user, err)
	}
	if _, err = m.call(ctx, "resolve_bot_group", []byte(`{"name_or_id":"Other"}`)); core.ErrorCode(err) != "denied" {
		t.Fatalf("unmounted group resolved: %v", err)
	}
	sent, err := m.call(ctx, "forward_bot_message", []byte(`{"target_type":"user","recipient":"Wang","content":"Exact quoted content"}`))
	if err != nil || sent.(map[string]any)["status"] != "accepted" {
		t.Fatalf("bot send=%+v error=%v", sent, err)
	}
	requests := s.Adapter.(*fakeAdapter).requests
	if len(requests) != 1 || requests[0].Transport != "bot_dm" || requests[0].ConversationID != "user-wang" || requests[0].Content != "Exact quoted content" || requests[0].IdempotencyKey != sent.(map[string]any)["action_id"] {
		t.Fatalf("bot used wrong identity or target: %+v", requests)
	}
	again, err := m.call(ctx, "forward_bot_message", []byte(`{"target_type":"user","recipient":"Wang","content":"Exact quoted content"}`))
	if err != nil || again.(map[string]any)["action_id"] != sent.(map[string]any)["action_id"] || len(s.Adapter.(*fakeAdapter).requests) != 1 {
		t.Fatalf("duplicate bot send=%+v error=%v requests=%+v", again, err, s.Adapter.(*fakeAdapter).requests)
	}
	if err = s.mutate(ctx, "global", "bot.mcp.complete", func(tx *core.Tx) (any, error) {
		var e error
		task, e = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, core.RuntimeAttemptResult{Result: "Bot accepted the message", Summary: "Forward complete", Actions: []core.RuntimeAction{{Kind: "bot_direct_message", Target: "user:user-wang", Payload: "duplicate"}}})
		return task, e
	}); err != nil {
		t.Fatal(err)
	}
	if task.Status != "completed" || len(task.Actions) != 0 {
		t.Fatalf("bot send still requires confirmation: %+v", task)
	}
}

func TestConfirmedBotGroupForwardUsesMountedRoute(t *testing.T) {
	s, cfg, app, group, other := setupGroupMentionService(t)
	ctx := context.Background()
	history, err := core.ReadChannel(ctx, s.Store.DB, app.Identity.HistoryChannel)
	if err != nil {
		t.Fatal(err)
	}
	history.Identity.Profile = "bound-profile"
	if _, err = s.Store.DB.ExecContext(ctx, "UPDATE channels SET identity=? WHERE id=?", core.JSON(history.Identity), history.ID); err != nil {
		t.Fatal(err)
	}
	target := "group:" + group.ConversationID
	payload := core.JSON(core.RuntimeBotForwardPayload{Target: target, RecipientName: "Origin", Content: "Group forwarding text"})
	task := core.RuntimeTask{ID: "task", Version: 1}
	action := core.RuntimePendingAction{ID: "action", TaskID: task.ID, TaskVersion: task.Version, Kind: core.RuntimeBotForwardAction, Target: target, Payload: payload, PayloadDigest: core.Hash([]byte(payload)), ConfirmedBy: cfg.OwnerPrincipalID, ConfirmationOrigin: "dingtalk_message:owner"}
	result, unknown, err := s.executeConfirmedBotForward(ctx, cfg, task, action)
	if err != nil || unknown || result.Result == "" {
		t.Fatalf("mounted group send=%+v unknown=%v error=%v", result, unknown, err)
	}
	requests := s.Adapter.(*fakeAdapter).requests
	if len(requests) != 1 || requests[0].Transport != "bot_group" || requests[0].ConversationID != group.ConversationID || requests[0].Content != "Group forwarding text" {
		t.Fatalf("group bot transport=%+v", requests)
	}
	action.Target = "group:" + other.ConversationID
	if _, _, err = s.executeConfirmedBotForward(ctx, cfg, task, action); core.ErrorCode(err) != "denied" || len(s.Adapter.(*fakeAdapter).requests) != 1 {
		t.Fatalf("changed or unmounted group inherited approval: %v", err)
	}
}
