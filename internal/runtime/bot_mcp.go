package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/channel/dingtalkapp"
	"github.com/zhoushoujianwork/memgov/internal/channel/dws"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

type botDirectory interface {
	AttestOwner(context.Context, core.Channel) error
	ResolveBotUser(context.Context, channel.Config, string) (channel.BotUser, error)
	ListBotGroups(context.Context, channel.Config) ([]channel.GroupConversation, error)
}

type botMCP struct {
	store     *core.Store
	directory botDirectory
	sender    channel.Adapter
	taskID    string
	attemptID string
}

type botMCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func botSchema(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

var botMCPTools = []botMCPTool{
	{"resolve_bot_user", "Resolve one same-tenant DingTalk user name to a stable userId. Ambiguous names must be clarified.", botSchema(map[string]any{"name": map[string]any{"type": "string"}}, "name")},
	{"resolve_bot_group", "Resolve a group name or ID among groups mounted on this application bot.", botSchema(map[string]any{"name_or_id": map[string]any{"type": "string"}}, "name_or_id")},
	{"forward_bot_message", "Send a separate message through this application bot to one resolved same-tenant user or mounted group. Reports platform acceptance, failure, or an unknown outcome; never retry an unknown send.", botSchema(map[string]any{"target_type": map[string]any{"type": "string", "enum": []string{"user", "group"}}, "recipient": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "target_type", "recipient", "content")},
}

// ServeBotMCP exposes the current group bot's forwarding operations to one
// claimed Agent attempt. It never grants the model direct platform credentials.
func ServeBotMCP(ctx context.Context, home, taskID, attemptID string, in io.Reader, out io.Writer) error {
	if !filepath.IsAbs(home) || taskID == "" || attemptID == "" {
		return core.Fail("invalid_input", "bot MCP requires an absolute home and a claimed task attempt")
	}
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		return err
	}
	defer store.Close()
	return (&botMCP{store: store, directory: dws.New(), sender: dingtalkapp.New(), taskID: taskID, attemptID: attemptID}).serve(ctx, in, out)
}

func (m *botMCP) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewScanner(in)
	reader.Buffer(make([]byte, 4096), 1<<20)
	writer := json.NewEncoder(out)
	for reader.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(reader.Bytes(), &request) != nil || request.JSONRPC != "2.0" || len(request.ID) == 0 || string(request.ID) == "null" {
			continue
		}
		var result any
		var rpcError any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "memgov-bot", "version": "1.0.0"}}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			result = map[string]any{"tools": botMCPTools}
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(request.Params, &params) != nil {
				rpcError = map[string]any{"code": -32602, "message": "invalid tool call"}
				break
			}
			value, err := m.call(ctx, params.Name, params.Arguments)
			if err != nil {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}}
				break
			}
			raw, _ := json.Marshal(value)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(raw)}}, "structuredContent": value}
		default:
			rpcError = map[string]any{"code": -32601, "message": "method not found"}
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		if rpcError != nil {
			response["error"] = rpcError
		} else {
			response["result"] = result
		}
		if err := writer.Encode(response); err != nil {
			return err
		}
	}
	if reader.Err() != nil {
		return core.Fail("unavailable", "bot MCP input failed")
	}
	return nil
}

func decodeBotArguments(raw json.RawMessage, dest any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return core.Fail("invalid_input", "invalid bot tool arguments")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return core.Fail("invalid_input", "invalid bot tool arguments")
	}
	return nil
}

func (m *botMCP) current(ctx context.Context) (core.RuntimeConfig, core.Channel, core.Channel, error) {
	task, err := core.ReadRuntimeTask(ctx, m.store.DB, m.taskID)
	if err != nil {
		return core.RuntimeConfig{}, core.Channel{}, core.Channel{}, err
	}
	cfg, err := core.ReadRuntime(ctx, m.store.DB, task.RuntimeID)
	if err != nil {
		return cfg, core.Channel{}, core.Channel{}, err
	}
	app, err := core.ReadChannel(ctx, m.store.DB, cfg.ChannelID)
	if err != nil {
		return cfg, app, core.Channel{}, err
	}
	if task.Status != "running" || cfg.Status != "running" || cfg.ApplicationMode != "group_mention" || app.Kind != core.ChannelDingTalkApp || !app.Capabilities.Verified["send"] || app.Status != "active" && app.Status != "configured" {
		return cfg, app, core.Channel{}, core.Fail("denied", "bot MCP requires an active group bot attempt")
	}
	route, err := core.ReadRoute(ctx, m.store.DB, task.RouteID)
	if err != nil || route.ChannelID != app.ID || route.ConversationType != "group" || route.Mode != "assistant" || route.Status != "active" {
		return cfg, app, core.Channel{}, core.Fail("denied", "bot MCP task is outside an active group route")
	}
	mounted := false
	for _, id := range cfg.RouteIDs {
		mounted = mounted || id == route.ID
	}
	if !mounted {
		return cfg, app, core.Channel{}, core.Fail("denied", "bot MCP task group is not mounted")
	}
	if err = core.RuntimeAttemptPolicyCurrent(ctx, m.store.DB, m.attemptID, task.ID, task.Version); err != nil {
		return cfg, app, core.Channel{}, err
	}
	history, err := core.ReadChannel(ctx, m.store.DB, app.Identity.HistoryChannel)
	if err != nil || history.Kind != core.ChannelDwsPersonal || history.Tenant != app.Tenant || history.Identity.Profile == "" || !strings.EqualFold(history.Status, "active") && history.Status != "configured" {
		return cfg, app, history, core.Fail("denied", "bot recipient lookup requires a bound same-tenant DWS directory")
	}
	if err := m.directory.AttestOwner(ctx, history); err != nil {
		return cfg, app, history, err
	}
	return cfg, app, history, nil
}

func (m *botMCP) resolveGroup(ctx context.Context, cfg core.RuntimeConfig, app, history core.Channel, query string) (channel.GroupConversation, error) {
	groups, err := m.directory.ListBotGroups(ctx, channel.ConfigFor(history))
	if err != nil {
		return channel.GroupConversation{}, err
	}
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 200 {
		return channel.GroupConversation{}, core.Fail("invalid_input", "group name or ID is required")
	}
	var matches []channel.GroupConversation
	for _, group := range groups {
		if group.ID != query && group.Name != query {
			continue
		}
		route, routeErr := core.RouteFor(ctx, m.store.DB, app.ID, group.ID)
		if routeErr == nil && route.Status == "active" && route.ConversationType == "group" && route.Mode == "assistant" {
			for _, id := range cfg.RouteIDs {
				if id == route.ID {
					matches = append(matches, group)
					break
				}
			}
		}
	}
	if len(matches) != 1 {
		return channel.GroupConversation{}, core.Fail("denied", "group name is missing, ambiguous, or not mounted on this bot")
	}
	return matches[0], nil
}

func (m *botMCP) call(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	if name != "resolve_bot_user" && name != "resolve_bot_group" && name != "forward_bot_message" {
		return nil, core.Fail("invalid_input", "bot tool is not enabled")
	}
	cfg, app, history, err := m.current(ctx)
	if err != nil {
		return nil, err
	}
	switch name {
	case "resolve_bot_user":
		var args struct {
			Name string `json:"name"`
		}
		if err = decodeBotArguments(raw, &args); err != nil {
			return nil, err
		}
		return m.directory.ResolveBotUser(ctx, channel.ConfigFor(history), args.Name)
	case "resolve_bot_group":
		var args struct {
			NameOrID string `json:"name_or_id"`
		}
		if err = decodeBotArguments(raw, &args); err != nil {
			return nil, err
		}
		return m.resolveGroup(ctx, cfg, app, history, args.NameOrID)
	case "forward_bot_message":
		var args struct {
			TargetType string `json:"target_type"`
			Recipient  string `json:"recipient"`
			Content    string `json:"content"`
		}
		if err = decodeBotArguments(raw, &args); err != nil {
			return nil, err
		}
		if strings.TrimSpace(args.Content) == "" || len([]rune(args.Content)) > 20000 {
			return nil, core.Fail("invalid_input", "forwarded message content is required and must be bounded")
		}
		targetID, recipientName := "", ""
		switch args.TargetType {
		case "user":
			user, lookupErr := m.directory.ResolveBotUser(ctx, channel.ConfigFor(history), args.Recipient)
			if lookupErr != nil {
				return nil, lookupErr
			}
			targetID = user.ID
			recipientName = user.Name
			if recipientName == "" {
				recipientName = strings.TrimSpace(args.Recipient)
			}
		case "group":
			group, lookupErr := m.resolveGroup(ctx, cfg, app, history, args.Recipient)
			if lookupErr != nil {
				return nil, lookupErr
			}
			targetID = group.ID
			recipientName = group.Name
		default:
			return nil, core.Fail("invalid_input", "bot forward target must be user or group")
		}
		proof := core.RuntimeBotTargetVerification{ChannelID: app.ID, ChannelVersion: app.ConfigVersion, Tenant: app.Tenant, TargetType: args.TargetType, TargetID: targetID}
		input := core.RuntimeMessageInput{AttemptID: m.attemptID, IdempotencyKey: core.Hash([]byte(core.JSON(map[string]string{"task": m.taskID, "target_type": args.TargetType, "target_id": targetID, "content": args.Content}))), TargetType: args.TargetType, TargetID: targetID, Content: args.Content, Reason: "group_bot_forward"}
		var action core.RuntimeMessageAction
		var created bool
		_, err = m.store.Mutate(ctx, core.Request{ID: core.NewID(), Scope: "global", Command: "runtime.bot.forward.authorize", Actor: "bot_mcp"}, func(tx *core.Tx) (any, error) {
			var e error
			action, created, e = tx.AuthorizeRuntimeBotMessage(ctx, m.taskID, input, proof)
			return action, e
		})
		if err != nil {
			return nil, err
		}
		if !created {
			return map[string]any{"action_id": action.ID, "status": action.State, "target_type": args.TargetType, "target_id": targetID, "recipient": recipientName}, nil
		}
		transport := "bot_dm"
		if args.TargetType == "group" {
			transport = "bot_group"
		}
		result, sendErr := m.sender.Send(ctx, channel.ConfigFor(app), channel.SendRequest{ConversationID: targetID, Transport: transport, Content: args.Content, Format: "markdown", IdempotencyKey: action.ID})
		state := result.State
		if sendErr != nil && state != "failed" && state != "blocked" || state == "accepted" && result.Receipt == "" || state != "accepted" && state != "failed" && state != "blocked" {
			state = "unknown"
		}
		if state == "blocked" {
			state = "failed"
		}
		if sendErr != nil && result.Detail == "" {
			result.Detail = sendErr.Error()
		}
		_, err = m.store.Mutate(ctx, core.Request{ID: core.NewID(), Scope: "global", Command: "runtime.bot.forward.record", Actor: "bot_mcp"}, func(tx *core.Tx) (any, error) {
			return tx.RecordRuntimeMessageResult(ctx, action.ID, state, result.Receipt, result.Detail)
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"action_id": action.ID, "status": state, "target_type": args.TargetType, "target_id": targetID, "recipient": recipientName}, nil
	}
	return nil, fmt.Errorf("unknown bot tool")
}
