package channel

import (
	"context"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestDirectBackfillUpdatesPeerAndOwnerSideWithoutExpiredImports(t *testing.T) {
	ctx := context.Background()
	a := &fakeAdapter{caps: core.Capabilities{Verified: map[string]bool{"receive": true, "history": true}}}
	c, stored := testCollector(t, a)
	direct, history := true, false
	var source core.DataSource
	_, err := c.Store.Mutate(ctx, core.Request{Scope: "global", Command: "direct.fixture"}, func(tx *core.Tx) (any, error) {
		if _, err := tx.SetChannelCapabilities(ctx, stored.ID, a.caps, "fake"); err != nil {
			return nil, err
		}
		var err error
		source, err = tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: "work", Channel: stored.ID, Workspace: "global", DirectEnabled: &direct, HistoryEnabled: &history})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetDataSourceStatus(ctx, source.ID, "running"); err != nil {
			return nil, err
		}
		return tx.AdmitDataSourceDirectConversation(ctx, source.ID, "cid:peer", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	start, _ := time.Parse(time.RFC3339, source.DirectEnabledAt)
	makeEvent := func(id, sender, name string, self bool, stamp time.Time) core.NormalizedEvent {
		return core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dws", ParseVersion: "dws-history/3", Origin: "history", ConversationID: "cid:peer", ConversationType: "direct", ProviderMessageID: id, Sender: core.Sender{IDType: "open_id", IDValue: sender, DisplayName: name, SelfAuthor: self}, Body: id, SentAt: stamp.Format(time.RFC3339Nano), EventAt: stamp.Format(time.RFC3339Nano)}
	}
	a.windows = []WindowResult{{Complete: true, Events: []core.NormalizedEvent{
		makeEvent("expired", "peer", "Peer", false, start.AddDate(0, 0, -8)),
		makeEvent("request", "peer", "Peer", false, start.Add(time.Second)),
		makeEvent("done", "owner-open", "Owner", true, start.Add(2*time.Second)),
	}}}
	c.DirectSourceID = source.ID
	c.EarliestSentAt = start
	result, err := c.Pull(ctx, core.Request{Scope: "global", Actor: "test"}, stored.ID, "cid:peer", start, start.Add(time.Minute), 5, 200)
	if err != nil || result.Applied != 2 || !result.Complete {
		t.Fatalf("direct pull: %+v %v", result, err)
	}
	peer, err := core.ReadDirectContact(ctx, c.Store.DB, stored.ID, "cid:peer")
	if err != nil || peer.PeerIDValue != "peer" || peer.DisplayName != "Peer" {
		t.Fatalf("peer overwritten by own send: %+v %v", peer, err)
	}
	query, err := core.MessageQuery(ctx, c.Store.DB, stored.ID, core.MessageQueryInput{ConversationType: "direct", ContactIDType: "open_id", ContactIDValue: "peer", Limit: 10})
	if err != nil || len(query.Messages) != 2 || !query.Messages[0].SelfAuthored || query.Messages[1].SelfAuthored {
		t.Fatalf("two-sided local query: %+v %v", query, err)
	}
}
