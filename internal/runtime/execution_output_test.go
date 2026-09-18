package runtime

import (
	"context"
	"encoding/json"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/tasklog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecutionStreamVisibleBeforeCompletion(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	binary := filepath.Join(home, "fake-claude")
	script := `#!/usr/bin/env python3
import json,sys,time,os
assert sys.argv[sys.argv.index('--output-format')+1]=='stream-json'
assert '--verbose' in sys.argv
sys.stdin.read()
print(json.dumps({'type':'assistant','message':{'content':[{'type':'text','text':'正在检查文件'},{'type':'tool_use','name':'Read','input':{'file_path':'report.txt'}}]}}),flush=True)
print('api_key=STDERR-SECRET diagnostic',file=sys.stderr,flush=True)
while not os.path.exists('finish'):time.sleep(.01)
print(json.dumps({'type':'user','message':{'content':[{'type':'tool_result','content':'文件已读取'}]}}),flush=True)
print(json.dumps({'type':'result','structured_output':{'result':'已完成'},'usage':{'input_tokens':3,'output_tokens':4},'total_cost_usd':.01}),flush=True)
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	w, err := tasklog.Open(home, "r", "t", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	in := ExecutionInput{Home: home, WorkDir: work, Trace: w}
	c := &Claude{Binary: binary}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type response struct {
		raw []byte
		err error
	}
	done := make(chan response, 1)
	go func() {
		raw, err := c.invokeExecution(ctx, in, []byte("request"), "--output-format", "json")
		done <- response{raw, err}
	}()
	deadline := time.Now().Add(3 * time.Second)
	visible := false
	for time.Now().Before(deadline) {
		tail, err := tasklog.Read(home, "r", "t", "a", 0)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(tail)
		if strings.Contains(string(raw), "正在检查文件") && strings.Contains(string(raw), "diagnostic") {
			if strings.Contains(string(raw), "STDERR-SECRET") {
				t.Fatal("stderr credential leaked")
			}
			visible = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !visible {
		t.Fatal("output was buffered until completion")
	}
	select {
	case <-done:
		t.Fatal("model ended before released")
	default:
	}
	if err := os.WriteFile(filepath.Join(work, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	var result core.RuntimeAttemptResult
	usage, err := decodeClaude(got.raw, &result)
	if err != nil || result.Result != "已完成" || usage.InputTokens != 3 {
		t.Fatalf("result contract changed: %+v %+v %v", result, usage, err)
	}
}

func TestExecutionStreamCancellationPreservesPartialOutput(t *testing.T) {
	home := t.TempDir()
	binary := filepath.Join(home, "fake-claude")
	script := "#!/usr/bin/env python3\nimport json,time\nprint(json.dumps({'type':'assistant','message':{'content':[{'type':'text','text':'已有进度'}]}}),flush=True)\ntime.sleep(30)\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	w, err := tasklog.Open(home, "r", "t", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&Claude{Binary: binary}).invokeExecution(ctx, ExecutionInput{WorkDir: home, Trace: w}, nil, "--output-format", "json")
		done <- err
	}()
	for {
		tail, err := tasklog.Read(home, "r", "t", "a", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(tail.Events) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("process did not emit progress")
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	err = <-done
	if core.ErrorCode(err) != "unavailable" || time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation stalled: %v", err)
	}
	tail, err := tasklog.Read(home, "r", "t", "a", 0)
	if err != nil || len(tail.Events) != 1 || tail.Events[0].Text != "已有进度" {
		t.Fatalf("partial output lost: %+v %v", tail, err)
	}
}

func TestDirectExecutionOutputIsolatedAcrossReusedProcess(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	c.Run = nil
	in.Task.ID, in.Task.RuntimeID, in.AttemptID = core.NewID(), core.NewID(), core.NewID()
	in.NativeSessionID = in.AttemptID
	in.Task.Messages = []core.RuntimeMessage{{Body: "first request"}}
	binary := filepath.Join(in.Home, "fake-output-claude")
	script := `#!/usr/bin/env python3
import json,sys
for n,line in enumerate(sys.stdin,1):
    print(json.dumps({'type':'assistant','message':{'content':[{'type':'text','text':'progress turn '+str(n)},{'type':'tool_use','name':'Read','input':{'file_path':'turn-'+str(n)+'.txt'}}]}}),flush=True)
    print(json.dumps({'type':'user','message':{'content':[{'type':'tool_result','content':'read result '+str(n)}]}}),flush=True)
    print(json.dumps({'type':'result','subtype':'success','result':'answer '+str(n)}),flush=True)
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c.Binary = binary
	defer c.CloseDirectSessions()
	firstAttempt, firstTask := in.AttemptID, in.Task.ID
	first, err := c.Execute(context.Background(), in)
	if err != nil || first.Result != "answer 1" {
		t.Fatalf("first turn: %+v %v", first, err)
	}
	in.ConversationContext = []core.RuntimeMessage{{Body: "first request"}, {Body: first.Result, SelfAuthored: true}}
	in.Task.ID, in.AttemptID = core.NewID(), core.NewID()
	in.NativeSessionID = in.AttemptID
	in.Task.Messages = []core.RuntimeMessage{{Body: "second request"}}
	second, err := c.Execute(context.Background(), in)
	if err != nil || second.Result != "answer 2" {
		t.Fatalf("private process was not reused: %+v %v", second, err)
	}
	for _, tc := range []struct{ task, attempt, want, absent string }{
		{firstTask, firstAttempt, "progress turn 1", "progress turn 2"},
		{in.Task.ID, in.AttemptID, "progress turn 2", "progress turn 1"},
	} {
		tail, err := tasklog.Read(in.Home, in.Task.RuntimeID, tc.task, tc.attempt, 0)
		raw, _ := json.Marshal(tail)
		if err != nil || !strings.Contains(string(raw), tc.want) || strings.Contains(string(raw), tc.absent) {
			t.Fatalf("attempt output crossed turns: %s %v", raw, err)
		}
	}
}
