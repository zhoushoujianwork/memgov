package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

type runtimeOwnerMessageAdapter interface {
	AttestOwner(context.Context, core.Channel) error
	VerifyOwnerMessageTarget(context.Context, core.Channel, string, string) error
}

func (a *app) runtimeMessageCmd() *cobra.Command {
	root := &cobra.Command{Use: "message", Short: "Cyber owner 自主沟通与投递记录"}
	root.AddCommand(a.read("list <task-id>", "读取 Agent 沟通动作和投递状态", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.RuntimeMessageActions(ctx, s.DB, args[0])
	}))
	root.AddCommand(&cobra.Command{Use: "send <task-id>", Short: "按当前 owner 委托发送 Agent 明确决定的消息", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), a.timeout)
		defer cancel()
		raw, err := a.payload()
		if err != nil {
			return err
		}
		var in core.RuntimeMessageInput
		if err = decode(raw, &in); err != nil {
			return err
		}
		s, err := core.Open(ctx, a.dbPath(), false)
		if err != nil {
			return err
		}
		defer s.Close()
		if prior, exists, e := core.ExistingRuntimeMessageAction(ctx, s.DB, args[0], in); e != nil {
			return e
		} else if exists {
			return a.emit(prior, true)
		}
		t, err := core.ReadRuntimeTask(ctx, s.DB, args[0])
		if err != nil {
			return err
		}
		cfg, err := core.ReadRuntime(ctx, s.DB, t.RuntimeID)
		if err != nil {
			return err
		}
		c, err := core.ReadChannel(ctx, s.DB, cfg.ChannelID)
		if err != nil {
			return err
		}
		adapter, err := a.adapterFor(c)
		if err != nil {
			return err
		}
		owner, ok := adapter.(runtimeOwnerMessageAdapter)
		if !ok {
			return core.Fail("unavailable", "selected adapter cannot verify owner communication")
		}
		if err = owner.AttestOwner(ctx, c); err != nil {
			return err
		}
		localTarget := false
		if in.TargetType == "group" {
			for _, r := range c.Routes {
				if r.ConversationID == in.TargetID && r.ConversationType == "group" && r.Status == "active" && r.Mode != "ignore" {
					localTarget = true
					break
				}
			}
		}
		if !localTarget {
			if err = owner.VerifyOwnerMessageTarget(ctx, c, in.TargetType, in.TargetID); err != nil {
				return err
			}
		}
		verified := core.RuntimeMessageTargetVerification{ChannelID: c.ID, ChannelVersion: c.ConfigVersion, OwnerProfile: c.Identity.Profile, OwnerUserID: c.Identity.ExpectedUserID, TargetType: in.TargetType, TargetID: in.TargetID}
		var action core.RuntimeMessageAction
		var created bool
		_, err = s.Mutate(ctx, core.Request{Command: "runtime.message.authorize", Scope: "global", Actor: a.actor, Input: map[string]any{"task_id": t.ID, "input_digest": core.Digest(in)}}, func(tx *core.Tx) (any, error) {
			// Persist the authenticated read proof, tied to this exact channel
			// version; an observed platform sender is not owner attestation.
			if _, e := tx.AttestDWSOwner(ctx, c.ID, c.ConfigVersion); e != nil {
				return nil, e
			}
			var e error
			action, created, e = tx.AuthorizeRuntimeMessageAction(ctx, t.ID, in, verified)
			return map[string]any{"action_id": action.ID, "created": created}, e
		})
		if err != nil {
			return err
		}
		if !created {
			return a.emit(action, true)
		}
		transport := "user_group"
		if action.TargetType == "user" {
			transport = "user_dm"
		}
		result, sendErr := adapter.Send(ctx, channel.ConfigFor(c), channel.SendRequest{ConversationID: action.TargetID, Transport: transport, Content: action.Content, Format: "markdown", IdempotencyKey: action.IdempotencyKey})
		state := result.State
		if state == "blocked" {
			state = "failed"
		} else if state != "accepted" && state != "failed" {
			state = "unknown"
		}
		if sendErr != nil {
			result.Detail = sendErr.Error()
		}
		// Persist timeouts with a detached bounded write; never invoke DWS twice.
		recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer recordCancel()
		_, err = s.Mutate(recordCtx, core.Request{Command: "runtime.message.result", Scope: "global", Actor: a.actor, Input: map[string]any{"action_id": action.ID, "state": state}}, func(tx *core.Tx) (any, error) {
			var e error
			action, e = tx.RecordRuntimeMessageResult(recordCtx, action.ID, state, result.Receipt, result.Detail)
			return map[string]any{"action_id": action.ID, "state": action.State}, e
		})
		if err != nil {
			return err
		}
		return a.emit(action, false)
	}})
	return root
}
