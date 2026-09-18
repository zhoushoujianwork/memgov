package core

import (
	"context"
	"testing"
	"time"
)

func TestDataSourceDiscoveryPreservesIgnoreAndIndependentStatus(t *testing.T) {
	f := newRuntimeFixture(t, 20, 300)
	ctx := context.Background()
	var d DataSource
	_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "test.source"}, func(tx *Tx) (any, error) {
		var e error
		d, e = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "work", Channel: f.channel.ID, Workspace: "global"})
		return d, e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "test.attach"}, func(tx *Tx) (any, error) { return tx.BindRuntimeDataSource(ctx, f.config.ID, d.ID) })
	if ErrorCode(err) != "conflict" {
		t.Fatalf("running runtime attached: %v", err)
	}
	_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "test.move"}, func(tx *Tx) (any, error) {
		if _, e := tx.SetRuntimeStatus(ctx, f.config.ID, "stopped", ""); e != nil {
			return nil, e
		}
		if _, e := tx.BindRuntimeDataSource(ctx, f.config.ID, d.ID); e != nil {
			return nil, e
		}
		return tx.SetDataSourceStatus(ctx, d.ID, "running")
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "test.discovery"}, func(tx *Tx) (any, error) {
		if _, e := tx.UpdateRoute(ctx, f.watch.ID, f.watch.Version, RouteInput{Mode: "ignore"}, "exclude"); e != nil {
			return nil, e
		}
		return tx.SyncDataSourceGroups(ctx, d.ID, []string{f.watch.ConversationID, "cid:new", "cid:new"})
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err = ReadDataSource(ctx, f.s.DB, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "running" || len(d.RouteIDs) != 1 {
		t.Fatalf("source state: %+v", d)
	}
	if direct, e := ReadRoute(ctx, f.s.DB, f.direct.ID); e != nil || direct.ConversationType != "direct" {
		t.Fatalf("outbound owner route disappeared: %+v %v", direct, e)
	}
	r, err := ReadRoute(ctx, f.s.DB, d.RouteIDs[0])
	if err != nil || r.ConversationID != "cid:new" || r.Mode != "collect" {
		t.Fatalf("route: %+v %v", r, err)
	}
	got, bound, err := DataSourceForRuntime(ctx, f.s.DB, f.config.ID)
	if err != nil || !bound || got.ID != d.ID {
		t.Fatalf("binding %+v %v %v", got, bound, err)
	}
	_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "test.checkpoint"}, func(tx *Tx) (any, error) { return nil, tx.CheckpointDataSource(ctx, d.ID, 4, "unavailable") })
	if err != nil {
		t.Fatal(err)
	}
	d, err = ReadDataSource(ctx, f.s.DB, d.ID)
	if err != nil || d.ReconcileCursor != 4 || d.LastErrorCode != "unavailable" {
		t.Fatalf("checkpoint %+v %v", d, err)
	}
}

func TestDataSourceIgnoreNameSurvivesDiscoveryAndBlocksIntake(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:initial")
	var source DataSource
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "test.ignore-config"}, func(tx *Tx) (any, error) {
		var e error
		source, e = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "work", Channel: c.ID, Workspace: "global", Ignore: []string{"Noise"}})
		return source, e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "test.ignore-discovery"}, func(tx *Tx) (any, error) {
		return tx.SyncDataSourceGroupDetails(ctx, source.ID, []DataSourceGroup{{ID: "cid:noise", Name: "Noise"}, {ID: "cid:work", Name: "Work"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	noise, err := RouteFor(ctx, s.DB, c.ID, "cid:noise")
	if err != nil || noise.Mode != "ignore" {
		t.Fatalf("noise was admitted: %+v %v", noise, err)
	}
	source, err = ReadDataSource(ctx, s.DB, source.ID)
	if err != nil || len(source.RouteIDs) != 1 || len(source.Ignore) != 1 || source.Ignore[0] != "Noise" {
		t.Fatalf("ignore was not durable: %+v %v", source, err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "test.ignore-intake"}, func(tx *Tx) (any, error) {
		return tx.Intake(ctx, c.ID, NormalizedEvent{Kind: EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "noise-1", ConversationID: "cid:noise", ConversationType: "group", Tenant: c.Tenant, Sender: Sender{IDType: "staff_id", IDValue: "peer"}, Body: "do not archive", Format: "text", SentAt: Now(), EventAt: Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE channel_id=? AND conversation_id=?", c.ID, "cid:noise").Scan(&count); err != nil || count != 0 {
		t.Fatalf("ignored body persisted: %d %v", count, err)
	}
}

func TestDisabledDataSourceCannotResumeReceive(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:work")
	disabled := false
	var source DataSource
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "test.disabled-source"}, func(tx *Tx) (any, error) {
		var e error
		source, e = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "disabled", Channel: c.ID, Workspace: "global", Enabled: &disabled})
		return source, e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "test.disabled-resume"}, func(tx *Tx) (any, error) { return tx.SetDataSourceStatus(ctx, source.ID, "running") })
	if ErrorCode(err) != "denied" {
		t.Fatalf("disabled source started: %v", err)
	}
}

func TestNewDataSourceDoesNotInheritUnverifiedLegacyGroups(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "legacy-group")
	var source DataSource
	runtimeMutate(t, s, "first-source.create", func(tx *Tx) (any, error) {
		if _, err := tx.AddRoute(ctx, c.ID, RouteInput{ConversationID: "ignored-group", ConversationType: "group"}); err != nil {
			return nil, err
		}
		var err error
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "new-source", Channel: c.ID, Workspace: "global", Ignore: []string{"Noise"}})
		return source, err
	})
	if len(source.RouteIDs) != 0 {
		t.Fatalf("new source inherited unproved routes: %d", len(source.RouteIDs))
	}
	runtimeMutate(t, s, "first-source.start", func(tx *Tx) (any, error) { return tx.SetDataSourceStatus(ctx, source.ID, "running") })
	runtimeMutate(t, s, "first-source.history", func(tx *Tx) (any, error) { return nil, tx.EnsureHistoryImports(ctx, source.ID, time.Now()) })
	var jobs int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM history_imports WHERE data_source_id=?", source.ID).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("unverified initial scope scheduled history: %d %v", jobs, err)
	}
	runtimeMutate(t, s, "first-source.positive-discovery", func(tx *Tx) (any, error) {
		var err error
		source, err = tx.ApplySourceGroupObservation(ctx, source.ID, []DataSourceGroup{{ID: "verified-group", Name: "Work"}, {ID: "ignored-group", Name: "Noise"}}, false, nil)
		return source, err
	})
	if len(source.RouteIDs) != 1 {
		t.Fatalf("partial positives widened acquisition: %d", len(source.RouteIDs))
	}
	route, err := ReadRoute(ctx, s.DB, source.RouteIDs[0])
	if err != nil || route.ConversationID != "verified-group" {
		t.Fatal("unverified or ignored group admitted")
	}
	// Reconfiguration preserves the source's committed scope, not channel scope.
	runtimeMutate(t, s, "first-source.reconfigure", func(tx *Tx) (any, error) {
		if _, err := tx.SetDataSourceStatus(ctx, source.ID, "stopped"); err != nil {
			return nil, err
		}
		var err error
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: source.Name, Channel: c.ID, Workspace: "global", Ignore: []string{"Noise"}})
		return source, err
	})
	if len(source.RouteIDs) != 1 || source.RouteIDs[0] != route.ID {
		t.Fatal("reconfigure replaced existing verified source scope")
	}
}
