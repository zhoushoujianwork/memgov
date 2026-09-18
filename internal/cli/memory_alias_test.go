package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMemoryAliasReadsAndIdempotentWrites(t *testing.T) {
	home := t.TempDir()
	if code, result := invoke(t, home, "", "init"); code != 0 {
		t.Fatal(code, result)
	}
	id := formalMemory(t, home)
	for _, args := range [][]string{{"list", "--status", "active", "--limit", "1"}, {"show", id}, {"history", id}} {
		longCode, long := invoke(t, home, "", append([]string{"memory"}, args...)...)
		shortCode, short := invoke(t, home, "", append([]string{"m"}, args...)...)
		if longCode != 0 || shortCode != 0 || !reflect.DeepEqual(long["data"], short["data"]) {
			t.Fatalf("%v: full=%d %v alias=%d %v", args, longCode, long, shortCode, short)
		}
	}
	args := []string{"retire", id, "--expected-version", "1", "--reason", "alias regression", "--idempotency-key", "retire-once"}
	if code, result := invoke(t, home, "", append([]string{"memory"}, args...)...); code != 0 {
		t.Fatal(code, result)
	}
	code, replay := invoke(t, home, "", append([]string{"m"}, args...)...)
	if code != 0 || replay["cached"] != true {
		t.Fatalf("alias did not replay the same command: %d %v", code, replay)
	}
	code, history := invoke(t, home, "", "m", "history", id)
	if code != 0 || len(history["data"].([]any)) != 2 {
		t.Fatalf("alias duplicated the mutation: %d %v", code, history)
	}
	code, conflict := invoke(t, home, "", "m", "retire", id, "--expected-version", "1", "--reason", "changed input", "--idempotency-key", "retire-once")
	if code != 3 || conflict["error"].(map[string]any)["code"] != "conflict" {
		t.Fatalf("alias bypassed idempotency conflict: %d %v", code, conflict)
	}
}

func TestMemoryAliasHelpDoesNotInitialize(t *testing.T) {
	home := filepath.Join(t.TempDir(), "uninitialized")
	for _, args := range [][]string{{"m", "--help"}, {"m", "list", "--help"}} {
		var out, errOut bytes.Buffer
		code := Run(context.Background(), append([]string{"--home", home}, args...), strings.NewReader(""), &out, &errOut)
		if code != 0 || !strings.Contains(out.String(), "memgov memory") {
			t.Fatalf("%v: %d stdout=%s stderr=%s", args, code, out.String(), errOut.String())
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("help initialized the data root: %v", err)
	}
}

func TestMemoryListHumanOutput(t *testing.T) {
	home := t.TempDir()
	if code, result := invoke(t, home, "", "init"); code != 0 {
		t.Fatal(code, result)
	}
	id := formalMemory(t, home)
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"--home", home, "m", "list", "--human", "--limit", "1"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("human list: %d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"VERSION", "STATUS", "CATEGORY", "WORKSPACE", "TITLE", "ID", id} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("human output missing %q: %s", want, out.String())
		}
	}
	if strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Fatalf("human output is JSON: %s", out.String())
	}
}

func TestMemoryListDefaultsToActiveAndAllIncludesHistory(t *testing.T) {
	home := t.TempDir()
	if code, result := invoke(t, home, "", "init"); code != 0 {
		t.Fatal(code, result)
	}
	id := formalMemory(t, home)
	if code, result := invoke(t, home, "", "m", "retire", id, "--expected-version", "1", "--reason", "list filter test"); code != 0 {
		t.Fatal(code, result)
	}
	code, active := invoke(t, home, "", "m", "list")
	if code != 0 || len(active["data"].([]any)) != 0 {
		t.Fatalf("default list exposed inactive history: %d %v", code, active)
	}
	code, all := invoke(t, home, "", "m", "list", "--status", "all")
	if code != 0 || len(all["data"].([]any)) != 1 {
		t.Fatalf("all list omitted history: %d %v", code, all)
	}
	code, invalid := invoke(t, home, "", "m", "list", "--status", "unknown")
	if code != 2 || invalid["error"].(map[string]any)["code"] != "invalid_input" {
		t.Fatalf("invalid status accepted: %d %v", code, invalid)
	}
}
