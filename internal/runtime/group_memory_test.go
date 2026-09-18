package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestGroupMemoryToolBindsTargetAndRejectsWrites(t *testing.T) {
	home := filepath.Join(t.TempDir(), "owner's home")
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "memgov"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	in := ExecutionInput{Home: home, WorkDir: t.TempDir(), ApplicationMode: "group_mention", ChannelID: "app-bot", ConversationID: "cid'group", Capabilities: []string{"memory_read"}}
	tool, err := prepareGroupMemoryTool(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"latest"}, {"latest", "20"}, {"recall", "literal $(touch /tmp/not-executed); --home evil"}, {"show", "memory-id"}} {
		out, err := exec.Command(tool, argv...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s %v", argv, out, err)
		}
		if !strings.Contains(string(out), home+"\n") || !strings.Contains(string(out), "--conversation\ncid'group\n") || !strings.Contains(string(out), "app-bot\n") {
			t.Fatalf("target changed: %s", out)
		}
		if argv[0] == "recall" && !strings.Contains(string(out), argv[1]+"\n") {
			t.Fatalf("query was interpreted as code: %s", out)
		}
	}
	for _, argv := range [][]string{{}, {"publish", "id"}, {"latest", "21"}, {"latest", "1", "--home", "evil"}, {"recall", "x", "--conversation", "other"}, {"show", "--home"}, {"show", "id", "extra"}} {
		if out, err := exec.Command(tool, argv...).CombinedOutput(); err == nil {
			t.Fatalf("unsafe operation permitted: %v %s", argv, out)
		}
	}
	preset, err := agent.Enable(context.Background(), home, "claude", "claude-default")
	if err != nil {
		t.Fatal(err)
	}
	in.Preset = preset
	var args []string
	c := &Claude{Run: func(_ context.Context, _ string, _ []byte, argv ...string) ([]byte, error) {
		args = append([]string{}, argv...)
		return claudeResult(t, core.RuntimeAttemptResult{Result: "answer"}), nil
	}}
	if _, err = c.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			values[args[i]] = args[i+1]
		}
	}
	if values["--allowedTools"] != "Bash("+tool+" *)" || values["--tools"] != "Bash" || values["--setting-sources"] != "" {
		t.Fatalf("group inherited arbitrary tools: %+v", values)
	}
}
