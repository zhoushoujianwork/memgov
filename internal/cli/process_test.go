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
	input := `{"uri":"process://source","content":"明确记录：项目部署前检查权限。"}`
	var wg sync.WaitGroup
	codes := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, _ := runBinary(binary, home, input, "source", "ingest", "--input", "-", "--idempotency-key", "same-source")
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
	_, list, _ := runBinary(binary, home, "", "source", "list")
	sources := list["data"].([]any)
	if len(sources) != 1 {
		t.Fatal(list)
	}
	id := sources[0].(map[string]any)["id"].(string)
	_, data, _ := runBinary(binary, home, "", "source", "show", id)
	source := data["data"].(map[string]any)
	f := source["fragments"].([]any)[0].(map[string]any)
	candidate := map[string]any{"reason": "explicit source", "memory": map[string]any{"category": "fact", "title": "部署前检查", "summary": "项目部署前检查权限。", "content": "明确记录：项目部署前检查权限。", "evidence": []any{map[string]any{"source_id": id, "fragment_id": f["id"], "sha256": f["sha256"]}}}}
	raw, _ := json.Marshal(candidate)
	code, c, _ := runBinary(binary, home, string(raw), "candidate", "submit", "--input", "-")
	if code != 0 {
		t.Fatal(c)
	}
	cand := c["data"].(map[string]any)
	code, v, _ := runBinary(binary, home, "", "candidate", "apply", cand["id"].(string), "--expected-digest", cand["digest"].(string))
	if code != 0 {
		t.Fatal(v)
	}
	mid := v["data"].(map[string]any)["memory"].(map[string]any)["id"].(string)
	codes = make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, _ := runBinary(binary, home, "", "memory", "retire", mid, "--expected-version", "1", "--reason", "concurrent update")
			codes <- code
		}()
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
	code, v, _ = runBinary(binary, home, "{invalid", "candidate", "submit", "--input", "-")
	if code != 2 || v["ok"] != false {
		t.Fatal(code, v)
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
	code, v, _ := runBinary(binary, home, `{"uri":"timeout://source","content":"blocked write"}`, "--timeout", "50ms", "source", "ingest", "--input", "-")
	if code != 5 || time.Since(start) > 2*time.Second {
		t.Fatal("timeout contract", code, v, time.Since(start))
	}
}
