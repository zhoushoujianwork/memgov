package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

// Opt-in binary acceptance: the script supplies an explicit binary. Neither
// the normal test suite nor this fixture discovers the user's live MEMGOV_HOME.
func acceptanceBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("MEMGOV_ACCEPTANCE_BINARY")
	if binary == "" {
		t.Skip("run scripts/runtime-offline-acceptance.sh to verify a built or installed binary")
	}
	absolute, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(absolute); err != nil {
		t.Fatal(err)
	}
	return absolute
}
func acceptanceEnv(home, binDir string) []string {
	// Deliberately exclude DWS/Claude credentials and the user's config variables.
	return []string{"HOME=" + home, "MEMGOV_HOME=" + home, "PATH=" + binDir + ":/usr/bin:/bin", "LANG=C", "TZ=UTC"}
}
func runAcceptanceBinary(t *testing.T, binary, home string, args ...string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{"--home", home}, args...)...)
	cmd.Env = acceptanceEnv(home, filepath.Join(home, "fake-bin"))
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated binary command %s failed: %v %s", args[0], err, raw)
	}
	var env struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		t.Fatalf("invalid envelope: %s", raw)
	}
	return env.Data
}

func TestInstalledRuntimeAcceptanceSchema6MigrationRehearsal(t *testing.T) {
	binary := acceptanceBinary(t)
	ctx := context.Background()
	originalHome := t.TempDir()
	original := filepath.Join(originalHome, "state.db")
	db, err := sql.Open("sqlite", original)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range databaseMigrations {
		if m.Version > 6 {
			break
		}
		if _, err = db.ExecContext(ctx, m.SQL); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(?,?,?)", m.Version, Hash([]byte(m.SQL)), Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('role','authoritative'),('acceptance_sentinel','preserve-on-upgrade')"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	before := Hash(raw)
	// This is a rehearsal on a disposable copy, not a nonexistent init --dry-run.
	cloneHome := t.TempDir()
	clone := filepath.Join(cloneHome, "state.db")
	if err = os.WriteFile(clone, raw, 0600); err != nil {
		t.Fatal(err)
	}
	runAcceptanceBinary(t, binary, cloneHome, "init")
	upgraded, err := Open(ctx, clone, false)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	var sentinel string
	if err = upgraded.DB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema6 migration target=%d want=%d error=%v", version, SchemaVersion, err)
	}
	if err = upgraded.DB.QueryRow("SELECT value FROM settings WHERE key='acceptance_sentinel'").Scan(&sentinel); err != nil || sentinel != "preserve-on-upgrade" {
		t.Fatal("migration lost persisted data")
	}
	after, err := os.ReadFile(original)
	if err != nil || Hash(after) != before {
		t.Fatal("migration rehearsal altered original fixture")
	}
	runAcceptanceBinary(t, binary, cloneHome, "doctor")
}

const acceptanceFakeDWS = `#!/bin/sh
case "$1 $2" in
  "chat +chat-list-all") printf '%s\n' '{"success":true,"result":{"complete":true,"groups":[{"openConversationId":"cid:watch","name":"Test"}]}}' ;;
  "chat +search-msg") printf '%s\n' '{"success":true,"result":{"complete":true,"messages":[{"conversationId":"cid:watch"}]}}' ;;
  "chat +chat-bots") printf '%s\n' '{"success":true,"result":{"bots":[{"name":"Fake Bot","robotCode":"fake-bot"}]}}' ;;
  "chat +chat-messages") printf '%s\n' '{"success":true,"result":{"messages":[],"complete":false,"hasMore":true,"nextPage":"fake-page-2","stopReason":"page_limit"}}' ;;
  "event +listen-im"|"event consume")
    printf '%s\n' '[event] ready' >&2
    sleep 1
    printf '%s\n' 'RAW_STDERR_MUST_NOT_SURVIVE token=PRIVATE_FAKE_CREDENTIAL https://secret.invalid/capability' >&2
    exit 17 ;;
  *) printf '%s\n' 'Unexpected fake provider operation' >&2; exit 19 ;;
esac
`

func TestInstalledRuntimeAcceptanceFakeDWSDisconnect(t *testing.T) {
	binary := acceptanceBinary(t)
	for _, complete := range []bool{true, false} {
		name, script := "complete-discovery", acceptanceFakeDWS
		if !complete {
			name = "partial-positive-discovery"
			script = strings.Replace(script, `"complete":true,"messages"`, `"complete":false,"hasMore":true,"messages"`, 1)
		}
		t.Run(name, func(t *testing.T) {
			runInstalledFakeDWSDisconnect(t, binary, script, complete)
		})
	}
}

func acceptanceReceiverRetries(t *testing.T, home, sourceID string) int {
	t.Helper()
	events, err := runlog.Show(home, sourceID, runlog.Filter{Component: "data_source"})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Event == "receiver_retry" {
			count++
		}
	}
	return count
}

func runInstalledFakeDWSDisconnect(t *testing.T, binary, script string, complete bool) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 20, 300)
	home := filepath.Dir(f.s.Path)
	binDir := filepath.Join(home, "fake-bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "dws"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	var source DataSource
	runtimeMutate(t, f.s, "acceptance.source", func(tx *Tx) (any, error) {
		if _, err := tx.SetRuntimeStatus(ctx, f.config.ID, "stopped", ""); err != nil {
			return nil, err
		}
		identity := f.channel.Identity
		identity.DeliveryRobotName = "Fake Bot"
		identity.DeliveryRobotCode = "fake-bot"
		if _, err := tx.Conn.ExecContext(ctx, "UPDATE channels SET identity=? WHERE id=?", JSON(identity), f.channel.ID); err != nil {
			return nil, err
		}
		if _, err := tx.SetChannelCapabilities(ctx, f.channel.ID, Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); err != nil {
			return nil, err
		}
		var err error
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "offline-source", Channel: f.channel.ID, Workspace: "global", ReconcileSeconds: 10, MemberRobotCode: "fake-bot"})
		return source, err
	})
	for run := 0; run < 2; run++ {
		retriesBefore := acceptanceReceiverRetries(t, home, source.ID)
		procCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(procCtx, binary, "--home", home, "data-source", "start", source.ID)
		cmd.Env = acceptanceEnv(home, binDir)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		failed := false
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			stored, err := ReadDataSource(ctx, f.s.DB, source.ID)
			if err != nil {
				cancel()
				<-done
				t.Fatal(err)
			}
			// Discovery or history errors alone must not satisfy a receiver-disconnect test.
			if stored.LastErrorCode != "" && acceptanceReceiverRetries(t, home, source.ID) > retriesBefore {
				failed = true
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
		runAcceptanceBinary(t, binary, home, "data-source", "stop", source.ID)
		select {
		case err := <-done:
			if err != nil {
				cancel()
				t.Fatalf("fake-source process failed: %v %s", err, output.String())
			}
		case <-time.After(3 * time.Second):
			cancel()
			<-done
			t.Fatal("source ignored stop")
		}
		cancel()
		if !failed {
			t.Fatalf("fake receiver disconnect was not observed: %s", output.String())
		}
		receipt, err := ReadSourceGroupDiscovery(ctx, f.s.DB, source.ID)
		if err != nil || !receipt.Valid || receipt.Complete != complete || len(receipt.Groups) != 1 || receipt.Groups[0].ID != "cid:watch" {
			t.Fatalf("expected verified discovery before disconnect: %+v %v", receipt, err)
		}
		for _, secret := range []string{"RAW_STDERR_MUST_NOT_SURVIVE", "PRIVATE_FAKE_CREDENTIAL", "secret.invalid"} {
			if strings.Contains(output.String(), secret) {
				t.Fatal("raw provider stderr leaked into binary output")
			}
		}
		err = filepath.Walk(filepath.Join(home, "runtime", "logs"), func(path string, info os.FileInfo, walkErr error) error {
			if os.IsNotExist(walkErr) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			for _, secret := range []string{"RAW_STDERR_MUST_NOT_SURVIVE", "PRIVATE_FAKE_CREDENTIAL", "secret.invalid"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Fatal("raw provider stderr leaked into structured log files")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var gaps int
		if err := f.s.DB.QueryRow("SELECT count(*) FROM coverage_windows WHERE channel_id=? AND complete=0", f.channel.ID).Scan(&gaps); err != nil || gaps == 0 {
			t.Fatalf("partial history not persisted as a gap: %d %v; process diagnostics: %s", gaps, err, output.String())
		}
		lease, err := ReadLease(ctx, f.s.DB, f.channel.ID)
		if err != nil || lease.Held {
			t.Fatalf("receiver lease survived stop: %+v %v", lease, err)
		}
		var attempts int
		if err = f.s.DB.QueryRow("SELECT count(*) FROM runtime_attempts").Scan(&attempts); err != nil || attempts != 0 {
			t.Fatal("collection failure executed an AI task")
		}
		// Reset only the diagnostic so the next process must record a new failure.
		if _, err = f.s.DB.Exec("UPDATE data_sources SET last_error_code='' WHERE id=?", source.ID); err != nil {
			t.Fatal(err)
		}
	}
}
