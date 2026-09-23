package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/processtree"
	"github.com/zhoushoujianwork/memgov/internal/tasklog"
)

// Execution uses the same policy and result contract, with incremental output.
// Analysis and confirmed actions keep their existing non-stream calls.
func (c *Claude) invokeExecution(ctx context.Context, in ExecutionInput, input []byte, args ...string) ([]byte, error) {
	if c.Run != nil {
		return c.Run(ctx, in.WorkDir, input, args...)
	}
	for i := range args {
		if args[i] == "--output-format" && i+1 < len(args) {
			args[i+1] = "stream-json"
		}
	}
	args = append(args, "--verbose")
	env, err := resolveShellAliasProfile(ctx, c.Profile)
	if err != nil {
		return nil, err
	}
	binary := c.Binary
	if binary == "" {
		binary = "claude"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	finishProcess := processtree.Bind(cmd)
	defer finishProcess()
	cmd.Dir = in.WorkDir
	cmd.Env = mergeEnvironment(os.Environ(), append(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1"))
	if in.ApplicationMode == "proactive" && in.MemgovBinary != "" {
		cmd.Env = ownerAgentEnvironment(in, env, in.MemgovBinary)
	}
	cmd.Stdin = strings.NewReader(string(input))
	cmd.WaitDelay = 5 * time.Second
	stderr := &traceStderr{trace: in.Trace}
	cmd.Stderr = stderr
	defer stderr.flush()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, core.Fail("unavailable", "Claude output pipe failed")
	}
	if err = processtree.Start(ctx, cmd); err != nil {
		return nil, core.Fail("unavailable", "Claude invocation failed")
	}
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-readCtx.Done(); _ = stdout.Close() }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	var result []byte
	for scanner.Scan() {
		processtree.Activity(ctx)
		line := scanner.Bytes()
		in.Trace.Claude(line)
		var header struct{ Type string }
		if json.Unmarshal(line, &header) == nil && header.Type == "result" {
			result = append([]byte(nil), line...)
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil && ctx.Err() == nil && cmd.Cancel != nil {
		_ = cmd.Cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, core.Fail("unavailable", "Claude invocation timed out or was cancelled")
	}
	if scanErr != nil || waitErr != nil {
		return nil, core.Fail("unavailable", "Claude output stream failed")
	}
	if len(result) == 0 {
		return nil, core.Fail("unavailable", "Claude closed before a result")
	}
	return result, nil
}

// A reused private process routes stderr to the current attempt, not its first.
type traceStderr struct {
	mu      sync.Mutex
	trace   *tasklog.Writer
	pending []byte
}

func (s *traceStderr) set(trace *tasklog.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = nil
	s.trace = trace
}
func (s *traceStderr) Write(raw []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, raw...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		s.trace.Emit("stderr", string(s.pending[:i]))
		s.pending = s.pending[i+1:]
	}
	if len(s.pending) > 32<<10 {
		s.pending = nil
		s.trace.Emit("stderr", "[过长的 CLI 诊断行已省略]")
	}
	return len(raw), nil
}
func (s *traceStderr) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		s.trace.Emit("stderr", string(s.pending))
		s.pending = nil
	}
}
