package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	localservice "github.com/zhoushoujianwork/memgov/internal/service"
)

// Opt-in because this exercises the real user's launchd domain. The service
// uses a temporary empty database and never connects to DingTalk or an Agent.
func TestLaunchdRecoveryAndBinaryUpdate(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("MEMGOV_TEST_LAUNCHD") != "1" {
		t.Skip("set MEMGOV_TEST_LAUNCHD=1 on macOS to test an isolated LaunchAgent")
	}
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := core.Open(ctx, filepath.Join(home, "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	// A managed startup must recover when the newly installed binary requires
	// a newer schema, rather than looping forever with "run memgov init".
	for _, statement := range []string{"DROP TABLE runtime_message_actions", "DELETE FROM schema_migrations WHERE version=22"} {
		if _, err := s.DB.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(home, "memgov")
	build := func(version, path string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", "-X github.com/zhoushoujianwork/memgov/internal/cli.Version="+version, "-o", path, "./cmd/memgov")
		cmd.Dir = "../.."
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v: %s", err, out)
		}
	}
	build("launchd-before", binary)
	m, err := localservice.UserAgent(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cleanupCancel()
		if err := m.Uninstall(cleanupCtx); err != nil {
			t.Errorf("cleanup LaunchAgent: %v", err)
		}
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	command := func(args ...string) {
		t.Helper()
		args = append(args, "--home", home, "--timeout", "40s")
		if out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	command("service", "install", "--port", strconv.Itoa(port))
	backups, err := filepath.Glob(filepath.Join(home, "backups", "service-upgrades", "*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("missing pre-upgrade backup: %v %v", backups, err)
	}
	old, err := core.OpenReadCompatible(ctx, backups[0])
	if err != nil {
		t.Fatal(err)
	}
	var schema int
	err = old.DB.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&schema)
	old.Close()
	if err != nil || schema != 21 {
		t.Fatalf("backup schema: %d %v", schema, err)
	}
	t.Logf("managed startup migrated Schema 21 with backup %s", backups[0])
	before, err := localservice.Read(home)
	if err != nil || before.State != "running" {
		t.Fatal(before, err)
	}
	if err := m.Inspect(ctx); err != nil || !m.Loaded {
		t.Fatal("job not loaded", err)
	}
	// Crash only the job this test just installed, addressed by its unique label.
	if out, err := exec.CommandContext(ctx, "/bin/launchctl", "kill", "SIGKILL", "gui/"+strconv.Itoa(os.Getuid())+"/"+m.Label).CombinedOutput(); err != nil {
		t.Fatalf("crash: %v: %s", err, out)
	}
	after, err := localservice.WaitRunning(ctx, home, before.ID)
	if err != nil || after.PID == before.PID {
		t.Fatal("crash recovery", after, err)
	}
	t.Logf("crash recovery PID %d -> %d", before.PID, after.PID)
	before = after
	build("launchd-after", binary+".new")
	// A temporarily missing install path must not stop the healthy old process.
	if err := os.Rename(binary, binary+".old"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	missing, err := localservice.Read(home)
	if err != nil || missing.ID != before.ID || missing.State != "running" {
		t.Fatal("missing binary stopped old process", missing, err)
	}
	if err := os.Rename(binary+".new", binary); err != nil {
		t.Fatal(err)
	}
	after, err = localservice.WaitRunning(ctx, home, before.ID)
	if err != nil || after.Version != "launchd-after" || after.Build == before.Build {
		t.Fatal("automatic update", after, err)
	}
	t.Logf("automatic update PID %d -> %d, version %s", before.PID, after.PID, after.Version)
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/meta")
		if err == nil {
			var result struct {
				OK   bool
				Data struct{ Version string }
			}
			err = json.NewDecoder(resp.Body).Decode(&result)
			resp.Body.Close()
			if err == nil && result.OK && result.Data.Version == "launchd-after" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("console did not recover on original port")
		}
		time.Sleep(50 * time.Millisecond)
	}
	command("service", "restart")
	restarted, _ := localservice.Read(home)
	if restarted.ID == after.ID {
		t.Fatal("managed CLI restart did not replace service")
	}
	command("service", "stop")
	time.Sleep(6 * time.Second)
	stopped, _ := localservice.Read(home)
	if err := m.Inspect(ctx); err != nil || m.Loaded || stopped.State != "stopped" {
		t.Fatal("manual stop auto-restarted", stopped, err)
	}
	command("service", "start")
	resumed, _ := localservice.Read(home)
	if resumed.State != "running" || resumed.ID == restarted.ID {
		t.Fatal("start did not resume managed job", resumed)
	}
	command("service", "uninstall")
	if err := m.Inspect(ctx); err != nil || m.Installed || m.Loaded {
		t.Fatal("uninstall left job", err)
	}
}
