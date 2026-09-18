package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

const dualChannelsYAML = `channels:
  - name: app-main
    kind: dingtalk_app
    identity: {expected_corp_id: corp1, client_id: client1, robot_code: bot1}
    credential_ref: env://DO_NOT_READ_CHANNEL_SECRET
  - name: dws-main
    kind: dws_personal
    identity: {expected_corp_id: corp1, expected_user_id: owner1, profile: "corp1:owner1", delivery_robot_code: bot1, delivery_robot_name: Bot}
data_sources:
  work_chat:
    channel: dws-main
    groups: {member_robot: app-main}
`

func channelPlan(t *testing.T, home, config string) map[string]any {
	t.Helper()
	code, value := invoke(t, home, "", "--config", config, "config", "plan")
	if code != 0 {
		t.Fatal(value)
	}
	return data(t, value)
}
func channelApply(t *testing.T, home, config string, p map[string]any, flags ...string) (int, map[string]any) {
	t.Helper()
	args := []string{"--config", config, "config", "apply-runtime", "--plan-digest", p["plan_digest"].(string), "--expected-version", fmt.Sprint(p["applied_version"])}
	args = append(args, flags...)
	return invoke(t, home, "", args...)
}
func channelHasBlock(p map[string]any, code string) bool {
	for _, v := range p["blockers"].([]any) {
		if v.(map[string]any)["code"] == code {
			return true
		}
	}
	return false
}

func TestDualChannelsBootstrapUpdateAndStalePlan(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	invoke(t, home, "", "init")
	t.Setenv("DO_NOT_READ_CHANNEL_SECRET", "NEVER_EXPOSE_SECRET_VALUE")
	cfg := configFile(t, dualChannelsYAML)
	p := channelPlan(t, home, cfg)
	if p["ready"] != true {
		t.Fatal(p)
	}
	if strings.Contains(fmt.Sprint(p), "NEVER_EXPOSE_SECRET_VALUE") {
		t.Fatal("secret resolved")
	}
	if code, v := channelApply(t, home, cfg, p); code != 0 {
		t.Fatal(v)
	}
	store, err := core.Open(context.Background(), filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source, err := core.ReadDataSource(context.Background(), store.DB, "work_chat")
	if err != nil || source.Status != "stopped" {
		t.Fatalf("source: %+v %v", source, err)
	}
	app, err := core.ReadChannel(context.Background(), store.DB, "app-main")
	if err != nil || app.Capabilities.Verified["receive"] {
		t.Fatalf("unverified channel: %+v %v", app, err)
	}
	again := channelPlan(t, home, cfg)
	if again["ready"] != true {
		t.Fatal(again)
	}
	for _, v := range again["changes"].([]any) {
		if v.(map[string]any)["action"] != "unchanged" {
			t.Fatal(again)
		}
	}
	if code, v := channelApply(t, home, cfg, again); code != 0 || data(t, v)["version"] != float64(1) {
		t.Fatal(v)
	}
	update := configFile(t, strings.Replace(dualChannelsYAML, "DO_NOT_READ_CHANNEL_SECRET", "NEXT_CHANNEL_SECRET", 1))
	p = channelPlan(t, home, update)
	if !channelHasBlock(p, "authorization_boundary_change") {
		t.Fatal(p)
	}
	if code, v := channelApply(t, home, update, p); code == 0 {
		t.Fatalf("unconfirmed credential change: %v", v)
	}
	if code, v := channelApply(t, home, update, p, "--authorize-boundary", "--reason", "Rotate credential reference"); code != 0 {
		t.Fatal(v)
	}
	if code, v := channelApply(t, home, update, p, "--authorize-boundary", "--reason", "Rotate credential reference"); code == 0 {
		t.Fatalf("stale plan accepted: %v", v)
	}
	updated, _ := core.ReadChannel(context.Background(), store.DB, "app-main")
	if updated.ConfigVersion != 2 || updated.CredentialRef != "env://NEXT_CHANNEL_SECRET" {
		t.Fatal(updated)
	}
}

func TestDualChannelsUnmanagedAndIdentityBoundary(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	invoke(t, home, "", "init")
	cfg := configFile(t, dualChannelsYAML)
	if code, v := invoke(t, home, "", "--config", cfg, "config", "apply", "app-main"); code != 0 {
		t.Fatal(v)
	}
	p := channelPlan(t, home, cfg)
	if !channelHasBlock(p, "unmanaged_conflict") {
		t.Fatal(p)
	}
	if code, v := channelApply(t, home, cfg, p, "--authorize-boundary", "--reason", "No implicit adoption"); code == 0 {
		t.Fatal(v)
	}
	otherHome := filepath.Join(t.TempDir(), "home")
	invoke(t, otherHome, "", "init")
	p = channelPlan(t, otherHome, cfg)
	if code, v := channelApply(t, otherHome, cfg, p); code != 0 {
		t.Fatal(v)
	}
	moved := configFile(t, strings.Replace(dualChannelsYAML, "client_id: client1", "client_id: client2", 1))
	p = channelPlan(t, otherHome, moved)
	if !channelHasBlock(p, "channel_identity_repoint") {
		t.Fatal(p)
	}
	if code, v := channelApply(t, otherHome, moved, p, "--authorize-boundary", "--reason", "Identity cannot be moved"); code == 0 {
		t.Fatal(v)
	}
}

func TestDualChannelsRouteAuthorizationAndAtomicFailure(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	invoke(t, home, "", "init")
	route := "\n    route: {conversation_id: group1, mode: assistant, triggers: [mention], audience_policy: conversation, send_policy: reply_to_trigger}"
	raw := strings.Replace(dualChannelsYAML, "credential_ref: env://DO_NOT_READ_CHANNEL_SECRET", "credential_ref: env://DO_NOT_READ_CHANNEL_SECRET"+route, 1)
	cfg := configFile(t, raw)
	p := channelPlan(t, home, cfg)
	if !channelHasBlock(p, "permission_expansion") {
		t.Fatal(p)
	}
	if code, v := channelApply(t, home, cfg, p); code == 0 {
		t.Fatal(v)
	}
	if code, v := channelApply(t, home, cfg, p, "--authorize-expansion", "--reason", "Reply only to addressed messages"); code != 0 {
		t.Fatal(v)
	}
	invalid := configFile(t, strings.Replace(raw, "conversation_id: group1", "conversation_id: group1, workspace_id: absent", 1))
	p = channelPlan(t, home, invalid)
	if !channelHasBlock(p, "channel_route_invalid") {
		t.Fatal(p)
	}
	store, err := core.Open(context.Background(), filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	before, _ := core.ReadChannel(context.Background(), store.DB, "app-main")
	if code, v := channelApply(t, home, invalid, p, "--authorize-boundary", "--reason", "Bad route"); code == 0 {
		t.Fatal(v)
	}
	after, _ := core.ReadChannel(context.Background(), store.DB, "app-main")
	if before.ConfigVersion != after.ConfigVersion {
		t.Fatal("partial channel write")
	}
}

func TestDualChannelsActiveConsumerNeedsFreshProbe(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	invoke(t, home, "", "init")
	raw := dualChannelsYAML + `agents:
  owner: {preset: missing, memory_scope: owner_authorized}
applications:
  proactive:
    enabled: true
    source: work_chat
    owner: {id_type: user_id, id_value: owner1}
    agent: owner
`
	p := channelPlan(t, home, configFile(t, raw))
	if !channelHasBlock(p, "source_capabilities") {
		t.Fatal(p)
	}
}

func TestDualChannelsProbeIsNotPolicyDriftButCredentialEditIs(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	invoke(t, home, "", "init")
	cfg := configFile(t, dualChannelsYAML)
	p := channelPlan(t, home, cfg)
	if code, v := channelApply(t, home, cfg, p); code != 0 {
		t.Fatal(v)
	}
	store, err := core.Open(context.Background(), filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c, err := core.ReadChannel(context.Background(), store.DB, "dws-main")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Mutate(context.Background(), core.Request{Scope: "global", Command: "test.probe"}, func(tx *core.Tx) (any, error) {
		return tx.SetChannelCapabilities(context.Background(), c.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true, "send": true}}, "fake")
	})
	if err != nil {
		t.Fatal(err)
	}
	p = channelPlan(t, home, cfg)
	if channelHasBlock(p, "managed_object_drift") || p["ready"] != true {
		t.Fatal(p)
	}
	if _, err = store.DB.Exec("UPDATE channels SET credential_ref='env://MANUAL_CHANGE' WHERE name='app-main'"); err != nil {
		t.Fatal(err)
	}
	p = channelPlan(t, home, cfg)
	if !channelHasBlock(p, "managed_object_drift") {
		t.Fatal(p)
	}
}

func TestDualChannelsRunningSourceBlocksChannelChanges(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	invoke(t, home, "", "init")
	cfg := configFile(t, dualChannelsYAML)
	p := channelPlan(t, home, cfg)
	if code, v := channelApply(t, home, cfg, p); code != 0 {
		t.Fatal(v)
	}
	store, err := core.Open(context.Background(), filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.DB.Exec("UPDATE data_sources SET status='running' WHERE name='work_chat'"); err != nil {
		t.Fatal(err)
	}
	changed := configFile(t, strings.Replace(dualChannelsYAML, "delivery_robot_name: Bot", "delivery_robot_name: Renamed", 1))
	p = channelPlan(t, home, changed)
	if !channelHasBlock(p, "channel_in_use") {
		t.Fatal(p)
	}
	if code, v := channelApply(t, home, changed, p, "--authorize-boundary", "--reason", "Blocked while running"); code == 0 {
		t.Fatal(v)
	}
}
