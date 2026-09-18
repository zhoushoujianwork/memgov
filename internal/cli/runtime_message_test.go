package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

type ownerMessageStub struct {
	*stubAdapter
	sends    int
	verified int
	attested int
	home     string
	result   channel.SendResult
	req      channel.SendRequest
	cfg      channel.Config
}

func (s *ownerMessageStub) AttestOwner(context.Context, core.Channel) error { s.attested++; return nil }
func (s *ownerMessageStub) VerifyOwnerMessageTarget(context.Context, core.Channel, string, string) error {
	s.verified++
	return nil
}
func (s *ownerMessageStub) Send(ctx context.Context, cfg channel.Config, req channel.SendRequest) (channel.SendResult, error) {
	s.sends++
	s.req = req
	s.cfg = cfg
	db, err := core.Open(ctx, filepath.Join(s.home, "state.db"), false)
	if err != nil {
		return channel.SendResult{}, err
	}
	defer db.Close()
	var n int
	if err = db.DB.QueryRow("SELECT count(*) FROM runtime_message_actions WHERE state='sending' AND idempotency_key=?", req.IdempotencyKey).Scan(&n); err != nil {
		return channel.SendResult{}, err
	}
	if n != 1 {
		return channel.SendResult{}, core.Fail("conflict", "transport invoked before durable authorization")
	}
	return s.result, nil
}

func cliOwnerMessageFixture(t *testing.T) (string, string, core.RuntimeMessageInput) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	s, err := core.Open(ctx, filepath.Join(home, "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mutate := func(fn func(*core.Tx) (any, error)) {
		t.Helper()
		if _, e := s.Mutate(ctx, core.Request{Scope: "global", Command: "test.setup"}, fn); e != nil {
			t.Fatal(e)
		}
	}
	var ch core.Channel
	mutate(func(tx *core.Tx) (any, error) {
		var e error
		ch, e = tx.AddChannel(ctx, core.ChannelInput{Name: "cyber", Kind: core.ChannelDwsPersonal, Identity: core.ChannelIdentity{Profile: "corp:owner", ExpectedCorpID: "corp", ExpectedUserID: "owner"}, Route: &core.RouteInput{ConversationID: "cid:group", ConversationType: "group", Mode: "collect"}})
		return ch, e
	})
	mutate(func(tx *core.Tx) (any, error) { return tx.AttestDWSOwner(ctx, ch.ID, ch.ConfigVersion) })
	var cfg core.RuntimeConfig
	mutate(func(tx *core.Tx) (any, error) {
		var e error
		cfg, e = tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Name: "cyber", Channel: ch.ID, RouteIDs: []string{ch.Routes[0].ID}, Owner: core.Sender{IDType: "user_id", IDValue: "owner"}, ApplicationMode: "proactive", ExternalActions: "owner_delegated", ItemThreshold: 1})
		return cfg, e
	})
	mutate(func(tx *core.Tx) (any, error) { return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "") })
	var intake core.IntakeResult
	mutate(func(tx *core.Tx) (any, error) {
		var e error
		intake, e = tx.Intake(ctx, ch.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "history", ProviderMessageID: "observed", ConversationID: "cid:group", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "peer"}, Body: "Service issue needs investigation", SentAt: time.Now().Add(time.Second).Format(time.RFC3339Nano)})
		return intake, e
	})
	mutate(func(tx *core.Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, cfg.ID) })
	var batch core.RuntimeBatch
	mutate(func(tx *core.Tx) (any, error) {
		var e error
		batch, e = tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now().Add(time.Minute))
		return batch, e
	})
	mutate(func(tx *core.Tx) (any, error) {
		return tx.CompleteRuntimeBatch(ctx, batch, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "investigate", Title: "Investigate issue", Instructions: "Investigate and coordinate", MessageIDs: []string{intake.MessageID}}}})
	})
	var task core.RuntimeTask
	mutate(func(tx *core.Tx) (any, error) {
		var e error
		task, _, e = tx.ClaimRuntimeTask(ctx, cfg.ID, "attempt-cli", "model", "preset", "commit", "")
		return task, e
	})
	return home, task.ID, core.RuntimeMessageInput{AttemptID: "attempt-cli", IdempotencyKey: "coordinate-once", TargetType: "group", TargetID: "cid:group", Content: "I am investigating.", Reason: "Coordinate the issue investigation", EvidenceMessageIDs: []string{intake.MessageID}}
}

func TestRuntimeMessageCLIDurableOwnerSendAndUnknownDedupe(t *testing.T) {
	for _, state := range []string{"accepted", "unknown"} {
		t.Run(state, func(t *testing.T) {
			home, taskID, in := cliOwnerMessageFixture(t)
			adapter := &ownerMessageStub{stubAdapter: &stubAdapter{}, home: home, result: channel.SendResult{State: state, Receipt: `{"openTaskId":"send-task"}`}}
			for i := 0; i < 2; i++ {
				code, out := invokeWith(t, adapter, home, core.JSON(in), "runtime", "message", "send", taskID, "--input", "-")
				if code != 0 {
					t.Fatalf("message send %d: %+v", code, out)
				}
				if data(t, out)["state"] != state {
					t.Fatalf("wrong state %+v", out)
				}
			}
			if adapter.sends != 1 || adapter.attested != 1 || adapter.verified != 0 {
				t.Fatalf("sends=%d attested=%d directory=%d", adapter.sends, adapter.attested, adapter.verified)
			}
			if adapter.req.Transport != "user_group" || adapter.req.IdempotencyKey != in.IdempotencyKey || adapter.cfg.Identity.Profile != "corp:owner" || adapter.cfg.Identity.DeliveryRobotCode != "" {
				t.Fatalf("wrong owner transport %+v %+v", adapter.req, adapter.cfg)
			}
			in.Content = "changed payload"
			if code, _ := invokeWith(t, adapter, home, core.JSON(in), "runtime", "message", "send", taskID, "--input", "-"); code == 0 {
				t.Fatal("changed payload replay accepted")
			}
			if adapter.sends != 1 {
				t.Fatal("replayed write")
			}
		})
	}
}

func TestRuntimeMessageCLIVerifiesDifferentRecipient(t *testing.T) {
	home, taskID, in := cliOwnerMessageFixture(t)
	in.TargetType = "user"
	in.TargetID = "owner"
	adapter := &ownerMessageStub{stubAdapter: &stubAdapter{}, home: home, result: channel.SendResult{State: "accepted", Receipt: `{"openTaskId":"sent"}`}}
	code, out := invokeWith(t, adapter, home, core.JSON(in), "runtime", "message", "send", taskID, "--input", "-")
	if code != 0 {
		t.Fatalf("send: %+v", out)
	}
	if adapter.verified != 1 || adapter.req.Transport != "user_dm" || adapter.req.ConversationID != "owner" {
		t.Fatalf("target not verified: %+v", adapter)
	}
}
