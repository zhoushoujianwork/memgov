package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceUpgradePreservesNamesAndManagedDrift(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 1, 30)
	dropSchema27(t, f.s)
	if _, err := f.s.DB.Exec("UPDATE runtime_configs SET memory_scope='owner_authorized',agent_capabilities='[\"memory_read\",\"local_read\"]',review_timeout_seconds=33; UPDATE channel_routes SET memory_policy='curated'"); err != nil {
		t.Fatal(err)
	}
	runtime, err := ReadRuntime(ctx, f.s.DB, f.config.ID)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := ReadChannel(ctx, f.s.DB, f.channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	route, err := ReadRoute(ctx, f.s.DB, f.watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	input := ChannelInput{Name: channel.Name, Route: &RouteInput{ConversationID: route.ConversationID}}
	oldRuntime := RuntimeManagedSnapshot(runtime)
	oldRuntime["memory_scope"], oldRuntime["review_timeout_seconds"] = runtime.MemoryScope, runtime.ReviewTimeoutSeconds
	oldRoute := RouteManagedSnapshot(route)
	oldRoute["memory_policy"] = route.MemoryPolicy
	objects := []ManagedConfigObject{
		{Kind: "channel", Name: channel.Name, ObjectType: "channel", ObjectID: channel.ID, Snapshot: Digest(channelManagedSnapshot(channel, input, true))},
		{Kind: "route", Name: "group", ObjectType: "route", ObjectID: route.ID, Snapshot: Digest(oldRoute)},
		{Kind: "application", Name: "proactive", ObjectType: "runtime", ObjectID: runtime.ID, Snapshot: Digest(oldRuntime)},
		{Kind: "agent", Name: "memory_scope", ObjectType: "preset", ObjectID: "preset", Snapshot: "preset-commit"},
	}
	declaration := map[string]any{
		"agents":       map[string]any{"memory_scope": map[string]any{"memory_scope": "owner_authorized", "home": "missing-old-home", "capabilities": []string{"memory_read", "local_read"}}, "review_timeout_seconds": map[string]any{"preset": "keep"}},
		"applications": map[string]any{"proactive": map[string]any{"review_timeout_seconds": 33, "agent": "memory_scope"}, "bots": map[string]any{"memory_scope": map[string]any{"group_mention": map[string]any{"shared_memory_workspaces": []string{"global"}, "default_agent": "review_timeout_seconds"}}}},
		"channels":     []any{map[string]any{"name": channel.Name, "route": map[string]any{"conversation_id": route.ConversationID, "memory_policy": "curated"}}},
	}
	for version := 1; version <= 2; version++ {
		if version == 2 {
			for n := 0; n < 3; n++ {
				objects[n].Snapshot = "manual-drift"
			}
		}
		if _, err = f.s.DB.Exec("INSERT INTO applied_configs VALUES(?,?,1,?,?,?,?)", NewID(), version, JSON(declaration), JSON(objects), "old-digest", Now()); err != nil {
			t.Fatal(err)
		}
	}
	path := f.s.Path
	f.s.Close()
	s, err := Open(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	current, err := ReadRuntime(ctx, s.DB, runtime.ID)
	if err != nil || current.MemoryScope != "" || JSON(current.AgentCapabilities) != `["local_read"]` {
		t.Fatal(current, err)
	}
	for version := 1; version <= 2; version++ {
		applied, err := ReadAppliedConfig(ctx, s.DB, version)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{ChannelManagedSnapshot(channel, input), Digest(RouteManagedSnapshot(route)), Digest(RuntimeManagedSnapshot(current)), "preset-commit"}
		for n, object := range applied.Objects {
			if version == 2 && n < 3 {
				want[n] = "manual-drift"
			}
			if object.Snapshot != want[n] {
				t.Fatalf("version %d object %s snapshot %s want %s", version, object.Name, object.Snapshot, want[n])
			}
		}
		var clean map[string]any
		if err = json.Unmarshal(applied.Declaration, &clean); err != nil {
			t.Fatal(err)
		}
		agents := clean["agents"].(map[string]any)
		if len(agents) != 2 || agents["review_timeout_seconds"] == nil || agents["memory_scope"] == nil {
			t.Fatal("named agents removed", clean)
		}
		agent := agents["memory_scope"].(map[string]any)
		if agent["memory_scope"] != nil || agent["home"] != nil || JSON(agent["capabilities"]) != `["local_read"]` {
			t.Fatal("legacy fields retained", agent)
		}
		if applied.Digest != Digest(map[string]any{"schema_version": applied.SchemaVersion, "declaration": clean, "objects": applied.Objects}) {
			t.Fatal("configuration digest not rebased")
		}
	}
}

func TestUpgradeRefusesAgentHomeThroughAncestorAlias(t *testing.T) {
	s := testStore(t)
	dropSchema27(t, s)
	home := filepath.Dir(s.Path)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(home), alias); err != nil {
		t.Fatal(err)
	}
	aliasedHome := filepath.Join(alias, filepath.Base(home))
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("agents:\n  owner:\n    home: "+aliasedHome+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	s.Close()
	if _, err := Open(context.Background(), path, true); err == nil || !strings.Contains(err.Error(), "contains the state or archive directory") {
		t.Fatal("ancestor alias permitted recursive archive", err)
	}
	old, err := OpenReadCompatible(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var version int
	if err = old.DB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != 26 {
		t.Fatal(version, err)
	}
}
