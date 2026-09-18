// Package scenarios provides offline black-box acceptance tests. Fixtures describe
// a proposed adapter contract, not captured DingTalk API responses.
package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	// Track adapter changes in Go test cache dependencies although it runs as a subprocess.
	_ "github.com/zhoushoujianwork/memgov/internal/scenario"
)

type request struct {
	Person string `json:"person"`
	Start  string `json:"start"`
	End    string `json:"end"`
}

type exchange struct {
	Args     []string        `json:"args"`
	Output   json.RawMessage `json:"output"`
	ExitCode int             `json:"exit_code"`
}

type finding struct {
	Topic    string   `json:"topic"`
	Status   string   `json:"status"`
	Evidence []string `json:"evidence"`
}

type result struct {
	Status     string    `json:"status"`
	SelfID     string    `json:"self_id"`
	PersonID   string    `json:"person_id"`
	Complete   bool      `json:"complete"`
	Findings   []finding `json:"findings"`
	Candidates []string  `json:"candidates"`
}

type scenario struct {
	ID        string     `json:"id"`
	Request   request    `json:"request"`
	Exchanges []exchange `json:"exchanges"`
	Expected  result     `json:"expected"`
}

func loadCases(t *testing.T) []scenario {
	t.Helper()
	b, err := os.ReadFile("testdata/scenario1.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []scenario
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty scenario suite")
	}
	return cases
}

// validateResult compares semantic facts, not prose. A future summarizer must
// return explicit state and message IDs so stale or unsupported claims fail.
func validateResult(want, got result) error {
	if want.Status != got.Status || want.SelfID != got.SelfID || want.PersonID != got.PersonID || want.Complete != got.Complete {
		return fmt.Errorf("status, identity or completeness mismatch")
	}
	if !reflect.DeepEqual(want.Candidates, got.Candidates) {
		return fmt.Errorf("candidate mismatch")
	}
	if len(want.Findings) != len(got.Findings) {
		return fmt.Errorf("finding count mismatch")
	}
	seen := map[string]bool{}
	for _, f := range got.Findings {
		if seen[f.Topic] {
			return fmt.Errorf("duplicate topic")
		}
		seen[f.Topic] = true
		matched := false
		for _, expected := range want.Findings {
			if f.Topic == expected.Topic {
				matched = f.Status == expected.Status && reflect.DeepEqual(f.Evidence, expected.Evidence)
			}
		}
		if !matched {
			return fmt.Errorf("unsupported finding or evidence")
		}
	}
	return nil
}

// runDriver substitutes dws on PATH. Only calls listed in the fixture succeed;
// each actual invocation is recorded, including unexpected calls. No real dws
// binary or user profile is used by the harness.
func runDriver(t *testing.T, s scenario, driver string, args ...string) (result, error) {
	t.Helper()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.json")
	b, err := json.Marshal(s)
	if err != nil {
		return result{}, err
	}
	if err := os.WriteFile(fixture, b, 0600); err != nil {
		return result{}, err
	}
	self, err := os.Executable()
	if err != nil {
		return result{}, err
	}
	// The executable path is provided through an environment variable, never
	// interpolated into shell code. Arguments remain distinct argv elements.
	shim := "#!/bin/sh\nexec \"$MEMGOV_TEST_BINARY\" -test.run '^TestDWSHelper$' -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "dws"), []byte(shim), 0700); err != nil {
		return result{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, driver, args...)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	// Minimal environment avoids passing the developer's credentials/config to
	// a test driver. Drivers must use the dws found on PATH, not an absolute path.
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir,
		"MEMGOV_TEST_BINARY=" + self, "MEMGOV_SCENARIO_FIXTURE=" + fixture,
		"MEMGOV_SCENARIO_DIR=" + dir}
	payload, _ := json.Marshal(s.Request)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return result{}, fmt.Errorf("driver failed: %w", err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "calls.jsonl"))
	if err != nil {
		return result{}, fmt.Errorf("missing dws invocation log: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != len(s.Exchanges) {
		return result{}, fmt.Errorf("expected %d dws calls, got %d", len(s.Exchanges), len(lines))
	}
	for i, line := range lines {
		var call []string
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			return result{}, err
		}
		if !reflect.DeepEqual(call, s.Exchanges[i].Args) {
			return result{}, fmt.Errorf("unexpected dws call at step %d", i+1)
		}
	}
	var got result
	dec := json.NewDecoder(&stdout)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		return result{}, fmt.Errorf("invalid driver result: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return result{}, fmt.Errorf("trailing driver output")
	}
	return got, validateResult(s.Expected, got)
}

// TestDWSHelper is an executable fake CLI, invoked only in a subprocess.
func TestDWSHelper(t *testing.T) {
	fixture := os.Getenv("MEMGOV_SCENARIO_FIXTURE")
	if fixture == "" {
		return
	}
	b, err := os.ReadFile(fixture)
	if err != nil {
		os.Exit(90)
	}
	var s scenario
	if json.Unmarshal(b, &s) != nil {
		os.Exit(90)
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	// The production driver resolves its dws profile before every request. The
	// fixture focuses on scenario commands, so provide one deterministic current
	// profile without recording it as a business-data exchange.
	if reflect.DeepEqual(args, []string{"profile", "list", "--format", "json"}) {
		fmt.Println(`{"success":true,"currentProfile":"fixture-corp","profiles":[{"profile":"fixture-corp","corpId":"fixture-corp","clientId":"fixture-client","lastLoginAt":"2026-09-14T00:00:00Z","expiresAt":"2026-09-14T01:00:00Z","isCurrent":true}]}`)
		os.Exit(0)
	}
	if len(args) >= 2 && args[len(args)-2] == "--format" && args[len(args)-1] == "json" {
		for i := 0; i+1 < len(args)-2; i++ {
			if args[i] == "--profile" && args[i+1] == "fixture-corp" {
				args = append(args[:i], args[i+2:]...)
				break
			}
		}
	}
	path := filepath.Join(os.Getenv("MEMGOV_SCENARIO_DIR"), "calls.jsonl")
	previous, _ := os.ReadFile(path)
	step := bytes.Count(previous, []byte("\n"))
	line, _ := json.Marshal(args)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(90)
	}
	if _, err = f.Write(append(line, '\n')); err != nil {
		os.Exit(90)
	}
	if f.Close() != nil {
		os.Exit(90)
	}
	if step >= len(s.Exchanges) || !reflect.DeepEqual(args, s.Exchanges[step].Args) {
		fmt.Fprintln(os.Stderr, `{"error":"unexpected_dws_command"}`)
		os.Exit(91)
	}
	fmt.Println(string(s.Exchanges[step].Output))
	os.Exit(s.Exchanges[step].ExitCode)
}

func TestScenario1Acceptance(t *testing.T) {
	driver := os.Getenv("MEMGOV_SCENARIO_DRIVER")
	if driver == "" {
		driver = filepath.Join(t.TempDir(), "scenario-driver")
		cmd := exec.Command("go", "build", "-o", driver, "../../cmd/scenario-driver")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build driver: %v: %s", err, output)
		}
	}
	if !filepath.IsAbs(driver) {
		t.Fatal("MEMGOV_SCENARIO_DRIVER must be an absolute executable path")
	}
	for _, s := range loadCases(t) {
		t.Run(s.ID, func(t *testing.T) {
			if _, err := runDriver(t, s, driver); err != nil {
				t.Fatal(err)
			}
		})
	}
}
