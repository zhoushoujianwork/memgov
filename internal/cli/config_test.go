package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/channel/dws"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

const botYAML = `channels:
  - name: app-main
    kind: dingtalk_app
    identity:
      expected_corp_id: corp1
      client_id: client1
      robot_code: robot1
    credential_ref: env://MEMGOV_CONFIG_TEST_SECRET
    transport: stream
    route:
      conversation_id: cid:group1
      conversation_type: group
      workspace_id: global
      mode: assistant
      triggers: [mention]
      send_policy: draft_only
`

func configFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigDefaultsPrecedenceAndOfflineReads(t *testing.T) {
	home := filepath.Join(t.TempDir(), "uninitialized")
	path := configFile(t, "default_workspace: global\nformat: text\ntimeout: 7s\nactor: yaml-agent\n"+botYAML)
	t.Setenv("MEMGOV_CONFIG", path)
	t.Setenv("MEMGOV_CONFIG_TEST_SECRET", "do-not-return-this-secret")
	code, value := invoke(t, home, "", "--format", "json", "config", "show")
	if code != 0 {
		t.Fatal(value)
	}
	out := data(t, value)
	if out["timeout"] != "7s" || out["actor"] != "yaml-agent" || out["format"] != "json" || len(out["channels"].([]any)) != 1 {
		t.Fatal(out)
	}
	encoded, _ := json.Marshal(value)
	if strings.Contains(string(encoded), "do-not-return-this-secret") {
		t.Fatal("config show resolved a secret")
	}
	_, value = invoke(t, home, "", "--format", "json", "--timeout", "9s", "--actor", "flag-agent", "config", "show")
	if out = data(t, value); out["timeout"] != "9s" || out["actor"] != "flag-agent" {
		t.Fatal(out)
	}
	other := configFile(t, "timeout: 11s\n")
	_, value = invoke(t, home, "", "--config", other, "config", "show")
	if data(t, value)["timeout"] != "11s" {
		t.Fatal(value)
	}
	code, value = invoke(t, home, "", "--format", "json", "config", "validate")
	if code != 0 || data(t, value)["valid"] != true {
		t.Fatal(value)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("read-only config created home: %v", err)
	}
}

func TestLegacyDualConfigRequiresExplicitMigration(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, "config.dual.yaml")
	raw := []byte("timeout: 11s\nagents:\n  group-helper:\n    preset: claude-default\napplications:\n  bots:\n    app-main:\n      default_agent: group-helper\n      group_mention:\n        enabled: false\n")
	if err := os.WriteFile(legacy, raw, 0600); err != nil {
		t.Fatal(err)
	}

	code, value := invoke(t, home, "", "config", "show")
	if code != 0 {
		t.Fatalf("show: %d %+v", code, value)
	}
	shown := data(t, value)
	if shown["config_status"] != "canonical_missing_legacy_present" || shown["legacy_config_path"] != legacy {
		t.Fatalf("legacy status was not explicit: %+v", shown)
	}
	if shown["config_path"] != canonicalConfigPath(home) || shown["timeout"] != "2m0s" {
		t.Fatalf("legacy file was implicitly loaded: %+v", shown)
	}
	if code, value = invoke(t, home, "", "config", "validate"); code != 3 {
		t.Fatalf("validate did not require migration: %d %+v", code, value)
	}

	code, value = invoke(t, home, "", "config", "migrate-legacy")
	if code != 0 || data(t, value)["migrated"] != true {
		t.Fatalf("migration failed: %d %+v", code, value)
	}
	migrated, err := os.ReadFile(canonicalConfigPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if string(migrated) != string(raw) {
		t.Fatalf("migration changed declaration bytes: %q", migrated)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("migration removed the recovery source: %v", err)
	}
	code, value = invoke(t, home, "", "config", "show")
	if code != 0 || data(t, value)["config_status"] != "canonical" || data(t, value)["timeout"] != "11s" {
		t.Fatalf("canonical config was not activated: %d %+v", code, value)
	}
	if code, value = invoke(t, home, "", "config", "migrate-legacy"); code != 3 {
		t.Fatalf("migration overwrote canonical config: %d %+v", code, value)
	}
}

func TestLegacyDualConfigMigrationRejectsInvalidSource(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.dual.yaml"), []byte("unknown: value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, value := invoke(t, home, "", "config", "migrate-legacy")
	if code != 2 {
		t.Fatalf("invalid legacy config was migrated: %d %+v", code, value)
	}
	if _, err := os.Stat(canonicalConfigPath(home)); !os.IsNotExist(err) {
		t.Fatalf("invalid migration created canonical config: %v", err)
	}
}

func TestConfigRejectsIgnoredFieldsAndBadValues(t *testing.T) {
	for _, raw := range []string{
		"store: legacy\n", "timeout: 0s\n", "format: xml\n", "logging:\n  retention: 0h\n",
		"logging:\n  max_bytes: -1\n", "runtime_setup:\n  concurrency: 33\n", "runtime_setup:\n  reconcile_seconds: 5\n",
		"timeout: 1s\ntimeout: 2s\n", "timeout: 1s\n---\nactor: other\n",
		strings.Replace(botYAML, "client_id: client1", "client_secret: DO_NOT_ECHO_SECRET", 1),
		strings.Replace(botYAML, "env://MEMGOV_CONFIG_TEST_SECRET", "DO_NOT_ECHO_SECRET", 1),
		botYAML + strings.TrimPrefix(botYAML, "channels:\n"),
	} {
		t.Run(raw, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			code, value := invoke(t, home, "", "--config", configFile(t, raw), "config", "validate")
			if code != 2 {
				t.Fatalf("bad configuration accepted: %+v", value)
			}
			encoded, _ := json.Marshal(value)
			if strings.Contains(string(encoded), "DO_NOT_ECHO_SECRET") {
				t.Fatal("error echoed a credential")
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatal("invalid configuration created home")
			}
		})
	}
}

func TestConfigApplyReplayAndVersionedUpdate(t *testing.T) {
	home := t.TempDir()
	path := configFile(t, botYAML)
	invoke(t, home, "", "init")
	args := []string{"--config", path, "config", "apply", "app-main", "--idempotency-key", "create-bot"}
	code, value := invoke(t, home, "", args...)
	if code != 0 {
		t.Fatal(value)
	}
	first := data(t, value)
	if first["identity"].(map[string]any)["client_id"] != "client1" || first["config_version"].(float64) != 1 {
		t.Fatal(first)
	}
	code, value = invoke(t, home, "", args...)
	if code != 0 || value["cached"] != true || data(t, value)["id"] != first["id"] {
		t.Fatalf("replay: %+v", value)
	}
	updated := strings.Replace(botYAML, "robot_code: robot1", "robot_code: robot2", 1)
	if err := os.WriteFile(path, []byte(updated), 0600); err != nil {
		t.Fatal(err)
	}
	code, value = invoke(t, home, "", args...)
	if code != 3 {
		t.Fatalf("changed file reused idempotency result: %+v", value)
	}
	updateArgs := []string{"--config", path, "config", "apply", "app-main", "--expected-version", "1", "--reason", "rotate bot"}
	code, value = invoke(t, home, "", updateArgs...)
	if code != 3 { // Existing routes require their own version.
		t.Fatalf("missing route version accepted: %+v", value)
	}
	updateArgs = append(updateArgs, "--expected-route-version", "1")
	code, value = invoke(t, home, "", updateArgs...)
	if code != 0 || data(t, value)["config_version"].(float64) != 2 || data(t, value)["identity"].(map[string]any)["robot_code"] != "robot2" {
		t.Fatalf("update: %+v", value)
	}
	code, value = invoke(t, home, "", updateArgs...)
	if code != 3 {
		t.Fatalf("stale version accepted: %+v", value)
	}
	code, value = invoke(t, home, "", "--config", path, "config", "apply", "app-main", "--input", "-")
	if code != 2 {
		t.Fatalf("ambiguous input accepted: %+v", value)
	}
}

func TestConfigApplyBadRouteRollsBackChannel(t *testing.T) {
	home := t.TempDir()
	invoke(t, home, "", "init")
	path := configFile(t, strings.Replace(botYAML, "workspace_id: global", "workspace_id: missing", 1))
	code, value := invoke(t, home, "", "--config", path, "config", "apply", "app-main")
	if code != 4 {
		t.Fatal(value)
	}
	_, value = invoke(t, home, "", "channel", "list")
	if len(value["data"].([]any)) != 0 {
		t.Fatalf("partially created channel: %+v", value)
	}
}

type configSetupAdapter struct {
	*setupStubAdapter
	request dws.RuntimeSetupRequest
}

func (s *configSetupAdapter) DiscoverRuntimeSetup(_ context.Context, in dws.RuntimeSetupRequest) (dws.RuntimeSetupResult, error) {
	s.request = in
	return s.discovered, nil
}

func TestRuntimeSetupUsesYAMLAndFlags(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	path := configFile(t, `runtime_setup:
  profile: yaml-profile
  robot_code: yaml-robot
  ignore: [yaml-ignore]
  claude_profile: cc
  analysis_model: haiku
  execution_model: profile
  item_threshold: 7
  max_wait_seconds: 40
  reconcile_seconds: 20
  pilot: true
logging:
  retention: 48h
  max_bytes: 2000000
  file_bytes: 100000
`)
	adapter := &configSetupAdapter{setupStubAdapter: &setupStubAdapter{
		stubAdapter: &stubAdapter{caps: core.Capabilities{Verified: map[string]bool{"history": true, "receive": true, "send": true}}},
		discovered: dws.RuntimeSetupResult{Profile: "corp1", CorpID: "corp1", OwnerUserID: "user1", RobotCode: "robot1", DeliveryID: "dm1",
			Conversations: []dws.RuntimeSetupConversation{{ID: "group1"}}},
	}}
	code, value := invokeWith(t, adapter, home, "", "--config", path, "runtime", "setup", "yaml-watcher", "--workspace-path", project,
		"--robot-code", "flag-robot", "--ignore", "flag-ignore", "--pilot=false", "--item-threshold", "3")
	if code != 0 {
		t.Fatal(value)
	}
	cfg := data(t, value)["runtime"].(map[string]any)
	if cfg["item_threshold"].(float64) != 3 || cfg["max_wait_seconds"].(float64) != 40 || cfg["reconcile_seconds"].(float64) != 20 ||
		cfg["claude_profile"] != "cc" || cfg["analysis_model"] != "haiku" || cfg["execution_model"] != "profile" {
		t.Fatalf("runtime defaults did not reach SQLite: %+v", cfg)
	}
	if adapter.request.Profile != "yaml-profile" || adapter.request.RobotCode != "flag-robot" || strings.Join(adapter.request.Ignore, ",") != "flag-ignore" {
		t.Fatalf("discovery defaults: %+v", adapter.request)
	}
	var a app
	raw, _ := os.ReadFile(path)
	if err := a.loadConfig(raw); err != nil {
		t.Fatal(err)
	}
	opts := a.cfg.Logging.options()
	if opts.Retention.Hours() != 48 || opts.MaxBytes != 2000000 || opts.FileBytes != 100000 {
		t.Fatal(opts)
	}
}

func TestConfigTextDefault(t *testing.T) {
	var out bytes.Buffer
	code := Run(context.Background(), []string{"--home", t.TempDir(), "--config", configFile(t, "format: text\n"), "config", "show"}, strings.NewReader(""), &out, &bytes.Buffer{})
	if code != 0 || strings.Contains(out.String(), `"schema_version"`) || !strings.Contains(out.String(), `"format": "text"`) {
		t.Fatal(out.String())
	}
}
