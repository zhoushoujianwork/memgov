package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDualConfigPlanReadOnlyAndDeterministic(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	cfg := configFile(t, "data_sources:\n  work_chat:\n    channel: dws-main\n    groups:\n      member_robot: app-main\n")
	code, result := invoke(t, home, "", "--config", cfg, "config", "plan")
	if code == 0 {
		t.Fatalf("plan opened absent database: %v", result)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("plan created home: %v", err)
	}
	if code, result = invoke(t, home, "", "init"); code != 0 {
		t.Fatal(result)
	}
	code, first := invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(first)
	}
	code, second := invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(second)
	}
	a, b := data(t, first), data(t, second)
	if a["plan_digest"] == "" || a["plan_digest"] != b["plan_digest"] {
		t.Fatalf("plan changed without state change: %v %v", a["plan_digest"], b["plan_digest"])
	}
	if a["ready"] != false {
		t.Fatalf("unresolved channel cannot be applied: %v", a)
	}
	if len(a["blockers"].([]any)) == 0 {
		t.Fatalf("missing references were not reported: %v", a)
	}
}
