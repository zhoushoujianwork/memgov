package cli

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

const knowledgeMCPAgent = `agents:
  owner:
    bash: true
    knowledge_mcp:
      command: /usr/bin/node
      args: [/opt/relayer/bin.js, mcp, --read-only, --actor, memgov, --scope, global]
      sources: [dokki, confluence]
`

func TestOwnerKnowledgeMCPRequiresReadOnlyRelayerAndNamedSources(t *testing.T) {
	for name, raw := range map[string]string{
		"relative command":  strings.Replace(knowledgeMCPAgent, "/usr/bin/node", "node", 1),
		"missing read only": strings.Replace(knowledgeMCPAgent, ", --read-only", "", 1),
		"write grant":       strings.Replace(knowledgeMCPAgent, "--read-only", "--read-only, --allow-write", 1),
		"unknown source":    strings.Replace(knowledgeMCPAgent, "confluence", "drive", 1),
		"duplicate source":  strings.Replace(knowledgeMCPAgent, "confluence", "dokki", 1),
		"unknown nested":    strings.Replace(knowledgeMCPAgent, "      sources: [dokki, confluence]", "      sources: [dokki, confluence]\n      token: DO_NOT_ECHO_SECRET", 1),
	} {
		t.Run(name, func(t *testing.T) {
			a := &app{}
			if err := a.loadConfig([]byte(raw)); core.ErrorCode(err) != "invalid_input" {
				t.Fatalf("unsafe knowledge MCP accepted: %v", err)
			}
		})
	}
	a := &app{}
	if err := a.loadConfig([]byte(knowledgeMCPAgent)); err != nil {
		t.Fatal(err)
	}
	validated, err := NormalizeDualModeConfig(a.cfg)
	if err != nil || validated.Declaration.Agents["owner"].KnowledgeMCP == nil {
		t.Fatalf("valid Owner source connection rejected: %v", err)
	}
}

func TestGroupAgentCanDeclareReadOnlyKnowledgeWithoutBash(t *testing.T) {
	raw := strings.Replace(dualConfigYAML, "    capabilities: [conversation_history_read, artifact_create]", "    capabilities: [conversation_history_read, artifact_create]\n    bash: false\n    knowledge_mcp:\n      sources: [dokki, confluence]", 1)
	a := &app{}
	if err := a.loadConfig([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	validated, err := NormalizeDualModeConfig(a.cfg)
	if err != nil || validated.Declaration.Agents["group-helper"].KnowledgeMCP == nil {
		t.Fatalf("read-only group source rejected: %v", err)
	}
}

func TestKnowledgeMCPChangeRequiresAuthorization(t *testing.T) {
	mcp := &core.RuntimeKnowledgeMCP{Command: "/usr/bin/node", Args: []string{"/opt/relayer/bin.js", "mcp", "--read-only"}, Sources: []string{"dokki"}}
	before := AgentDeclaration{ExternalActions: "owner_confirmation"}
	after := before
	after.KnowledgeMCP = mcp
	change := PlanChange{}
	classifyPermissionChange(&change, before, after)
	if !change.PermissionExpansion || !change.BoundaryChange {
		t.Fatalf("knowledge source admission was not classified as a permission and boundary change: %+v", change)
	}
}

func TestBuiltInKnowledgeDeclarationNeedsOnlySourceNames(t *testing.T) {
	raw := "agents:\n  owner:\n    bash: true\n    knowledge_mcp:\n      sources: [dokki, confluence]\n"
	a := &app{}
	if err := a.loadConfig([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeDualModeConfig(a.cfg); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(raw, "sources: [dokki, confluence]", "command: /usr/bin/node\n      sources: [dokki, confluence]", 1)
	if err := (&app{}).loadConfig([]byte(bad)); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("half legacy declaration accepted: %v", err)
	}
}
