package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLaunchAgentDefinitionAndLifecycle(t *testing.T) {
	home := t.TempDir()
	m := &LaunchAgent{Label: "test.memgov", Path: filepath.Join(home, "LaunchAgents", "test.plist"), LogPath: filepath.Join(home, "runtime", "launchd.log"), domain: "gui/501"}
	o := AgentOptions{Executable: "/opt/a & b/memgov", Home: home, Config: "/config/dual config.yaml", WorkingDirectory: "/opt/a & b", UserHome: "/Users/test", SearchPath: "/opt/a & b:/usr/bin", Port: 8787}
	loaded := false
	var calls []string
	m.run = func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "print":
			if !loaded {
				return errors.New("not loaded")
			}
		case "bootstrap":
			loaded = true
		case "bootout":
			loaded = false
		}
		return nil
	}
	ctx := context.Background()
	if err := m.Install(ctx, o); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/opt/a &amp; b/memgov", "<string>run</string>", "<key>KeepAlive</key><true/>", "<key>RunAtLoad</key><true/>", "<integer>5</integer>"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %s", want)
		}
	}
	info, _ := os.Stat(m.Path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("launch agent is not private")
	}
	if !loaded {
		t.Fatal("install did not bootstrap")
	}
	calls = nil
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"print gui/501/test.memgov", "disable gui/501/test.memgov", "bootout gui/501/test.memgov"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("stop must disable before unloading: %v", calls)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !loaded {
		t.Fatal("start did not bootstrap stopped service")
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.Path); !os.IsNotExist(err) || loaded {
		t.Fatal("uninstall left a job installed")
	}
	if _, err := os.Stat(m.LogPath); err != nil {
		t.Fatal("uninstall removed logs")
	}
}

func TestLaunchAgentInvalidOptionsDoNotStopService(t *testing.T) {
	m := &LaunchAgent{run: func(context.Context, ...string) error { t.Fatal("invalid install touched launchctl"); return nil }}
	if err := m.Install(context.Background(), AgentOptions{}); err == nil {
		t.Fatal("accepted relative paths")
	}
	_, err := m.definition(AgentOptions{Executable: "/bin/memgov", Home: "/home", Config: "/config", WorkingDirectory: "/cwd", Port: 0})
	if err == nil {
		t.Fatal("accepted unstable managed port")
	}
}

func TestLaunchAgentUninstallPreservesDefinitionOnStopFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.plist")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	m := &LaunchAgent{Path: path, run: func(_ context.Context, args ...string) error {
		if args[0] == "bootout" {
			return errors.New("bootout failed")
		}
		return nil
	}}
	if err := m.Uninstall(context.Background()); err == nil {
		t.Fatal("ignored stop failure")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed definition after stop failed")
	}
}

func TestLaunchAgentStartRetriesAsynchronousBootout(t *testing.T) {
	home := t.TempDir()
	m := &LaunchAgent{
		Label:  "test.memgov",
		Path:   filepath.Join(home, "test.plist"),
		domain: "gui/501",
		sleep:  func(time.Duration) {},
	}
	if err := os.WriteFile(m.Path, []byte("plist"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded := false
	bootstrapAttempts := 0
	m.run = func(_ context.Context, args ...string) error {
		switch args[0] {
		case "print":
			if !loaded {
				return errors.New("not loaded")
			}
		case "bootstrap":
			bootstrapAttempts++
			if bootstrapAttempts == 1 {
				return errors.New("service still unloading")
			}
			loaded = true
		}
		return nil
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if bootstrapAttempts != 2 || !loaded {
		t.Fatalf("start did not retry bootstrap: attempts=%d loaded=%v", bootstrapAttempts, loaded)
	}
}
