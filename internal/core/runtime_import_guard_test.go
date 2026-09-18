package core

import (
	"context"
	"testing"
	"time"
)

func TestRuntimeImportedContextRemainsContextAfterEdit(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	at := time.Now().Add(time.Second)
	m := intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "imported-context", f.owner, "historical promise", at)
	runtimeMutate(t, f.s, "test.import-mark", func(tx *Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, "UPDATE messages SET context_only=1 WHERE id=?", m.MessageID)
		return nil, err
	})
	sync := func() {
		t.Helper()
		runtimeMutate(t, f.s, "test.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, f.config.ID) })
		var state string
		if err := f.s.DB.QueryRowContext(ctx, "SELECT state FROM runtime_message_states WHERE runtime_id=? AND message_id=?", f.config.ID, m.MessageID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "context" {
			t.Fatalf("import unexpectedly triggered AI: %s", state)
		}
	}
	sync()
	intakeRuntimeMessage(t, f.s, f.channel, f.watch.ConversationID, "imported-context", f.owner, "edited historical promise", at)
	sync()
}

func TestRuntimeExplicitEmptyCapabilitiesStayEmpty(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	runtimeMutate(t, f.s, "test.stop", func(tx *Tx) (any, error) { return tx.SetRuntimeStatus(ctx, f.config.ID, "stopped", "") })
	runtimeMutate(t, f.s, "test.configure-empty", func(tx *Tx) (any, error) {
		current, err := ReadRuntime(ctx, tx.Conn, f.config.ID)
		if err != nil {
			return nil, err
		}
		return tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: current.Name, Channel: f.channel.ID, RouteIDs: []string{f.watch.ID}, DeliveryRouteID: f.direct.ID, Owner: f.owner, AgentCapabilities: []string{}, ExpectedVersion: current.Version})
	})
	current, err := ReadRuntime(ctx, f.s.DB, f.config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.AgentCapabilities == nil || len(current.AgentCapabilities) != 0 {
		t.Fatalf("explicit empty capability list expanded: %v", current.AgentCapabilities)
	}
}
