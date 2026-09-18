package dws

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// VerifyOwnerMessageTarget resolves stable IDs under the sending profile. Group
// names and user display names are never accepted as identity evidence.
func (a *Adapter) VerifyOwnerMessageTarget(ctx context.Context, c core.Channel, targetType, targetID string) error {
	cfg := channel.ConfigFor(c)
	switch targetType {
	case "group":
		// Target resolution uses the complete directory, not observation's
		// recent-activity or robot-membership filters.
		raw, err := a.run(ctx, withProfile(cfg, []string{"chat", "+chat-list-all", "--limit", "200", "--page-all", "--page-limit", "50"})...)
		if err != nil {
			return err
		}
		var directory struct {
			Groups []struct {
				ID string `json:"openConversationId"`
			} `json:"groups"`
			Complete    bool `json:"complete"`
			HasMore     bool `json:"hasMore"`
			Partial     bool `json:"partial"`
			FailedCount int  `json:"failedCount"`
		}
		if json.Unmarshal(raw, &directory) != nil || !directory.Complete || directory.HasMore || directory.Partial || directory.FailedCount > 0 {
			return core.Fail("unavailable", "DWS group directory did not return a complete verified result")
		}
		for _, group := range directory.Groups {
			if group.ID == targetID {
				return nil
			}
		}
		return core.Fail("denied", "target is not an exact group ID visible to this DWS owner")
	case "user":
		// DWS documents that --user performs exact userId directory resolution
		// even in dry-run. It does not fall back to --user-query or a nickname.
		raw, err := a.run(ctx, withProfile(cfg, []string{"chat", "+messages-send", "--as", "user", "--user", targetID, "--text", "memgov target verification", "--dry-run"})...)
		if err != nil {
			return err
		}
		var preview struct {
			DryRun      bool `json:"dry_run"`
			Executed    bool `json:"executed"`
			ActionCount int  `json:"actionCount"`
		}
		if json.Unmarshal(raw, &preview) != nil || !preview.DryRun || preview.Executed || preview.ActionCount != 1 {
			return core.Fail("denied", "DWS did not verify one exact user target without execution")
		}
		return nil
	default:
		return core.Fail("invalid_input", "owner message target must be group or user")
	}
}

func (a *Adapter) sendOwnerMessage(ctx context.Context, cfg channel.Config, req channel.SendRequest) (channel.SendResult, error) {
	if cfg.Identity.Profile == "" || cfg.Identity.ExpectedUserID == "" || cfg.Tenant == "" || req.ConversationID == "" || req.IdempotencyKey == "" || strings.TrimSpace(req.Content) == "" {
		return channel.SendResult{State: "blocked"}, core.Fail("invalid_input", "owner send requires a fixed profile, owner, stable target and idempotency key")
	}
	if err := a.AttestOwner(ctx, core.Channel{Kind: cfg.Kind, Tenant: cfg.Tenant, Identity: cfg.Identity}); err != nil {
		return channel.SendResult{State: "blocked"}, err
	}
	targetFlag := "--group"
	if req.Transport == "user_dm" {
		targetFlag = "--user"
	}
	contentFlag := "--markdown"
	if req.Format == "text" {
		contentFlag = "--text"
	}
	args := []string{"chat", "+messages-send", "--as", "user", targetFlag, req.ConversationID, contentFlag, req.Content, "--ai-tag", "true", "--idempotency-key", req.IdempotencyKey, "--yes"}
	raw, err := a.run(ctx, withProfile(cfg, args)...)
	if err != nil {
		return channel.SendResult{State: "unknown"}, err
	}
	var result struct {
		OpenTaskID string `json:"openTaskId"`
		Failed     int    `json:"failedCount"`
		Result     struct {
			OpenTaskID string `json:"openTaskId"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return channel.SendResult{State: "unknown", Detail: "unrecognized owner-send receipt"}, nil
	}
	if result.Failed > 0 {
		return channel.SendResult{State: "failed", Receipt: string(raw), Detail: "platform reported a failed target"}, nil
	}
	taskID := result.OpenTaskID
	if taskID == "" {
		taskID = result.Result.OpenTaskID
	}
	if taskID == "" {
		return channel.SendResult{State: "unknown", Receipt: string(raw), Detail: "platform did not acknowledge a user-send task"}, nil
	}
	// The acceptance task ID is not a message ID. Read status once to recover
	// provider evidence for echo suppression; never repeat the send operation.
	status, readErr := a.ReadOwnerMessageStatus(ctx, cfg, string(raw))
	if readErr != nil {
		return channel.SendResult{State: "accepted", Receipt: string(raw), Detail: "accepted; provider message identity is not yet verified"}, nil
	}
	return status, nil
}

func (a *Adapter) ReadOwnerMessageStatus(ctx context.Context, cfg channel.Config, receipt string) (channel.SendResult, error) {
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(receipt), &value) != nil {
		return channel.SendResult{}, core.Fail("invalid_input", "owner receipt is not structured JSON")
	}
	send := json.RawMessage(receipt)
	if saved, ok := value["send"]; ok {
		send = saved
	}
	var result struct {
		OpenTaskID string `json:"openTaskId"`
		Result     struct {
			OpenTaskID string `json:"openTaskId"`
		} `json:"result"`
	}
	if json.Unmarshal(send, &result) != nil {
		return channel.SendResult{}, core.Fail("invalid_input", "owner receipt does not contain a send task")
	}
	taskID := result.OpenTaskID
	if taskID == "" {
		taskID = result.Result.OpenTaskID
	}
	if taskID == "" {
		return channel.SendResult{}, core.Fail("unavailable", "no verified send task ID is available for reconciliation")
	}
	status, err := a.run(ctx, withProfile(cfg, []string{"chat", "message", "query-send-status", "--open-task-id", taskID})...)
	if err != nil {
		return channel.SendResult{}, err
	}
	updated, _ := json.Marshal(map[string]any{"send": send, "delivery": json.RawMessage(status)})
	state, detail := ownerDeliveryState(status)
	return channel.SendResult{State: state, Receipt: string(updated), Detail: detail}, nil
}

// Native status enum numbers are not a reviewed contract. Use explicit failure
// evidence or actual provider message IDs; an unfamiliar/pending shape remains
// unresolved and is queried later, never guessed successful or resent.
func ownerDeliveryState(status json.RawMessage) (string, string) {
	var raw any
	if json.Unmarshal(status, &raw) != nil {
		return "unknown", "unrecognized owner delivery status"
	}
	failed, delivered := false, false
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				switch k {
				case "failedCount":
					if n, ok := v.(float64); ok && n > 0 {
						failed = true
					}
				case "success":
					if success, ok := v.(bool); ok && !success {
						failed = true
					}
				case "openMessageId", "messageId", "msgId":
					if id, ok := v.(string); ok && id != "" {
						delivered = true
					}
				default:
					walk(v)
				}
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(raw)
	if failed {
		return "failed", "platform reported owner-message delivery failure"
	}
	if delivered {
		return "accepted", ""
	}
	return "unknown", "owner-message delivery is not yet verified; no resend"
}

var _ channel.OwnerMessageStatusReader = (*Adapter)(nil)
