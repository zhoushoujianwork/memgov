package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestWorkspaceConfigMigrationArchivesAndPreservesAuthority(t *testing.T) {
	home := t.TempDir()
	notes := filepath.Join(home, "old-agent")
	if err := os.MkdirAll(notes, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notes, "CLAUDE.md"), []byte("OLD_KNOWLEDGE_SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(dualConfigYAML, "preset: owner-preset", "preset: owner-preset\n    home: "+notes+"\n    memory_scope: owner_authorized\n    external_actions: owner_delegated\n    skills: {inherit: none, paths: [/skills/memgov-memory, /skills/work]}", 1)
	raw = strings.Replace(raw, "local_read, local_write", "memory_read, local_read, local_write", 1)
	raw = strings.Replace(raw, "    channel: app-main", "    shared_memory_workspaces: [global]\n    excluded_memory_categories: [preference]\n    channel: app-main", 1)
	config := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(config, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if code, out := invoke(t, home, "", "config", "validate"); code != 2 {
		t.Fatal(code, out)
	}
	code, out := invoke(t, home, "", "config", "migrate-workspaces")
	if code != 0 {
		t.Fatal(code, out)
	}
	archive := data(t, out)["archive_path"].(string)
	old, err := os.ReadFile(filepath.Join(archive, "config.yaml"))
	if err != nil || string(old) != raw {
		t.Fatal(err)
	}
	archived, err := os.ReadFile(filepath.Join(archive, "agent-homes", core.Hash([]byte(notes)), "CLAUDE.md"))
	if err != nil || string(archived) != "OLD_KNOWLEDGE_SENTINEL" {
		t.Fatal(string(archived), err)
	}
	converted, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: config}
	if err = a.loadConfig(converted); err != nil {
		t.Fatal(err)
	}
	agent := a.cfg.Agents["owner-assistant"]
	if agent.Preset != "owner-preset" || agent.ExternalActions != "owner_delegated" || len(agent.Capabilities) != 3 || len(agent.Skills.Paths) != 1 || agent.Skills.Paths[0] != "/skills/work" || a.cfg.Applications.GroupMention.Channel != "app-main" {
		t.Fatalf("authority changed: %+v", a.cfg)
	}
	if _, err = os.Stat(filepath.Join(home, "agent-workspaces")); !os.IsNotExist(err) {
		t.Fatal("old notes imported", err)
	}
	if code, out = invoke(t, home, "", "config", "migrate-workspaces"); code != 0 || data(t, out)["migrated"] != false {
		t.Fatal(code, out)
	}
}

func TestWorkspaceConfigArchiveFailureDoesNotConvert(t *testing.T) {
	for _, kind := range []string{"blocked-backup", "symlink-notes", "symlink-ancestor", "invalid-config"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			notes := filepath.Join(home, "old-agent")
			if kind == "symlink-ancestor" {
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(filepath.Dir(home), alias); err != nil {
					t.Fatal(err)
				}
				notes = filepath.Join(alias, filepath.Base(home))
			}
			raw := "agents:\n  owner: {preset: owner-preset, memory_scope: owner_authorized, home: " + notes + "}\n"
			if kind == "blocked-backup" {
				if err := os.WriteFile(filepath.Join(home, "backups"), []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink-notes" {
				if err := os.Symlink(t.TempDir(), notes); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "invalid-config" {
				raw += "unrecognized: secret-do-not-echo\n"
			}
			path := filepath.Join(home, "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if code, out := invoke(t, home, "", "config", "migrate-workspaces"); code == 0 {
				t.Fatal(out)
			} else if strings.Contains(core.JSON(out), "secret-do-not-echo") {
				t.Fatal("secret leaked")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != raw {
				t.Fatal("configuration changed before archive/validation", err)
			}
		})
	}
}

func TestWorkspaceConfigMigrationPreservesNamesMatchingRetiredFields(t *testing.T) {
	raw := []byte("agents:\n  memory_scope: {preset: test, memory_scope: owner_authorized}\n  memory_policy: {preset: test}\n  review_timeout_seconds: {preset: test}\n  shared_memory_workspaces: {preset: test}\n")
	body, _, changed, err := convertWorkspaceConfig(raw)
	if err != nil || !changed {
		t.Fatal(err)
	}
	a := &app{}
	if err = a.loadConfig(body); err != nil {
		t.Fatal(err)
	}
	if len(a.cfg.Agents) != 4 {
		t.Fatalf("Agent names mistaken for obsolete fields: %s", body)
	}
	for _, name := range []string{"memory_scope", "memory_policy", "review_timeout_seconds", "shared_memory_workspaces"} {
		if a.cfg.Agents[name].Preset != "test" {
			t.Fatal("Agent lost", name)
		}
	}
}
