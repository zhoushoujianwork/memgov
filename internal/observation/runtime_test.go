package observation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunnerIdentityFreshnessAndIndependentCleanup(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	stop, err := Watch(context.Background(), home, "runtime-one", "v1", "build-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Watch(context.Background(), home, "runtime-one", "v2", "build-two")
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	if entries := Read(home, "runtime-one", now.Add(time.Second)); len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	if entries := Read(t.TempDir(), "runtime-one", now.Add(time.Second)); len(entries) != 0 {
		t.Fatal("cross-home heartbeat")
	}
	if entries := Read(home, "other-runtime", now.Add(time.Second)); len(entries) != 0 {
		t.Fatal("cross-runtime heartbeat")
	}
	if entries := Read(home, "runtime-one", now.Add(time.Minute)); len(entries) != 0 {
		t.Fatal("stale heartbeat trusted")
	}
	stop()
	if entries := Read(home, "runtime-one", time.Now().Add(time.Second)); len(entries) != 1 || entries[0].Version != "v2" {
		t.Fatal("cleanup removed other runner")
	}
	forged := Runner{RuntimeID: "runtime-one", Home: home, InstanceID: "fake", HeartbeatAt: now}
	body, _ := json.Marshal(forged)
	os.WriteFile(filepath.Join(home, "runtime", "observations", "wrong-name.json"), body, 0600)
	if entries := Read(home, "runtime-one", time.Now().Add(time.Second)); len(entries) != 1 {
		t.Fatal("mismatched record trusted")
	}
}
