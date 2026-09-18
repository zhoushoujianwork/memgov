package scenarios

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

// This replay driver tests the HARNESS ONLY. It deliberately reads the golden
// result; it is not a DingTalk integration or an implementation of summarization.
func TestReplayDriverHelper(t *testing.T) {
	fixture := os.Getenv("MEMGOV_SCENARIO_FIXTURE")
	if fixture == "" {
		return
	}
	b, err := os.ReadFile(fixture)
	if err != nil {
		os.Exit(92)
	}
	var s scenario
	if json.Unmarshal(b, &s) != nil {
		os.Exit(92)
	}
	var req request
	if json.NewDecoder(os.Stdin).Decode(&req) != nil || !reflect.DeepEqual(req, s.Request) {
		os.Exit(92)
	}
	mode := os.Args[len(os.Args)-1]
	for i, e := range s.Exchanges {
		if mode == "missing_call" && i == len(s.Exchanges)-1 {
			break
		}
		args := e.Args
		if mode == "wrong_command" && i == 0 {
			args = []string{"chat", "message", "send", "--format", "json"}
		}
		cmd := exec.Command("dws", args...)
		// Continue through expected CLI failures: the adapter must translate them
		// to a structured result rather than pretend that history is empty.
		output, runErr := cmd.Output()
		code := 0
		if runErr != nil {
			failure, ok := runErr.(*exec.ExitError)
			if !ok {
				os.Exit(94)
			}
			code = failure.ExitCode()
		}
		var got, want any
		if code != e.ExitCode || json.Unmarshal(output, &got) != nil || json.Unmarshal(e.Output, &want) != nil || !reflect.DeepEqual(got, want) {
			os.Exit(94)
		}
	}
	if mode == "extra_call" {
		_ = exec.Command("dws", "contact", "user", "get-self", "--format", "json").Run()
	}
	if mode == "exit_failure" {
		os.Exit(93)
	}
	if mode == "invalid_json" {
		fmt.Print("not json")
		os.Exit(0)
	}
	if mode == "wrong_status" {
		s.Expected.Status = "made_up"
	}
	if mode == "unsupported_evidence" {
		s.Expected.Findings[0].Evidence = []string{"nonexistent-message"}
	}
	if mode == "stale_status" {
		s.Expected.Findings[0].Status = "blocked"
	}
	if mode == "false_complete" {
		s.Expected.Complete = !s.Expected.Complete
	}
	_ = json.NewEncoder(os.Stdout).Encode(s.Expected)
	if mode == "trailing_json" {
		fmt.Print("{}")
	}
	os.Exit(0)
}

func TestFrameworkReplay(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range loadCases(t) {
		t.Run(s.ID, func(t *testing.T) {
			if _, err := runDriver(t, s, self, "-test.run", "^TestReplayDriverHelper$", "--", "valid"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFrameworkRejectsBrokenDrivers(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := loadCases(t)[0]
	for _, mode := range []string{"missing_call", "extra_call", "wrong_command", "exit_failure", "invalid_json", "trailing_json", "wrong_status", "unsupported_evidence", "stale_status", "false_complete"} {
		t.Run(mode, func(t *testing.T) {
			if _, err := runDriver(t, s, self, "-test.run", "^TestReplayDriverHelper$", "--", mode); err == nil {
				t.Fatal("broken driver was accepted")
			}
		})
	}
}

func TestScenarioFixtures(t *testing.T) {
	ids := map[string]bool{}
	for _, s := range loadCases(t) {
		if s.ID == "" || ids[s.ID] {
			t.Fatalf("empty or duplicate scenario ID %q", s.ID)
		}
		ids[s.ID] = true
		if len(s.Exchanges) == 0 {
			t.Fatalf("%s: no CLI calls", s.ID)
		}
		for _, e := range s.Exchanges {
			args := e.Args
			if len(args) < 4 || !reflect.DeepEqual(args[len(args)-2:], []string{"--format", "json"}) {
				t.Fatalf("%s: missing JSON output flag", s.ID)
			}
			if !json.Valid(e.Output) {
				t.Fatalf("%s: invalid fixture output", s.ID)
			}
		}
		if err := validateResult(s.Expected, s.Expected); err != nil {
			t.Fatalf("%s: %v", s.ID, err)
		}
	}
}
