package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

const mountSourceYAML = `data_sources:
  work_chat:
    channel: dws-main
    groups: {member_robot: app-main}
`
const mountGroupYAML = `agents:
  helper:
    preset: claude-default
    capabilities: [conversation_history_read]
applications:
  group_mention:
    enabled: true
    source: work_chat
    channel: app-main
    default_agent: helper
`

func groupMountFixture(t *testing.T) (string, *core.Store, core.DataSource, core.Channel, core.Channel) {
	t.Helper()
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), "home")
	invoke(t, home, "", "init")
	if code, v := invoke(t, home, "", "agent", "preset", "enable", "claude", "--name", "claude-default"); code != 0 {
		t.Fatal(v)
	}
	s, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var dws, app core.Channel
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "fixture.mount.channels"}, func(tx *core.Tx) (any, error) {
		var e error
		dws, e = tx.AddChannel(ctx, core.ChannelInput{Name: "dws-main", Kind: core.ChannelDwsPersonal, Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "owner1", Profile: "corp1:owner1", DeliveryRobotName: "Bot", DeliveryRobotCode: "bot1"}})
		if e != nil {
			return nil, e
		}
		if _, e = tx.SetChannelCapabilities(ctx, dws.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true, "send": true}}, "fake"); e != nil {
			return nil, e
		}
		dws, e = core.ReadChannel(ctx, tx.Conn, dws.ID)
		if e != nil {
			return nil, e
		}
		if _, e = tx.AttestDWSOwner(ctx, dws.ID, dws.ConfigVersion); e != nil {
			return nil, e
		}
		app, e = tx.AddChannel(ctx, core.ChannelInput{Name: "app-main", Kind: core.ChannelDingTalkApp, Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ClientID: "client1", RobotCode: "bot1", HistoryChannel: dws.ID}})
		if e != nil {
			return nil, e
		}
		if _, e = tx.SetChannelCapabilities(ctx, app.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "send": true}}, "fake"); e != nil {
			return nil, e
		}
		if _, e = tx.AddRoute(ctx, dws.ID, core.RouteInput{ConversationID: "group1", Mode: "collect"}); e != nil {
			return nil, e
		}
		if _, e = tx.AddRoute(ctx, dws.ID, core.RouteInput{ConversationID: "owner1", ConversationType: "direct", Mode: "notify"}); e != nil {
			return nil, e
		}
		if _, e = tx.AddRoute(ctx, app.ID, core.RouteInput{ConversationID: "owner1", ConversationType: "direct", Mode: "notify", SendPolicy: "dispatch_only"}); e != nil {
			return nil, e
		}
		return tx.Intake(ctx, dws.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "owner", ConversationID: "owner1", ConversationType: "direct", Tenant: "corp1", Sender: core.Sender{IDType: "user_id", IDValue: "owner1"}, Body: "fixture", SentAt: core.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := configFile(t, mountSourceYAML)
	p := channelPlan(t, home, cfg)
	if p["ready"] != true {
		t.Fatal(p)
	}
	if code, v := channelApply(t, home, cfg, p); code != 0 {
		t.Fatal(v)
	}
	source, err := core.ReadDataSource(ctx, s.DB, "work_chat")
	if err != nil {
		t.Fatal(err)
	}
	dws, _ = core.ReadChannel(ctx, s.DB, dws.ID)
	app, _ = core.ReadChannel(ctx, s.DB, app.ID)
	return home, s, source, dws, app
}
func recordMountDiscovery(t *testing.T, s *core.Store, source core.DataSource, dws core.Channel, groups []core.DataSourceGroup) {
	t.Helper()
	ctx := context.Background()
	_, err := s.Mutate(ctx, core.Request{Scope: "global", Command: "fixture.complete-discovery"}, func(tx *core.Tx) (any, error) {
		if _, e := tx.SyncDataSourceGroupDetails(ctx, source.ID, groups); e != nil {
			return nil, e
		}
		return nil, tx.RecordSourceGroupDiscovery(ctx, source.ID, source.Version, dws.ConfigVersion, groups)
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestDualGroupMountColdStartRequiresProviderDiscovery(t *testing.T) {
	home, s, source, dws, app := groupMountFixture(t)
	cfg := configFile(t, mountSourceYAML+mountGroupYAML)
	p := channelPlan(t, home, cfg)
	if !channelHasBlock(p, "group_routes_missing") {
		t.Fatal("locally prefilled source route was trusted")
	}
	recordMountDiscovery(t, s, source, dws, []core.DataSourceGroup{{ID: "group1", Name: "Work"}})
	p = channelPlan(t, home, cfg)
	if p["ready"] != true || len(p["group_mounts"].([]any)) != 1 {
		t.Fatal(p)
	}
	if _, err := core.RouteFor(context.Background(), s.DB, app.ID, "group1"); err == nil {
		t.Fatal("plan wrote a route")
	}
	if code, v := channelApply(t, home, cfg, p); code != 0 {
		t.Fatal(v)
	}
	r, err := core.RouteFor(context.Background(), s.DB, app.ID, "group1")
	if err != nil || r.Mode != "assistant" || r.SendPolicy != "reply_to_trigger" || r.AudiencePolicy != "conversation" {
		t.Fatalf("mount: %+v %v", r, err)
	}
	runtime, err := core.ReadRuntime(context.Background(), s.DB, "group-mention")
	if err != nil || len(runtime.RouteIDs) != 1 || runtime.RouteIDs[0] != r.ID {
		t.Fatalf("runtime: %+v %v", runtime, err)
	}
	again := channelPlan(t, home, cfg)
	if again["ready"] != true || len(again["group_mounts"].([]any)) != 0 {
		t.Fatal(again)
	}
}
func TestDualGroupMountReceiptDriftAndIgnore(t *testing.T) {
	home, s, source, dws, app := groupMountFixture(t)
	recordMountDiscovery(t, s, source, dws, []core.DataSourceGroup{{ID: "group1", Name: "Work"}, {ID: "noise", Name: "Noise"}})
	cfg := configFile(t, "data_sources:\n  work_chat:\n    channel: dws-main\n    groups: {member_robot: app-main, ignore: [Noise]}\n"+strings.Replace(mountGroupYAML, "default_agent: helper", "default_agent: helper\n    bindings: [{conversation_id: noise, agent: helper}]", 1))
	p := channelPlan(t, home, cfg)
	if p["ready"] != true || len(p["group_mounts"].([]any)) != 1 {
		t.Fatal(p)
	}
	if _, err := s.DB.Exec("UPDATE source_group_discoveries SET observed_at=? WHERE source_id=?", time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339Nano), source.ID); err != nil {
		t.Fatal(err)
	}
	if code, v := channelApply(t, home, cfg, p); code == 0 {
		t.Fatalf("stale discovery plan was applied: %v", v)
	}
	if _, err := core.RouteFor(context.Background(), s.DB, app.ID, "group1"); err == nil {
		t.Fatal("stale plan partially created route")
	}
	recordMountDiscovery(t, s, source, dws, []core.DataSourceGroup{{ID: "group1", Name: "Work"}, {ID: "noise", Name: "Noise"}})
	p = channelPlan(t, home, cfg)
	if code, v := channelApply(t, home, cfg, p); code != 0 {
		t.Fatal(v)
	}
	if _, err := core.RouteFor(context.Background(), s.DB, app.ID, "noise"); err == nil {
		t.Fatal("ignored group was mounted")
	}
}
func TestDualGroupMountWrongRobotOrUnverifiedBotStaysBlocked(t *testing.T) {
	for _, scenario := range []string{"wrong-robot", "unverified-app", "app-ignore", "failed-discovery"} {
		t.Run(scenario, func(t *testing.T) {
			home, s, source, dws, app := groupMountFixture(t)
			recordMountDiscovery(t, s, source, dws, []core.DataSourceGroup{{ID: "group1", Name: "Work"}})
			switch scenario {
			case "wrong-robot":
				if _, err := s.DB.Exec("UPDATE source_group_discoveries SET robot_code='other' WHERE source_id=?", source.ID); err != nil {
					t.Fatal(err)
				}
			case "unverified-app":
				if _, err := s.DB.Exec("UPDATE channels SET capabilities='{}' WHERE id=?", app.ID); err != nil {
					t.Fatal(err)
				}
			case "failed-discovery":
				if _, err := s.DB.Exec("UPDATE source_group_discoveries SET valid=0 WHERE source_id=?", source.ID); err != nil {
					t.Fatal(err)
				}
			case "app-ignore":
				_, err := s.Mutate(context.Background(), core.Request{Scope: "global", Command: "fixture.ignore"}, func(tx *core.Tx) (any, error) {
					return tx.AddRoute(context.Background(), app.ID, core.RouteInput{ConversationID: "group1", Mode: "ignore"})
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg := configFile(t, mountSourceYAML+mountGroupYAML)
			p := channelPlan(t, home, cfg)
			if p["ready"] != false {
				t.Fatal("untrusted group was ready")
			}
			if code, v := channelApply(t, home, cfg, p); code == 0 {
				t.Fatal(v)
			}
		})
	}
}
