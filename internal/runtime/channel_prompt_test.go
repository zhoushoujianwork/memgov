package runtime

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestDingTalkChannelSystemPromptsKeepConversationBoundaries(t *testing.T) {
	direct := channelSystemPrompt(core.Channel{Provider: "dingtalk", Kind: core.ChannelDingTalkApp}, core.Route{ConversationType: "direct"}, "direct", "corp-owner", "work-chat")
	for _, expected := range []string{`memgov message query "work-chat"`, "returned conversation type and watermark", "empty result is not proof of absence", "one-to-one DingTalk chat", "before considering broad long-term-memory recall", "invoke the installed dws skill", "report that concrete failure", "Do not search all workspace knowledge first", "query the controlled workspace tool", "runtime session directory is scratch space", `profile "corp-owner"`, "never guess an identity from memory", "Ordinary answers", "application bot", "chat +messages-send", "--as user", "--format json", "DWS owner user", "AI marker", "preserve idempotency", "parent-command help probe"} {
		if !strings.Contains(direct, expected) {
			t.Fatalf("direct channel prompt omitted %q: %s", expected, direct)
		}
	}
	if strings.Contains(direct, "Every message originating from this group Agent") {
		t.Fatalf("owner direct prompt acquired the group-only sending rule: %s", direct)
	}

	directWithoutProfile := channelSystemPrompt(core.Channel{Provider: "dingtalk", Kind: core.ChannelDingTalkApp}, core.Route{ConversationType: "direct"}, "direct", "", "")
	for _, expected := range []string{"No bound DWS profile", "do not use an ambient or guessed DWS identity", "prepare the operation"} {
		if !strings.Contains(directWithoutProfile, expected) {
			t.Fatalf("unbound direct channel prompt omitted %q: %s", expected, directWithoutProfile)
		}
	}
	if strings.Contains(directWithoutProfile, "chat +messages-send") {
		t.Fatalf("unbound direct channel advertised a DWS send path: %s", directWithoutProfile)
	}

	group := channelSystemPrompt(core.Channel{Provider: "dingtalk", Kind: core.ChannelDingTalkApp}, core.Route{ConversationType: "group"}, "group_mention", "corp-owner", "work-chat")
	for _, expected := range []string{"group mention", "same-group history", "application bot identity", "Do not call dws", "never use DWS --as user", "even when the owner is the requester", "bot-scoped MCP", "pending confirmation", "Do not inspect a member's private chat"} {
		if !strings.Contains(group, expected) {
			t.Fatalf("group channel prompt omitted %q: %s", expected, group)
		}
	}
	if strings.Contains(group, "corp-owner") || strings.Contains(group, "one-to-one DingTalk chat") {
		t.Fatalf("group channel prompt exposed owner-private lookup context: %s", group)
	}

	if got := channelSystemPrompt(core.Channel{Provider: "another"}, core.Route{ConversationType: "direct"}, "direct", "", ""); got != "" {
		t.Fatalf("unknown provider received a guessed prompt: %q", got)
	}
}
