package core

import (
	"context"
	"testing"
	"time"
)

func fallbackProofFixture(t *testing.T) (*Store, DataSource, Channel) {
	t.Helper()
	ctx := context.Background()
	s := testStore(t)
	var source DataSource
	var c Channel
	runtimeMutate(t, s, "fallback.fixture", func(tx *Tx) (any, error) {
		var err error
		c, err = tx.AddChannel(ctx, ChannelInput{Name: "fallback", Kind: ChannelDwsPersonal, Identity: ChannelIdentity{ExpectedCorpID: "corp", ExpectedUserID: "owner", DeliveryRobotCode: "robot", DeliveryRobotName: "Bot"}})
		if err != nil {
			return nil, err
		}
		c, err = tx.SetChannelCapabilities(ctx, c.ID, Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake")
		if err != nil {
			return nil, err
		}
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "fallback", Channel: c.ID, Workspace: "global", MemberRobotCode: "robot"})
		if err != nil {
			return nil, err
		}
		source, err = tx.SyncDataSourceGroupDetails(ctx, source.ID, []DataSourceGroup{{ID: "proved", Name: "Work"}, {ID: "unproved", Name: "Other"}})
		if err != nil {
			return nil, err
		}
		if err = tx.RecordSourceGroupObservation(ctx, source.ID, source.Version, c.ConfigVersion, []DataSourceGroup{{ID: "proved", Name: "Work"}}, false); err != nil {
			return nil, err
		}
		source, err = tx.SetDataSourceStatus(ctx, source.ID, "running")
		return source, err
	})
	return s, source, c
}

func TestSourceFallbackNeedsUnchangedRecentPositiveAuthority(t *testing.T) {
	tests := []struct {
		name    string
		change  func(context.Context, *Tx, DataSource, Channel) error
		allowed bool
	}{
		{name: "current positive subset", allowed: true},
		{name: "transient invalid proof", allowed: true, change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			return tx.InvalidateSourceGroupDiscovery(ctx, d.ID)
		}},
		{name: "revoked proof", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			return tx.RevokeSourceGroupDiscovery(ctx, d.ID)
		}},
		{name: "source version", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			_, e := tx.Conn.ExecContext(ctx, "UPDATE data_sources SET version=version+1 WHERE id=?", d.ID)
			return e
		}},
		{name: "credential rotation", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			_, e := tx.Conn.ExecContext(ctx, "UPDATE channels SET credential_ref='new-reference',config_version=config_version+1 WHERE id=?", c.ID)
			return e
		}},
		{name: "robot identity", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			c.Identity.DeliveryRobotCode = "other"
			_, e := tx.Conn.ExecContext(ctx, "UPDATE channels SET identity=? WHERE id=?", JSON(c.Identity), c.ID)
			return e
		}},
		{name: "capability revoked", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			_, e := tx.Conn.ExecContext(ctx, "UPDATE channels SET capabilities='{}' WHERE id=?", c.ID)
			return e
		}},
		{name: "disabled source", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			_, e := tx.Conn.ExecContext(ctx, "UPDATE data_sources SET enabled=0 WHERE id=?", d.ID)
			return e
		}},
		{name: "paused source", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			_, e := tx.SetDataSourceStatus(ctx, d.ID, "paused")
			return e
		}},
		{name: "removed scope", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			_, e := tx.Conn.ExecContext(ctx, "UPDATE data_sources SET route_ids='[]' WHERE id=?", d.ID)
			return e
		}},
		{name: "name ignore", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			_, e := tx.Conn.ExecContext(ctx, "UPDATE data_sources SET ignore_rules=? WHERE id=?", JSON([]string{"Work"}), d.ID)
			return e
		}},
		{name: "route ignore", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			r, e := RouteFor(ctx, tx.Conn, c.ID, "proved")
			if e != nil {
				return e
			}
			_, e = tx.UpdateRoute(ctx, r.ID, r.Version, RouteInput{Mode: "ignore"}, "deny collection")
			return e
		}},
		{name: "route changed after proof", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			r, e := RouteFor(ctx, tx.Conn, c.ID, "proved")
			if e != nil {
				return e
			}
			_, e = tx.UpdateRoute(ctx, r.ID, r.Version, RouteInput{Mode: "collect"}, "new policy needs discovery")
			return e
		}},
		{name: "expired proof", change: func(ctx context.Context, tx *Tx, d DataSource, c Channel) error {
			_, e := tx.Conn.ExecContext(ctx, "UPDATE source_group_discoveries SET observed_at=? WHERE source_id=?", time.Now().Add(-25*time.Hour).UTC().Format(time.RFC3339Nano), d.ID)
			return e
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, d, c := fallbackProofFixture(t)
			ctx := context.Background()
			if test.change != nil {
				runtimeMutate(t, s, "fallback.change", func(tx *Tx) (any, error) { return nil, test.change(ctx, tx, d, c) })
			}
			before, e := ReadSourceGroupDiscovery(ctx, s.DB, d.ID)
			if e != nil {
				t.Fatal(e)
			}
			result, err := ReadSourceFallbackScope(ctx, s.DB, d.ID, time.Now())
			if test.allowed {
				if err != nil || len(result.RouteIDs) != 1 {
					t.Fatalf("safe prior scope unavailable: %+v %v", result.RouteIDs, err)
				}
				route, e := ReadRoute(ctx, s.DB, result.RouteIDs[0])
				if e != nil || route.ConversationID != "proved" {
					t.Fatalf("unproved route admitted: %+v %v", route, e)
				}
			} else if ErrorCode(err) != "denied" {
				t.Fatalf("changed authority accepted: %v", err)
			}
			currentSource, readErr := ReadDataSource(ctx, s.DB, d.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			processing, processingErr := ReadSourcePositiveProcessingRoutes(ctx, s.DB, currentSource, time.Now())
			if processingErr != nil || (test.allowed && len(processing) != 1) || (!test.allowed && len(processing) != 0) {
				t.Fatalf("AI processing scope ignored bounded positive proof: routes=%v error=%v", processing, processingErr)
			}
			after, e := ReadSourceGroupDiscovery(ctx, s.DB, d.ID)
			if e != nil || Digest(before) != Digest(after) {
				t.Fatal("fallback renewed or rewrote prior proof")
			}
		})
	}
}

func TestSourceFallbackRetainsAuthorizedPreconfiguredWorkspace(t *testing.T) {
	s, d, c := fallbackProofFixture(t)
	ctx := context.Background()
	var workspace Workspace
	var route Route
	runtimeMutate(t, s, "fallback.scoped-route", func(tx *Tx) (any, error) {
		var err error
		workspace, err = tx.AddWorkspace(ctx, "project-scope", "")
		if err != nil {
			return nil, err
		}
		route, err = tx.AddRoute(ctx, c.ID, RouteInput{ConversationID: "project-group", ConversationType: "group", Workspace: workspace.ID})
		if err != nil {
			return nil, err
		}
		d, err = tx.SyncDataSourceGroupDetails(ctx, d.ID, []DataSourceGroup{{ID: route.ConversationID, Name: "Project"}})
		if err != nil {
			return nil, err
		}
		if err = tx.RecordSourceGroupObservation(ctx, d.ID, d.Version, c.ConfigVersion, []DataSourceGroup{{ID: route.ConversationID, Name: "Project"}}, false); err != nil {
			return nil, err
		}
		return nil, tx.InvalidateSourceGroupDiscovery(ctx, d.ID)
	})
	fallback, err := ReadSourceFallbackScope(ctx, s.DB, d.ID, time.Now())
	if err != nil || len(fallback.RouteIDs) != 1 || fallback.RouteIDs[0] != route.ID {
		t.Fatalf("authorized preconfigured scope denied: %+v %v", fallback.RouteIDs, err)
	}
	stored, err := ReadRoute(ctx, s.DB, route.ID)
	if err != nil || stored.WorkspaceID != workspace.ID || stored.WorkspaceID == fallback.WorkspaceID {
		t.Fatal("fallback moved existing route into source default workspace")
	}
	// The accepted cross-workspace case must not weaken stale policy rejection.
	runtimeMutate(t, s, "fallback.route-policy-change", func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, route.ID, route.Version, RouteInput{MemoryPolicy: "curated"}, "policy changed after proof")
	})
	if _, err = ReadSourceFallbackScope(ctx, s.DB, d.ID, time.Now()); ErrorCode(err) != "denied" {
		t.Fatalf("changed route policy reused old proof: %v", err)
	}
}
