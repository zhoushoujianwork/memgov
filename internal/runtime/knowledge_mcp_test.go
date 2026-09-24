package runtime

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestKnowledgeMCPOnlyAddsNamedReadToolsToOwnerRuns(t *testing.T) {
	mcp := &core.RuntimeKnowledgeMCP{Command: "/usr/bin/node", Args: []string{"/opt/relayer/bin.js", "mcp", "--read-only"}, Sources: []string{"dokki", "confluence"}}
	for _, mode := range []string{"direct", "proactive", "group_mention"} {
		in := ExecutionInput{ApplicationMode: mode, KnowledgeMCP: mcp}
		config, allowed, prompt := knowledgeMCPConfiguration(in)
		if mode == "group_mention" {
			if config != emptyMCPConfig || len(allowed) != 0 || prompt != "" {
				t.Fatal("group Agent received Owner MCP tools")
			}
			continue
		}
		var parsed struct {
			MCPServers map[string]struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(config), &parsed); err != nil || parsed.MCPServers["relayer"].Command != mcp.Command {
			t.Fatalf("invalid managed MCP config: %s, %v", config, err)
		}
		if len(allowed) != 7 || !strings.Contains(strings.Join(allowed, ","), "mcp__relayer__read_dokki_resource") || !strings.Contains(strings.Join(allowed, ","), "mcp__relayer__query_confluence") || strings.Contains(prompt, "write") {
			t.Fatalf("unexpected source tools or prompt: %v %q", allowed, prompt)
		}
	}
}

func TestDirectAgentMCPChangeReplacesNativeSession(t *testing.T) {
	_, in, _ := directAgentFixture(t)
	in.ApplicationMode = "direct"
	base := directPolicyDigest(in, "", "")
	in.KnowledgeMCP = &core.RuntimeKnowledgeMCP{Command: "/usr/bin/node", Args: []string{"/opt/relayer/bin.js", "mcp", "--read-only"}, Sources: []string{"dokki"}}
	if base == directPolicyDigest(in, "", "") {
		t.Fatal("MCP policy change would reuse the old native session")
	}
	args := directClaudeArgs(in, "", "", "")
	values := claudeArgumentValues(args)
	if !strings.Contains(values["--mcp-config"], `"relayer"`) || !strings.Contains(values["--allowedTools"], "mcp__relayer__search_dokki") || strings.Contains(values["--allowedTools"], "mcp__relayer__search_confluence") {
		t.Fatalf("direct Agent tool selection is wrong: %v", values)
	}
}

func TestProactiveAgentReceivesKnowledgeMCP(t *testing.T) {
	c, in, _ := directAgentFixture(t)
	in.ApplicationMode = "proactive"
	in.BashEnabled = true
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in.KnowledgeMCP = &core.RuntimeKnowledgeMCP{Command: executable, Args: []string{"/opt/relayer/bin.js", "mcp", "--read-only"}, Sources: []string{"confluence"}}
	c.Run = func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
		values := claudeArgumentValues(args)
		if !strings.Contains(values["--mcp-config"], `"relayer"`) || !strings.Contains(values["--allowedTools"], "mcp__relayer__search_confluence") || strings.Contains(values["--allowedTools"], "mcp__relayer__search_dokki") {
			t.Fatalf("proactive Agent source tools are wrong: %v", values)
		}
		return claudeResult(t, core.RuntimeAttemptResult{Result: "done"}), nil
	}
	if _, err := c.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeMCPFailsClosedWhenExecutableIsMissing(t *testing.T) {
	in := ExecutionInput{ApplicationMode: "direct", BashEnabled: true, KnowledgeMCP: &core.RuntimeKnowledgeMCP{
		Command: t.TempDir() + "/missing", Args: []string{"mcp", "--read-only"}, Sources: []string{"dokki"},
	}}
	if err := validateKnowledgeMCP(in); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("missing MCP command did not fail before the model starts: %v", err)
	}
	in.ApplicationMode = "group_mention"
	if err := validateKnowledgeMCP(in); core.ErrorCode(err) != "denied" {
		t.Fatalf("group Agent received Owner source grant: %v", err)
	}
}

func TestBuiltInKnowledgeMCPUsesMemgovBinaryAndSelectedSources(t *testing.T) {
	in := ExecutionInput{ApplicationMode: "direct", BashEnabled: true, Home: t.TempDir(), KnowledgeMCP: &core.RuntimeKnowledgeMCP{Sources: []string{"dokki", "confluence"}}}
	if err := validateKnowledgeMCP(in); err != nil {
		t.Fatal(err)
	}
	config, allowed, _ := knowledgeMCPConfiguration(in)
	var parsed struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(config), &parsed); err != nil {
		t.Fatal(err)
	}
	server, ok := parsed.MCPServers["memgov_knowledge"]
	if !ok || server.Command == "" || !strings.Contains(strings.Join(server.Args, " "), "knowledge-mcp") || strings.Contains(config, "relayer") {
		t.Fatalf("not self-contained: %s", config)
	}
	if len(allowed) != 7 || !strings.Contains(strings.Join(allowed, ","), "mcp__memgov_knowledge__search_dokki") || !strings.Contains(strings.Join(allowed, ","), "mcp__memgov_knowledge__query_confluence") {
		t.Fatalf("wrong tool grant: %v", allowed)
	}
	in.ApplicationMode = "group_mention"
	if err := validateKnowledgeMCP(in); core.ErrorCode(err) != "denied" {
		t.Fatalf("group received MCP: %v", err)
	}
}
