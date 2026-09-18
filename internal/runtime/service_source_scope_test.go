package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func setupSourceScopeService(t *testing.T) (*Service, core.RuntimeConfig, core.Channel, core.DataSource) {
	t.Helper()
	s, cfg, _, _, _ := setupService(t)
	ctx := context.Background()
	c, err := core.ReadChannel(ctx, s.Store.DB, cfg.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	c.Identity.DeliveryRobotName = "Test Robot"
	identity, _ := json.Marshal(c.Identity)
	var source core.DataSource
	err = s.mutate(ctx, "global", "test.source.scope", func(tx *core.Tx) (any, error) {
		if _, e := tx.Conn.ExecContext(ctx, "UPDATE channels SET identity=? WHERE id=?", string(identity), c.ID); e != nil {
			return nil, e
		}
		var e error
		source, e = tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: "scope-source", Channel: c.ID, Workspace: "global", MemberRobotCode: "robot"})
		if e != nil {
			return nil, e
		}
		// Model a legacy collector union. Only a later receipt can authorize AI.
		source, e = tx.SyncDataSourceGroups(ctx, source.ID, []string{"watch", "legacy-one", "legacy-two"})
		if e != nil {
			return nil, e
		}
		if _, e = tx.SetDataSourceStatus(ctx, source.ID, "running"); e != nil {
			return nil, e
		}
		if _, e = tx.SetRuntimeStatus(ctx, cfg.ID, "stopped", ""); e != nil {
			return nil, e
		}
		if _, e = tx.BindRuntimeDataSource(ctx, cfg.ID, source.ID); e != nil {
			return nil, e
		}
		_, e = tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET route_ids=? WHERE id=?", mustSourceRoutes(source.RouteIDs), cfg.ID)
		return nil, e
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = core.ReadRuntime(ctx, s.Store.DB, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	source, err = core.ReadDataSource(ctx, s.Store.DB, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg, c, source
}

func sourceScopeMutation(t *testing.T, s *Service, fn func(*core.Tx) (any, error)) {
	t.Helper()
	if err := s.mutate(context.Background(), "global", "test.scope.change", fn); err != nil {
		t.Fatal(err)
	}
}

func assertProcessingGroups(t *testing.T, s *Service, cfg core.RuntimeConfig, want ...string) {
	t.Helper()
	stored, err := core.ReadRuntime(context.Background(), s.Store.DB, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.RouteIDs, cfg.RouteIDs) {
		t.Fatal("returned and persisted scopes differ")
	}
	got := map[string]bool{}
	for _, id := range cfg.RouteIDs {
		r, err := core.ReadRoute(context.Background(), s.Store.DB, id)
		if err != nil {
			t.Fatal(err)
		}
		got[r.ConversationID] = true
	}
	if len(got) != len(want) {
		t.Fatalf("processing scope %v, want %v", got, want)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("processing scope %v lacks %s", got, id)
		}
	}
}

func TestSourceRuntimeSyncUsesPositiveProofInsteadOfCollectorUnion(t *testing.T) {
	s, cfg, c, source := setupSourceScopeService(t)
	ctx := context.Background()
	sourceScopeMutation(t, s, func(tx *core.Tx) (any, error) {
		return nil, tx.RecordSourceGroupObservation(ctx, source.ID, source.Version, c.ConfigVersion, []core.DataSourceGroup{{ID: "watch"}}, false)
	})
	next := s.syncGroups(ctx, cfg, c)
	assertProcessingGroups(t, s, next, "watch")
	if next.Version != cfg.Version+1 {
		t.Fatal("scope shrink did not invalidate runtime version")
	}
	var writesBefore int
	if err := s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM requests WHERE command='runtime.groups.source'").Scan(&writesBefore); err != nil {
		t.Fatal(err)
	}
	again := s.syncGroups(ctx, next, c)
	if again.Version != next.Version {
		t.Fatal("unchanged proof caused version churn")
	}
	var writesAfter int
	if err := s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM requests WHERE command='runtime.groups.source'").Scan(&writesAfter); err != nil {
		t.Fatal(err)
	}
	if writesAfter != writesBefore {
		t.Fatalf("unchanged proof entered a write transaction: before=%d after=%d", writesBefore, writesAfter)
	}
	collector, err := core.ReadDataSource(ctx, s.Store.DB, source.ID)
	if err != nil || len(collector.RouteIDs) != 3 {
		t.Fatal("AI shrink changed collector continuity scope")
	}
	sourceScopeMutation(t, s, func(tx *core.Tx) (any, error) {
		groups := []core.DataSourceGroup{{ID: "watch"}, {ID: "new-positive"}}
		if _, err := tx.ApplySourceGroupObservation(ctx, source.ID, groups, false, nil); err != nil {
			return nil, err
		}
		return nil, tx.RecordSourceGroupObservation(ctx, source.ID, source.Version, c.ConfigVersion, groups, false)
	})
	next = s.syncGroups(ctx, again, c)
	assertProcessingGroups(t, s, next, "watch", "new-positive")
}

func TestSourceRuntimeSyncClearsScopeWithoutProof(t *testing.T) {
	s, cfg, c, _ := setupSourceScopeService(t)
	next := s.syncGroups(context.Background(), cfg, c)
	assertProcessingGroups(t, s, next)
	if next.Version != cfg.Version+1 {
		t.Fatal("unproved legacy scope was retained")
	}
}

func TestProactiveSourceSyncIncludesDirectButGroupScopeDoesNot(t *testing.T) {
	s, cfg, c, source := setupSourceScopeService(t)
	ctx := context.Background()
	sourceScopeMutation(t, s, func(tx *core.Tx) (any, error) {
		if _, err := tx.SetDataSourceStatus(ctx, source.ID, "stopped"); err != nil {
			return nil, err
		}
		enabled := true
		disabled := false
		var err error
		source, err = tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: source.Name, Channel: c.ID, Workspace: "global", MemberRobotCode: "robot", DirectEnabled: &enabled, HistoryEnabled: &disabled})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetDataSourceStatus(ctx, source.ID, "running"); err != nil {
			return nil, err
		}
		if err = tx.RecordSourceGroupObservation(ctx, source.ID, source.Version, c.ConfigVersion, []core.DataSourceGroup{{ID: "watch"}}, false); err != nil {
			return nil, err
		}
		_, err = tx.EnsureDataSourceDirectConversation(ctx, source.ID, core.NormalizedEvent{Kind: core.EventMessage, ConversationID: "peer-direct", ConversationType: "direct", Tenant: c.Tenant, Sender: core.Sender{IDType: "open_id", IDValue: "peer"}, SentAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)})
		return nil, err
	})
	cfg.ApplicationMode = "proactive"
	next := s.syncGroups(ctx, cfg, c)
	assertProcessingGroups(t, s, next, "watch", "peer-direct")
	next.ApplicationMode = "group_mention"
	group := s.syncGroups(ctx, next, c)
	assertProcessingGroups(t, s, group, "watch")
}

func TestSourceRuntimeSyncLimitsTransientFallbackAndRevocation(t *testing.T) {
	for _, scenario := range []string{"transient", "expired", "revoked", "source-version", "route-ignore"} {
		t.Run(scenario, func(t *testing.T) {
			s, cfg, c, source := setupSourceScopeService(t)
			ctx := context.Background()
			sourceScopeMutation(t, s, func(tx *core.Tx) (any, error) {
				return nil, tx.RecordSourceGroupObservation(ctx, source.ID, source.Version, c.ConfigVersion, []core.DataSourceGroup{{ID: "watch"}}, false)
			})
			sourceScopeMutation(t, s, func(tx *core.Tx) (any, error) {
				if err := tx.InvalidateSourceGroupDiscovery(ctx, source.ID); err != nil {
					return nil, err
				}
				switch scenario {
				case "expired":
					_, err := tx.Conn.ExecContext(ctx, "UPDATE source_group_discoveries SET observed_at=? WHERE source_id=?", time.Now().Add(-25*time.Hour).UTC().Format(time.RFC3339Nano), source.ID)
					return nil, err
				case "revoked":
					return nil, tx.RevokeSourceGroupDiscovery(ctx, source.ID)
				case "source-version":
					_, err := tx.Conn.ExecContext(ctx, "UPDATE data_sources SET version=version+1 WHERE id=?", source.ID)
					return nil, err
				case "route-ignore":
					_, err := tx.Conn.ExecContext(ctx, "UPDATE channel_routes SET mode='ignore' WHERE channel_id=? AND conversation_id='watch'", c.ID)
					return nil, err
				}
				return nil, nil
			})
			next := s.syncGroups(ctx, cfg, c)
			if scenario == "transient" {
				assertProcessingGroups(t, s, next, "watch")
			} else {
				assertProcessingGroups(t, s, next)
			}
		})
	}
}

func TestSourceRuntimeTickRechecksProofBeforeAnalyzingLegacyMessage(t *testing.T) {
	s, cfg, c, source := setupSourceScopeService(t)
	ctx := context.Background()
	sourceScopeMutation(t, s, func(tx *core.Tx) (any, error) {
		if _, err := tx.SetRuntimeStatus(ctx, cfg.ID, "running", ""); err != nil {
			return nil, err
		}
		return tx.Intake(ctx, c.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderEventID: "legacy-event", ProviderMessageID: "legacy-message", ConversationID: "legacy-one", Tenant: c.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "alice"}, Body: "please analyze", SentAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	})
	// Pass the old three-route snapshot deliberately. tick must use current proof
	// before core message sync rereads persisted runtime scope.
	s.tick(ctx, cfg, agent.Preset{})
	current, err := core.ReadRuntime(ctx, s.Store.DB, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertProcessingGroups(t, s, current)
	if s.Analyzer.(*fakeModels).analyses != 0 {
		t.Fatal("unproved legacy message reached model")
	}
	sourceScopeMutation(t, s, func(tx *core.Tx) (any, error) {
		return nil, tx.RecordSourceGroupObservation(ctx, source.ID, source.Version, c.ConfigVersion, []core.DataSourceGroup{{ID: "watch"}}, false)
	})
	s.tick(ctx, cfg, agent.Preset{})
	current, err = core.ReadRuntime(ctx, s.Store.DB, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertProcessingGroups(t, s, current, "watch")
	if s.Analyzer.(*fakeModels).analyses != 0 {
		t.Fatal("collector union bypassed processing proof")
	}
}
