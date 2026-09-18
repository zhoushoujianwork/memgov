package core

import (
	"context"
	"testing"
)

func TestRuntimeReconfigureRevokesPendingWorkAndReadyDelivery(t *testing.T) {
	f := newRuntimeFixture(t, 20, 300)
	ctx := context.Background()
	runtimeMutate(t, f.s, "test.stop-before-config", func(tx *Tx) (any, error) { return tx.SetRuntimeStatus(ctx, f.config.ID, "stopped", "") })
	_, err := f.s.DB.ExecContext(ctx, "INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,kind,title,instructions,status,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", "task-old", f.config.ID, f.watch.ID, "config-old", "task", "Old", "old instructions", "pending", 1, Now(), Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.DB.ExecContext(ctx, "INSERT INTO runtime_pending_actions(id,task_id,task_version,kind,target,payload,payload_digest,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)", "action-old", "task-old", 1, "push", "repo", "git push", Hash([]byte("git push")), "pending", Now(), Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.DB.ExecContext(ctx, "INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,content,input_digest,send_policy,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", "delivery-old", f.channel.ID, f.direct.ID, f.direct.Version, "task-old", f.direct.ConversationID, f.direct.AudienceKey, "Old result", "old-digest", "dispatch_only", "ready", Now(), Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []string{RuntimeReceiptPurpose, RuntimeProcessingReceiptPurpose, RuntimeCompletionReceiptPurpose, RuntimeFailureReceiptPurpose, "result", "confirmation"} {
		_, err = f.s.DB.ExecContext(ctx, `INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,content,input_digest,send_policy,state,reason,created_at,updated_at)
SELECT ?,channel_id,route_id,route_version,job_id,conversation_id,audience_key,content,?,'dispatch_only','sending',?,created_at,updated_at FROM outbox WHERE id='delivery-old'`, purpose, purpose, purpose)
		if err != nil {
			t.Fatal(err)
		}
	}
	runtimeMutate(t, f.s, "test.reconfigure", func(tx *Tx) (any, error) {
		current, err := ReadRuntime(ctx, tx.Conn, f.config.ID)
		if err != nil {
			return nil, err
		}
		return tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: current.Name, Channel: f.channel.ID, RouteIDs: []string{f.watch.ID}, DeliveryRouteID: f.direct.ID, Owner: f.owner, AgentCapabilities: []string{"memory_read"}, ExpectedVersion: current.Version})
	})
	var status string
	var version int
	if err = f.s.DB.QueryRowContext(ctx, "SELECT status,version FROM runtime_tasks WHERE id='task-old'").Scan(&status, &version); err != nil || status != "stale" || version != 2 {
		t.Fatalf("old task retained permission: %s %d %v", status, version, err)
	}
	if err = f.s.DB.QueryRowContext(ctx, "SELECT status FROM runtime_pending_actions WHERE id='action-old'").Scan(&status); err != nil || status != "stale" {
		t.Fatalf("action remained confirmable: %s %v", status, err)
	}
	if err = f.s.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE id='delivery-old'").Scan(&status); err != nil || status != "stale" {
		t.Fatalf("old result remained deliverable: %s %v", status, err)
	}
	for _, purpose := range []string{RuntimeReceiptPurpose, RuntimeProcessingReceiptPurpose, RuntimeCompletionReceiptPurpose, RuntimeFailureReceiptPurpose, "result", "confirmation"} {
		var reason string
		if err = f.s.DB.QueryRowContext(ctx, "SELECT state,reason FROM outbox WHERE id=?", purpose).Scan(&status, &reason); err != nil || status != "unknown" || reason != purpose {
			t.Fatalf("invalidation lost delivery purpose: state=%s purpose=%s err=%v", status, reason, err)
		}
	}
}
