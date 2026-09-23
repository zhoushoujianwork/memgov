package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agentworkspace"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func buildCLI(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "memgov")
	cmd := exec.Command("go", "build", "-o", p, "../../cmd/memgov")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, raw)
	}
	return p
}
func runBinary(binary, home, input string, args ...string) (int, map[string]any, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{"--home", home}, args...)...)
	cmd.Stdin = bytes.NewBufferString(input)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else {
			code = -1
		}
	}
	var value map[string]any
	if e := json.Unmarshal(out.Bytes(), &value); e != nil {
		return -2, nil, out.String() + stderr.String()
	}
	return code, value, stderr.String()
}
func TestBinaryConcurrentCASAndIdempotency(t *testing.T) {
	binary := buildCLI(t)
	home := t.TempDir()
	if code, v, _ := runBinary(binary, home, "", "init"); code != 0 {
		t.Fatal(v)
	}
	ref, err := agentworkspace.OwnerRef("process-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = agentworkspace.Ensure(home, ref); err != nil {
		t.Fatal(err)
	}
	input := `{"path":"notes/process.md","content":"verified experience","expected_digest":""}`
	var wg sync.WaitGroup
	codes := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, _ := runBinary(binary, home, input, "agent", "workspace", "write", "--workspace-id", ref.ID, "--input", "-", "--idempotency-key", "same-write")
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 0 {
			t.Fatal("concurrent idempotency", code)
		}
	}
	doc, err := agentworkspace.Read(home, ref.ID, "notes/process.md")
	if err != nil {
		t.Fatal(err)
	}
	codes = make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			input := core.JSON(map[string]any{"path": doc.Path, "content": fmt.Sprintf("unique update %d", i), "expected_digest": doc.Digest})
			code, _, _ := runBinary(binary, home, input, "agent", "workspace", "write", "--workspace-id", ref.ID, "--input", "-")
			codes <- code
		}(i)
	}
	wg.Wait()
	close(codes)
	wins, conflicts := 0, 0
	for code := range codes {
		if code == 0 {
			wins++
		} else if code == 3 {
			conflicts++
		} else {
			t.Fatal("unexpected code", code)
		}
	}
	if wins != 1 || conflicts != 11 {
		t.Fatal(wins, conflicts)
	}
	revisions, err := agentworkspace.History(home, ref.ID, doc.Path)
	if err != nil || len(revisions) != 2 {
		t.Fatal(revisions, err)
	}
	if code, out, _ := runBinary(binary, home, "{invalid", "agent", "workspace", "write", "--workspace-id", ref.ID, "--input", "-"); code != 2 {
		t.Fatal(code, out)
	}
}

func TestDatabaseLockHelper(t *testing.T) {
	if os.Getenv("MEMGOV_TEST_LOCK_HELPER") != "1" {
		return
	}
	s, err := core.Open(context.Background(), os.Getenv("MEMGOV_TEST_HOME")+"/state.db", false)
	if err != nil {
		panic(err)
	}
	defer s.Close()
	_, err = s.Mutate(context.Background(), core.Request{}, func(*core.Tx) (any, error) {
		fmt.Fprintln(os.Stdout, "ready")
		var b [1]byte
		os.Stdin.Read(b[:])
		return nil, nil
	})
	if err != nil {
		panic(err)
	}
}
func TestBinaryTimeoutUnderAnotherProcessWriteLock(t *testing.T) {
	binary := buildCLI(t)
	home := t.TempDir()
	runBinary(binary, home, "", "init")
	helper := exec.Command(os.Args[0], "-test.run=^TestDatabaseLockHelper$")
	helper.Env = append(os.Environ(), "MEMGOV_TEST_LOCK_HELPER=1", "MEMGOV_TEST_HOME="+home)
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); helper.Wait() }()
	ready := make([]byte, 6)
	if _, err = io.ReadFull(stdout, ready); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, v, _ := runBinary(binary, home, "", "--timeout", "50ms", "workspace", "add", "blocked")
	if code != 5 || time.Since(start) > 2*time.Second {
		t.Fatal("timeout contract", code, v, time.Since(start))
	}
}
