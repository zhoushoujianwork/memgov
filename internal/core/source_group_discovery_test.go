package core

import (
	"context"
	"testing"
	"time"
)

func TestSourceDiscoveryReceiptRequiresFrozenConfiguration(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	var c Channel
	var d DataSource
	runtimeMutate(t, s, "receipt.configure", func(tx *Tx) (any, error) {
		var err error
		c, err = tx.AddChannel(ctx, ChannelInput{Name: "receipt-dws", Kind: ChannelDwsPersonal, Identity: ChannelIdentity{ExpectedCorpID: "corp", ExpectedUserID: "user", DeliveryRobotCode: "bot", DeliveryRobotName: "Bot"}})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetChannelCapabilities(ctx, c.ID, Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); err != nil {
			return nil, err
		}
		d, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "receipt-source", Channel: c.ID, Workspace: "global", MemberRobotCode: "bot"})
		return d, err
	})
	c, _ = ReadChannel(ctx, s.DB, c.ID)
	runtimeMutate(t, s, "receipt.local-routes", func(tx *Tx) (any, error) {
		return tx.SyncDataSourceGroupDetails(ctx, d.ID, []DataSourceGroup{{ID: "group"}})
	})
	if _, err := ReadSourceGroupDiscovery(ctx, s.DB, d.ID); ErrorCode(err) != "not_found" {
		t.Fatal("local route sync minted provider evidence")
	}
	runtimeMutate(t, s, "receipt.provider", func(tx *Tx) (any, error) {
		return nil, tx.RecordSourceGroupDiscovery(ctx, d.ID, d.Version, c.ConfigVersion, []DataSourceGroup{{ID: "z", Name: "Z"}, {ID: "a", Name: "A"}})
	})
	receipt, err := ReadSourceGroupDiscovery(ctx, s.DB, d.ID)
	if err != nil || receipt.RobotCode != "bot" || receipt.RobotName != "Bot" || len(receipt.Groups) != 2 || receipt.Groups[0].ID != "a" {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
	for _, versions := range [][2]int{{d.Version + 1, c.ConfigVersion}, {d.Version, c.ConfigVersion + 1}} {
		_, err = s.Mutate(ctx, Request{Scope: "global", Command: "receipt.stale"}, func(tx *Tx) (any, error) {
			return nil, tx.RecordSourceGroupDiscovery(ctx, d.ID, versions[0], versions[1], nil)
		})
		if ErrorCode(err) != "conflict" {
			t.Fatalf("stale discovery was recorded: %v", err)
		}
	}
	current, err := ReadSourceGroupDiscovery(ctx, s.DB, d.ID)
	if err != nil || Digest(current) != Digest(receipt) {
		t.Fatal("failed discovery replaced last complete observation")
	}
	runtimeMutate(t, s, "receipt.failed", func(tx *Tx) (any, error) { return nil, tx.InvalidateSourceGroupDiscovery(ctx, d.ID) })
	invalid, err := ReadSourceGroupDiscovery(ctx, s.DB, d.ID)
	if err != nil || invalid.Valid || len(invalid.Groups) != 2 {
		t.Fatal("failed discovery did not invalidate previous authorization evidence")
	}
	runtimeMutate(t, s, "receipt.empty-complete", func(tx *Tx) (any, error) {
		return nil, tx.RecordSourceGroupDiscovery(ctx, d.ID, d.Version, c.ConfigVersion, []DataSourceGroup{})
	})
	current, err = ReadSourceGroupDiscovery(ctx, s.DB, d.ID)
	if err != nil || !current.Valid || len(current.Groups) != 0 {
		t.Fatal("empty complete result failed to remove memberships")
	}
}

func TestPartialSourceProofPreservesScopeWithoutRenewingOldGroups(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	var c Channel
	var source DataSource
	runtimeMutate(t, s, "partial.configure", func(tx *Tx) (any, error) {
		var err error
		c, err = tx.AddChannel(ctx, ChannelInput{Name: "partial-dws", Kind: ChannelDwsPersonal, Identity: ChannelIdentity{ExpectedCorpID: "corp", ExpectedUserID: "owner", DeliveryRobotCode: "bot", DeliveryRobotName: "Bot"}})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetChannelCapabilities(ctx, c.ID, Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); err != nil {
			return nil, err
		}
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "partial-source", Channel: c.ID, Workspace: "global", MemberRobotCode: "bot"})
		return source, err
	})
	c, _ = ReadChannel(ctx, s.DB, c.ID)
	runtimeMutate(t, s, "partial.initial", func(tx *Tx) (any, error) {
		return tx.SyncDataSourceGroupDetails(ctx, source.ID, []DataSourceGroup{{ID: "old"}, {ID: "left"}, {ID: "never"}})
	})
	source, _ = ReadDataSource(ctx, s.DB, source.ID)
	if routes, err := ReadSourcePositiveProcessingRoutes(ctx, s.DB, source, time.Now()); err != nil || len(routes) != 0 {
		t.Fatalf("legacy source scope authorized processing without a receipt: routes=%v error=%v", routes, err)
	}
	runtimeMutate(t, s, "partial.observation", func(tx *Tx) (any, error) {
		groups := []DataSourceGroup{{ID: "fresh"}}
		if _, err := tx.ApplySourceGroupObservation(ctx, source.ID, groups, false, []string{"left"}); err != nil {
			return nil, err
		}
		return nil, tx.RecordSourceGroupObservation(ctx, source.ID, source.Version, c.ConfigVersion, groups, false)
	})
	source, _ = ReadDataSource(ctx, s.DB, source.ID)
	if len(source.RouteIDs) != 3 {
		t.Fatalf("partial result shrank unobserved scope: %+v", source)
	}
	ids := map[string]bool{}
	for _, id := range source.RouteIDs {
		route, err := ReadRoute(ctx, s.DB, id)
		if err != nil {
			t.Fatal(err)
		}
		ids[route.ConversationID] = true
	}
	if !ids["old"] || !ids["never"] || !ids["fresh"] || ids["left"] {
		t.Fatalf("wrong merged scope: %v", ids)
	}
	receipt, err := ReadSourceGroupDiscovery(ctx, s.DB, source.ID)
	if err != nil || !receipt.Valid || receipt.Complete || len(receipt.Groups) != 1 || receipt.Groups[0].ID != "fresh" {
		t.Fatalf("old route gained fresh evidence: %+v %v", receipt, err)
	}
	processing, err := ReadSourcePositiveProcessingRoutes(ctx, s.DB, source, time.Now())
	if err != nil || len(processing) != 1 {
		t.Fatalf("partial proof widened AI consumer to legacy routes: routes=%v error=%v", processing, err)
	}
	positive, err := ReadRoute(ctx, s.DB, processing[0])
	if err != nil || positive.ConversationID != "fresh" {
		t.Fatalf("AI consumer did not select the one fresh group: route=%+v error=%v", positive, err)
	}
	runtimeMutate(t, s, "partial.no-proof", func(tx *Tx) (any, error) {
		return nil, tx.RecordSourceGroupObservation(ctx, source.ID, source.Version, c.ConfigVersion, nil, false)
	})
	invalid, _ := ReadSourceGroupDiscovery(ctx, s.DB, source.ID)
	if invalid.Valid || invalid.ObservedAt != receipt.ObservedAt {
		t.Fatalf("empty partial renewed evidence: %+v", invalid)
	}
	processing, err = ReadSourcePositiveProcessingRoutes(ctx, s.DB, source, time.Now())
	if err != nil || len(processing) != 0 {
		t.Fatalf("inactive source used a transient receipt for new processing: routes=%v error=%v", processing, err)
	}
}
