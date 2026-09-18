package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

const dualConfigYAML = `data_sources:
  work_chat:
    channel: dws-main
    groups:
      member_robot: app-main
      ignore: [ignored-group]
agents:
  owner-assistant:
    preset: owner-preset
    claude_profile: cc
    memory_scope: owner_authorized
    capabilities: [memory_read, local_read, local_write, local_test]
    directories: [/does-not-have-to-exist]
  group-helper:
    preset: group-preset
    claude_profile: cc
    memory_scope: conversation_published
    capabilities: [conversation_history_read, memory_read, artifact_create]
applications:
  proactive:
    enabled: true
    source: work_chat
    owner: {id_type: staff_id, id_value: OWNER_ID}
    agent: owner-assistant
  group_mention:
    enabled: true
    source: work_chat
    channel: app-main
    default_agent: group-helper
    bindings:
      - conversation_id: ignored-group
        agent: group-helper
`

func TestGroupSharingConfigAndPermissionChanges(t *testing.T) {
	for _, tt := range []struct {
		shared, excluded string
		valid            bool
	}{
		{"[global]", "[preference]", true}, {"[]", "[]", true},
		{"[private-workspace]", "[preference]", false}, {"[global, global]", "[]", false},
		{"[global]", "[fact]", false}, {"[]", "[preference, preference]", false},
	} {
		a := &app{}
		body := strings.Replace(dualConfigYAML, "    channel: app-main", "    shared_memory_workspaces: "+tt.shared+"\n    excluded_memory_categories: "+tt.excluded+"\n    channel: app-main", 1)
		err := a.loadConfig([]byte(body))
		if err == nil {
			_, err = NormalizeDualModeConfig(a.cfg)
		}
		if (err == nil) != tt.valid {
			t.Fatalf("shared=%s excluded=%s: %v", tt.shared, tt.excluded, err)
		}
	}
	var change PlanChange
	before := &GroupMentionApplication{}
	after := &GroupMentionApplication{SharedMemoryWorkspaces: []string{"global"}, ExcludedMemoryCategories: []string{"preference"}}
	classifyPermissionChange(&change, before, after)
	if !change.PermissionExpansion || !change.PermissionReduction || !change.BoundaryChange {
		t.Fatalf("sharing change not classified: %+v", change)
	}
	change = PlanChange{}
	classifyPermissionChange(&change, after, &GroupMentionApplication{SharedMemoryWorkspaces: []string{"global"}})
	if !change.PermissionExpansion || change.PermissionReduction || !change.BoundaryChange {
		t.Fatalf("removing exclusion must require expansion authorization: %+v", change)
	}
}

func TestDualConfigNormalizationOffline(t *testing.T) {
	a := &app{}
	if err := a.loadConfig([]byte(dualConfigYAML)); err != nil {
		t.Fatal(err)
	}
	v, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := v.Declaration
	s := d.DataSources["work_chat"]
	p := d.Applications.Proactive
	if !*s.Enabled || s.Archive != "sqlite" || *s.Groups.ActiveDays != 30 || *s.ReconcileSeconds != 300 || *s.HistoryImport.Enabled || *s.HistoryImport.Days != 30 {
		t.Fatalf("source defaults: %+v", s)
	}
	if *p.Batch.Items != 20 || *p.Batch.MaxWaitSeconds != 30 || p.AnalysisModel != "haiku" || p.Focus != "owner_relevant_work" || p.Delivery != "record_only" {
		t.Fatalf("application defaults: %+v", p)
	}
	if d.Agents["owner-assistant"].ExecutionModel != "profile" {
		t.Fatal("profile default missing")
	}
	if len(v.Diagnostics) != 1 || v.Diagnostics[0].Code != "binding_ignored" {
		t.Fatal(v.Diagnostics)
	}
	if len(v.UnresolvedReferences) != 5 {
		t.Fatal(v.UnresolvedReferences)
	}
	if a.cfg.DataSources["work_chat"].Enabled != nil || a.cfg.Applications.Proactive.Batch.Items != nil || a.cfg.Agents["owner-assistant"].ExecutionModel != "" {
		t.Fatal("normalization mutated parsed declaration")
	}
	home := filepath.Join(t.TempDir(), "not-initialized")
	code, result := invoke(t, home, "", "--config", configFile(t, dualConfigYAML), "config", "validate")
	if code != 0 {
		t.Fatal(result)
	}
	got := data(t, result)
	if got["valid"] != true || got["application_status"] != "not_applied" || got["declaration"] == nil || got["unresolved_references"] == nil {
		t.Fatal(got)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("static validation touched home: %v", err)
	}
}

func TestNormalizeDirectSourceDefaultsAndRejectsLegacyHistoryImport(t *testing.T) {
	a := &app{}
	if err := a.loadConfig([]byte("data_sources:\n  work_chat:\n    channel: dws-main\n    groups: {member_robot: app-main}\n    direct: {enabled: true}\n    retention: {days: 7}\n    backfill_after_enable: true\n")); err != nil {
		t.Fatal(err)
	}
	validated, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	source := validated.Declaration.DataSources["work_chat"]
	if !*source.Direct.Enabled || *source.Retention.Days != 7 || !*source.BackfillAfterEnable || *source.HistoryImport.Enabled {
		t.Fatalf("direct defaults: %+v", source)
	}
	b := &app{}
	if err = b.loadConfig([]byte("data_sources:\n  work_chat:\n    channel: dws-main\n    groups: {member_robot: app-main}\n    direct: {enabled: true}\n    history_import: {enabled: true, days: 7}\n")); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("direct source accepted legacy history import: %v", err)
	}
}

func TestDualConfigOmittedAndExplicitValues(t *testing.T) {
	a := &app{}
	if err := a.loadConfig([]byte(`agents:
  defaults: {}
  none: {capabilities: [], directories: []}
applications:
  proactive: {enabled: false}
`)); err != nil {
		t.Fatal(err)
	}
	if a.cfg.Applications.GroupMention != nil || a.cfg.Applications.Proactive.Enabled == nil || *a.cfg.Applications.Proactive.Enabled {
		t.Fatal("omitted/false application lost")
	}
	v, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if *v.Declaration.Applications.Proactive.Enabled || *v.Declaration.Applications.GroupMention.Enabled {
		t.Fatal("application enabled implicitly")
	}
	if v.Declaration.Agents["none"].Capabilities == nil || len(v.Declaration.Agents["none"].Capabilities) != 0 || len(v.Declaration.Agents["defaults"].Capabilities) != 2 {
		t.Fatal("empty capabilities inherited defaults")
	}
	if a.cfg.Agents["defaults"].Capabilities != nil || a.cfg.Agents["none"].Capabilities == nil || a.cfg.Agents["defaults"].Directories != nil || a.cfg.Agents["none"].Directories == nil {
		t.Fatal("omitted/empty collection lost")
	}
	if err := a.loadConfig([]byte(strings.Replace(dualConfigYAML, "groups:\n", "enabled: false\n    groups:\n", 1))); err == nil {
		t.Fatal("enabled consumer accepted disabled source")
	}
}

func TestDualConfigRejectsUnsafeAndInvalidDeclarations(t *testing.T) {
	cases := map[string]string{
		"unknown root":          dualConfigYAML + "secret_token: DO_NOT_ECHO_SECRET\n",
		"unknown nested":        strings.Replace(dualConfigYAML, "preset: owner-preset", "secret_token: DO_NOT_ECHO_SECRET", 1),
		"duplicate map key":     strings.Replace(dualConfigYAML, "channel: dws-main", "channel: dws-main\n    channel: DO_NOT_ECHO_SECRET", 1),
		"invalid type":          strings.Replace(dualConfigYAML, "enabled: true", "enabled: DO_NOT_ECHO_SECRET", 1),
		"missing source":        strings.ReplaceAll(dualConfigYAML, "source: work_chat", "source: DO_NOT_ECHO_SECRET"),
		"missing agent":         strings.Replace(dualConfigYAML, "agent: owner-assistant", "agent: DO_NOT_ECHO_SECRET", 1),
		"duplicate binding":     dualConfigYAML + "      - conversation_id: ignored-group\n        agent: group-helper\n",
		"relative directory":    strings.Replace(dualConfigYAML, "/does-not-have-to-exist", "DO_NOT_ECHO_SECRET", 1),
		"invalid capability":    strings.Replace(dualConfigYAML, "local_test", "DO_NOT_ECHO_SECRET", 1),
		"duplicate capability":  strings.Replace(dualConfigYAML, "local_test", "local_read", 1),
		"group private memory":  strings.Replace(dualConfigYAML, "memory_scope: conversation_published", "memory_scope: owner_authorized", 1),
		"invalid profile":       strings.Replace(dualConfigYAML, "claude_profile: cc", "claude_profile: 'DO_NOT_ECHO_SECRET; echo unsafe'", 1),
		"profile without alias": "agents:\n  bad: {execution_model: profile}\n",
		"invalid model":         "agents:\n  bad: {execution_model: 'DO_NOT_ECHO_SECRET; unsafe'}\n",
		"invalid owner":         strings.Replace(dualConfigYAML, "id_type: staff_id", "id_type: union_id", 1),
		"invalid reference":     strings.Replace(dualConfigYAML, "channel: dws-main", "channel: 'DO_NOT_ECHO_SECRET; unsafe'", 1),
		"zero days":             strings.Replace(dualConfigYAML, "member_robot: app-main", "member_robot: app-main\n      active_days: 0", 1),
		"too many days":         strings.Replace(dualConfigYAML, "member_robot: app-main", "member_robot: app-main\n      active_days: 31", 1),
		"zero items":            strings.Replace(dualConfigYAML, "agent: owner-assistant", "agent: owner-assistant\n    batch: {items: 0}", 1),
		"bad policy":            strings.Replace(dualConfigYAML, "default_agent: group-helper", "default_agent: group-helper\n    reply_policy: broadcast", 1),
		"disabled dangling ref": "applications:\n  proactive: {enabled: false, source: DO_NOT_ECHO_SECRET}\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			a := &app{}
			err := a.loadConfig([]byte(raw))
			if err == nil {
				t.Fatal("accepted invalid declaration")
			}
			if strings.Contains(err.Error(), "DO_NOT_ECHO_SECRET") {
				t.Fatalf("sensitive error: %v", err)
			}
		})
	}
}

func TestDualConfigExplicitAnalysisAndDisabledApplications(t *testing.T) {
	raw := strings.Replace(dualConfigYAML, "agent: owner-assistant", "agent: owner-assistant\n    analysis_model: claude-haiku-4-5", 1)
	a := &app{}
	if err := a.loadConfig([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	v, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v.Declaration.Applications.Proactive.AnalysisModel != "claude-haiku-4-5" {
		t.Fatal("explicit model lost")
	}
	raw = strings.ReplaceAll(dualConfigYAML, "enabled: true", "enabled: false")
	if err := a.loadConfig([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	v, err = NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !*v.Declaration.DataSources["work_chat"].Enabled || *v.Declaration.Applications.Proactive.Enabled || *v.Declaration.Applications.GroupMention.Enabled {
		t.Fatal("disabled consumers changed collection")
	}
	b, err := json.Marshal(v)
	if err != nil || !json.Valid(b) {
		t.Fatal(err)
	}
}

func TestDualConfigBashAndOwnerPrivatePolicies(t *testing.T) {
	for _, raw := range []string{
		"agents:\n  owner-chat: {bash: yes}\n",
		"agents:\n  owner-chat: {bash: 'true'}\n",
		"agents:\n  owner-chat: {bash: 1}\n",
	} {
		a := &app{}
		if err := a.loadConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted non-boolean Bash declaration: %q", raw)
		}
	}
	valid := `agents:
  owner-chat: {memory_scope: owner_authorized, bash: true, external_actions: owner_request}
  group-default: {memory_scope: conversation_published, bash: false}
  group-special: {memory_scope: conversation_published, bash: true, capabilities: [local_test]}
applications:
  owner_private: {enabled: true, runtime: owner-private, agent: owner-chat}
  group_mention: {enabled: false, default_agent: group-default, bindings: [{conversation_id: group1, agent: group-special}]}
`
	a := &app{}
	if err := a.loadConfig([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !normalized.Declaration.Agents["owner-chat"].Bash || normalized.Declaration.Agents["group-default"].Bash || !normalized.Declaration.Agents["group-special"].Bash {
		t.Fatal("Bash policy lost during normalization")
	}
	if normalized.Declaration.Applications.OwnerPrivate.Agent != "owner-chat" {
		t.Fatal("owner-private Agent binding lost")
	}
	for _, raw := range []string{
		strings.Replace(valid, "agent: owner-chat", "agent: DO_NOT_ECHO_SECRET", 1),
		strings.Replace(valid, "agent: owner-chat", "agent: group-default", 1),
		strings.Replace(valid, "default_agent: group-default", "default_agent: owner-chat", 1),
		valid + "  proactive: {enabled: false, agent: owner-chat}\n",
	} {
		probe := &app{}
		if err := probe.loadConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted unsafe owner/group binding")
		}
	}
	builtIn := &app{}
	if err := builtIn.loadConfig([]byte("applications:\n  owner_private: {enabled: true, runtime: owner-private}\n")); err != nil {
		t.Fatal("built-in owner Agent should not need a declaration: ", err)
	}
	if builtIn.cfg.Applications.OwnerPrivate.Agent != "" {
		t.Fatal("empty Agent must denote built-in owner policy")
	}
}
