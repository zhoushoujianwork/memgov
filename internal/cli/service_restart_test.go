package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	localservice "github.com/zhoushoujianwork/memgov/internal/service"
)

func TestWebRestartLoadsInstalledBinaryAndPreservesDirectAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and restarts the real local service binary")
	}
	home := t.TempDir()
	store, err := core.Open(context.Background(), filepath.Join(home, "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(home, "memgov")
	build := func(version, target string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", "-X github.com/zhoushoujianwork/memgov/internal/cli.Version="+version, "-o", target, "./cmd/memgov")
		cmd.Dir = "../.."
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build failed: %v: %s", err, output)
		}
	}
	build("restart-before", binary)
	outputPath := filepath.Join(home, "service-output")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	cmd := exec.Command(binary, "service", "start", "--home", home, "--port", "0")
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = localservice.Stop(ctx, home)
		select {
		case <-done:
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
		}
	})
	wait := func(ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if ready() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("timed out waiting for local service")
	}
	var startup *url.URL
	pattern := regexp.MustCompile(`http://127\.0\.0\.1:\d+/`)
	wait(func() bool {
		data, _ := os.ReadFile(outputPath)
		match := pattern.FindString(string(data))
		if match == "" {
			return false
		}
		startup, err = url.Parse(match)
		return err == nil
	})
	base := "http://" + startup.Host
	client := &http.Client{Timeout: 2 * time.Second}
	type meta struct {
		Version, Build string
		Service        localservice.Snapshot
	}
	readMeta := func() (meta, bool) {
		var envelope struct {
			OK   bool
			Data meta
		}
		resp, err := client.Get(base + "/api/v1/meta")
		if err != nil {
			return meta{}, false
		}
		defer resp.Body.Close()
		err = json.NewDecoder(resp.Body).Decode(&envelope)
		return envelope.Data, err == nil && resp.StatusCode == 200 && envelope.OK
	}
	var before meta
	wait(func() bool {
		var ok bool
		before, ok = readMeta()
		return ok && before.Service.State == "running"
	})
	if before.Version != "restart-before" {
		t.Fatal("initial version differed")
	}
	// Replace the installed file while the old process still runs.
	replacement := binary + ".new"
	build("restart-after", replacement)
	if err := os.Rename(replacement, binary); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest("POST", base+"/api/v1/service/restart", bytes.NewBufferString("{}"))
	request.Header.Set("Origin", base)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Memgov-Console", "1")
	resp, err := client.Do(request)
	if err != nil {
		t.Fatal("restart acknowledgement was lost")
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("restart status: %d", resp.StatusCode)
	}
	var after meta
	wait(func() bool {
		var ok bool
		after, ok = readMeta()
		return ok && after.Service.ID != before.Service.ID && after.Service.State == "running"
	})
	if after.Version != "restart-after" || after.Build == before.Build || after.Service.PID != before.Service.PID {
		t.Fatal("restart did not load the replacement in the original foreground process")
	}
	if after.Service.PID != cmd.Process.Pid {
		t.Fatal("restart detached the service")
	}
}
