package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestRecoveredDirectTaskIsFinalizedAndOwnerIsNotified(t *testing.T) {
	scenario := runDirectReplyScenario(t, "第一条已完成的请求", false, nil, false)
	ctx := context.Background()
	if _, err := scenario.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.recovery.intake"}, func(tx *core.Tx) (any, error) {
		return tx.Intake(ctx, scenario.cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/4", Origin: "stream", ProviderMessageID: "interrupted-direct-request", ConversationID: scenario.route.ConversationID, ConversationType: "direct", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "owner"}, Body: "这条请求执行到一半服务中断", SentAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)})
	}); err != nil {
		t.Fatal(err)
	}
	var batch core.RuntimeBatch
	if _, err := scenario.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.recovery.queue"}, func(tx *core.Tx) (any, error) {
		if _, err := tx.SyncRuntimeMessages(ctx, scenario.cfg.ID); err != nil {
			return nil, err
		}
		var claimErr error
		batch, claimErr = tx.ClaimRuntimeBatch(ctx, scenario.cfg.ID, time.Now().Add(3*time.Hour))
		return batch, claimErr
	}); err != nil {
		t.Fatal(err)
	}
	scenario.service.queueDirectTurn(ctx, scenario.cfg, batch)
	var task core.RuntimeTask
	if _, err := scenario.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.recovery.claim"}, func(tx *core.Tx) (any, error) {
		var claimErr error
		task, _, claimErr = tx.ClaimRuntimeTask(ctx, scenario.cfg.ID, core.NewID(), "model", "preset", "commit", "")
		return task, claimErr
	}); err != nil {
		t.Fatal(err)
	}
	if task.ID == "" {
		t.Fatal("direct task was not running before recovery")
	}
	if _, err := scenario.service.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.recovery.finalize"}, func(tx *core.Tx) (any, error) {
		return tx.RecoverRuntime(ctx, scenario.cfg.ID)
	}); err != nil {
		t.Fatal(err)
	}
	scenario.service.reconcileStageAcknowledgements(ctx, scenario.cfg)
	scenario.service.reconcileFailureNotices(ctx, scenario.cfg)
	stored, err := core.ReadRuntimeTask(ctx, scenario.service.Store.DB, task.ID)
	if err != nil || stored.Status != "failed" || stored.ErrorCode != "runtime_restarted" || len(stored.Attempts) != 1 || stored.Attempts[0].Status != "failed" {
		t.Fatalf("recovered task was not finalized: task=%+v err=%v", stored, err)
	}
	if scenario.adapter.sends != 2 {
		ids, scanErr := core.RuntimeDirectFailureNoticeTaskIDs(ctx, scenario.service.Store.DB, scenario.cfg.ID)
		var receiptState, noticeState string
		_ = scenario.service.Store.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE job_id=? AND reason=?", task.ID, core.RuntimeReceiptPurpose).Scan(&receiptState)
		_ = scenario.service.Store.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE job_id=? AND reason=?", task.ID, core.RuntimeFailureNoticePurpose).Scan(&noticeState)
		t.Fatalf("expected the normal first answer and one recovery notice, sends=%d ids=%v scan_err=%v receipt=%s notice=%s", scenario.adapter.sends, ids, scanErr, receiptState, noticeState)
	}
	var failureStage string
	if err = scenario.service.Store.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE job_id=? AND reason=?", task.ID, core.RuntimeFailureReceiptPurpose).Scan(&failureStage); err != nil || failureStage != "accepted" {
		t.Fatalf("direct failure stage was not persisted: state=%s err=%v", failureStage, err)
	}
	reactions := scenario.service.Adapter.(*directReactionAdapter)
	if len(scenario.adapter.reactions) == 0 || len(reactions.removed) == 0 {
		t.Fatalf("direct failure marker was not sent: reactions=%+v removed=%+v", scenario.adapter.reactions, reactions.removed)
	}
	if got := scenario.adapter.reactions[len(scenario.adapter.reactions)-1].Emoji; got != core.RuntimeFailureAcknowledgement || reactions.removed[len(reactions.removed)-1] != core.RuntimeAcknowledgement {
		t.Fatalf("direct failure marker did not replace receipt: reactions=%+v removed=%+v", scenario.adapter.reactions, reactions.removed)
	}
	request := scenario.adapter.requests[len(scenario.adapter.requests)-1]
	if request.Transport != "bot_dm" || !strings.Contains(request.Content, "服务现已恢复") || !strings.Contains(request.Content, "已标记为失败") {
		t.Fatalf("recovery notice did not close the direct task: %+v", request)
	}
	scenario.service.reconcileFailureNotices(ctx, scenario.cfg)
	if scenario.adapter.sends != 2 {
		t.Fatal("recovery notice was sent more than once")
	}
}
