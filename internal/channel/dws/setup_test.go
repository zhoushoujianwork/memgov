package dws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestDiscoverRuntimeSetupResolvesCurrentAccountAndUniqueChoices(t *testing.T) {
	adapter := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "profile list"):
			// This is the current dws envelope: projected fields sit beside success.
			return []byte(`{"success":true,"currentProfile":"corp-a","profiles":[{"profile":"corp-a","corpId":"corp-a"}]}`), nil
		case strings.HasPrefix(joined, "contact +me"):
			return []byte(`{"ok":true,"data":{"name":"Owner","userId":"owner-1"}}`), nil
		case strings.HasPrefix(joined, "chat +chat-search"):
			return []byte(`{"chats":[{"name":"Runtime Test","openConversationId":"cid-group"}]}`), nil
		case strings.HasPrefix(joined, "chat +bot-search"):
			return []byte(`{"robots":[{"robotCode":"robot-1","robotName":"Delivery Bot"}]}`), nil
		default:
			t.Fatalf("unexpected dws call: %s", joined)
			return nil, nil
		}
	}}
	got, err := adapter.DiscoverRuntimeSetup(context.Background(), RuntimeSetupRequest{Watch: "Runtime Test", RobotCode: "robot-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "corp-a" || got.CorpID != "corp-a" || got.OwnerUserID != "owner-1" || got.ConversationID != "cid-group" || got.RobotCode != "robot-1" || got.DeliveryID != "owner-1" {
		t.Fatalf("unexpected discovery: %+v", got)
	}
}

func TestDiscoverRuntimeSetupAcceptsExplicitEnterpriseRobot(t *testing.T) {
	adapter := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "profile list"):
			return []byte(`{"currentProfile":"corp-a","profiles":[{"profile":"corp-a","corpId":"corp-a"}]}`), nil
		case strings.HasPrefix(joined, "contact +me"):
			return []byte(`{"data":{"userId":"owner-1"}}`), nil
		case strings.HasPrefix(joined, "chat +chat-list-all"):
			return []byte(`{"complete":true,"groups":[{"name":"Work","openConversationId":"cid-work"}]}`), nil
		case strings.HasPrefix(joined, "chat +search-msg"):
			return []byte(`{"complete":true,"messages":[{"conversationId":"cid-work"}]}`), nil
		case strings.HasPrefix(joined, "chat +bot-search"):
			return []byte(`{"robots":[]}`), nil
		case strings.HasPrefix(joined, "chat +chat-bots"):
			return []byte(`{"bots":[{"name":"Enterprise Bot","openBotId":"bot-open"}]}`), nil
		default:
			t.Fatalf("unexpected dws call: %s", joined)
			return nil, nil
		}
	}}
	got, err := adapter.DiscoverRuntimeSetup(context.Background(), RuntimeSetupRequest{RobotCode: "enterprise-app", RobotName: "Enterprise Bot"})
	if err != nil {
		t.Fatal(err)
	}
	if got.RobotCode != "enterprise-app" || got.DeliveryID != "owner-1" {
		t.Fatalf("unexpected enterprise robot discovery: %+v", got)
	}
}

func TestDiscoverRuntimeSetupRefusesAmbiguousRobot(t *testing.T) {
	adapter := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "profile":
			return []byte(`{"currentProfile":"corp-a","profiles":[{"profile":"corp-a","corpId":"corp-a"}]}`), nil
		case args[0] == "contact":
			return []byte(`{"data":{"userId":"owner-1"}}`), nil
		case args[0] == "chat" && args[1] == "+bot-search":
			return []byte(`{"robots":[{"robotCode":"one","robotName":"Bot"},{"robotCode":"two","robotName":"Bot"}]}`), nil
		default:
			t.Fatalf("unexpected dws call: %v", args)
			return nil, nil
		}
	}}
	_, err := adapter.DiscoverRuntimeSetup(context.Background(), RuntimeSetupRequest{Watch: "cid-group", RobotName: "Bot"})
	if err == nil || !strings.Contains(err.Error(), "pass --robot-code") {
		t.Fatalf("expected a useful ambiguity error, got %v", err)
	}
}

func TestDiscoverRuntimeSetupWatchesAllGroupsAndMarksIgnore(t *testing.T) {
	adapter := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "profile list"):
			return []byte(`{"currentProfile":"corp-a","profiles":[{"profile":"corp-a","corpId":"corp-a"}]}`), nil
		case strings.HasPrefix(joined, "contact +me"):
			return []byte(`{"data":{"userId":"owner-1"}}`), nil
		case strings.HasPrefix(joined, "chat +chat-list-all"):
			return []byte(`{"complete":true,"groups":[{"name":"Work","openConversationId":"cid-work"},{"name":"Noise","openConversationId":"cid-noise"}]}`), nil
		case strings.HasPrefix(joined, "chat +search-msg"):
			return []byte(`{"complete":true,"messages":[{"conversationId":"cid-work"},{"conversationId":"cid-noise"}]}`), nil
		default:
			t.Fatalf("unexpected dws call: %s", joined)
			return nil, nil
		}
	}}
	got, err := adapter.DiscoverRuntimeSetup(context.Background(), RuntimeSetupRequest{Ignore: []string{"Noise"}, DeliveryConversationID: "cid-direct"})
	if err != nil {
		t.Fatal(err)
	}
	if got.RobotCode != "" || got.RobotName != "" || !got.GroupDiscoveryComplete || len(got.Conversations) != 2 || got.Conversations[0].Ignored || !got.Conversations[1].Ignored {
		t.Fatalf("unexpected group scope: %+v", got.Conversations)
	}
}

func TestListGroupConversationsKeepsActiveGroupsContainingDeliveryRobot(t *testing.T) {
	adapter := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "chat +chat-list-all"):
			return []byte(`{"complete":true,"groups":[{"name":"Managed","openConversationId":"cid-managed"},{"name":"Other","openConversationId":"cid-other"}]}`), nil
		case strings.HasPrefix(joined, "chat +search-msg"):
			return []byte(`{"complete":true,"messages":[{"conversationId":"cid-managed"},{"conversationId":"cid-other"}]}`), nil
		case strings.Contains(joined, "chat +chat-bots --group cid-managed"):
			return []byte(`{"bots":[{"name":"Delivery","openBotId":"bot-open"}]}`), nil
		case strings.Contains(joined, "chat +chat-bots --group cid-other"):
			return []byte(`{"bots":[{"name":"Another Bot","openBotId":"other-open"}]}`), nil
		default:
			t.Fatalf("unexpected dws call: %s", joined)
			return nil, nil
		}
	}}
	groups, err := adapter.ListGroupConversations(context.Background(), channel.Config{Identity: core.ChannelIdentity{Profile: "corp-a", DeliveryRobotName: "Delivery"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != "cid-managed" {
		t.Fatalf("filtered groups = %+v", groups)
	}
}

func TestGroupMembershipAmbiguousRobotNameIsRejected(t *testing.T) {
	a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		return []byte(`{"bots":[{"name":"Bot"},{"name":"Bot"}]}`), nil
	}}
	_, err := a.groupsWithRobot(context.Background(), channel.Config{}, []channel.GroupConversation{{ID: "group"}}, "Bot")
	if core.ErrorCode(err) != "denied" {
		t.Fatalf("ambiguous robot match accepted: %v", err)
	}
}

func TestPartialActiveSearchYieldsOnlyVerifiedPositiveGroups(t *testing.T) {
	calls := 0
	a := &Adapter{Run: func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		joined := strings.Join(args, " ")
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Minute {
			t.Fatal("unbounded discovery")
		}
		switch {
		case strings.HasPrefix(joined, "chat +chat-list-all"):
			return []byte(`{"complete":true,"groups":[{"openConversationId":"cid-seen"},{"openConversationId":"cid-missed"}]}`), nil
		case strings.HasPrefix(joined, "chat +search-msg"):
			if !strings.Contains(joined, "--start ") || !strings.Contains(joined, "--end ") {
				t.Fatal("search range not frozen")
			}
			return []byte(`{"complete":false,"hasMore":true,"count":500,"messages":[{"conversationId":"cid-seen"}]}`), nil
		case strings.Contains(joined, "+chat-bots --group cid-seen"):
			return []byte(`{"bots":[{"name":"Bot"}]}`), nil
		default:
			t.Fatalf("unseen group caused a query: %s", joined)
			return nil, nil
		}
	}}
	result, err := a.DiscoverGroupConversations(context.Background(), channel.Config{Identity: core.ChannelIdentity{DeliveryRobotName: "Bot"}})
	if err != nil || result.Complete || len(result.Groups) != 1 || result.Groups[0].ID != "cid-seen" || len(result.Excluded) != 0 || calls != 3 {
		t.Fatalf("partial positive: %+v calls=%d err=%v", result, calls, err)
	}
	if _, err = a.ListGroupConversations(context.Background(), channel.Config{Identity: core.ChannelIdentity{DeliveryRobotName: "Bot"}}); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("legacy caller could replace scope with partial: %v", err)
	}
}
func TestActiveGroupDiscoveryRejectsUnresolvedEmptySearch(t *testing.T) {
	for name, payload := range map[string]string{
		"cap":                             `{"complete":false,"hasMore":true,"messages":[]}`,
		"contradictory":                   `{"complete":true,"hasMore":true,"messages":[]}`,
		"missing completeness":            `{"messages":[]}`,
		"page limit without completeness": `{"failedCount":1,"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 10."}],"messages":[{"conversationId":"cid-old"}]}`,
		"positive without completeness":   `{"messages":[{"conversationId":"cid-old"}]}`,
		"partial failure":                 `{"complete":true,"failures":[{"code":"denied"}],"messages":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
				if args[1] == "+chat-list-all" {
					return []byte(`{"complete":true,"groups":[{"openConversationId":"cid-old"}]}`), nil
				}
				return []byte(payload), nil
			}}
			_, err := a.DiscoverGroupConversations(context.Background(), channel.Config{})
			if core.ErrorCode(err) != "unavailable" {
				t.Fatalf("unresolved empty result accepted: %v", err)
			}
		})
	}
}
func TestGroupMembershipIncompleteResultCannotProveRobotExit(t *testing.T) {
	a := &Adapter{Run: func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"complete":false,"hasMore":true,"bots":[]}`), nil
	}}
	_, err := a.groupsWithRobot(context.Background(), channel.Config{}, []channel.GroupConversation{{ID: "cid-group"}}, "Bot")
	if core.ErrorCode(err) != "unavailable" {
		t.Fatalf("partial bot list proved absence: %v", err)
	}
}

func TestLargeGroupDiscoveryBoundsRobotCallsAndKeepsPositiveProofs(t *testing.T) {
	groups := []map[string]string{}
	messages := []map[string]string{}
	for i := 0; i < 1519; i++ {
		id := fmt.Sprintf("cid-%d", i)
		groups = append(groups, map[string]string{"openConversationId": id})
		if i < 70 {
			messages = append(messages, map[string]string{"conversationId": id})
		}
	}
	groupJSON, _ := json.Marshal(map[string]any{"complete": true, "groups": groups})
	activityJSON, _ := json.Marshal(map[string]any{"complete": false, "hasMore": true, "messages": messages})
	calls := 0
	a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		switch args[1] {
		case "+chat-list-all":
			return groupJSON, nil
		case "+search-msg":
			return activityJSON, nil
		case "+chat-bots":
			return []byte(`{"bots":[{"name":"Bot"}]}`), nil
		default:
			t.Fatal(args)
			return nil, nil
		}
	}}
	result, err := a.DiscoverGroupConversations(context.Background(), channel.Config{Identity: core.ChannelIdentity{DeliveryRobotName: "Bot"}})
	if err != nil || result.Complete || len(result.Groups) != 64 || calls != 66 {
		t.Fatalf("discovery budget/proofs: groups=%d complete=%v calls=%d err=%v", len(result.Groups), result.Complete, calls, err)
	}
}

func TestPageLimitedSearchLedgerRetainsVerifiedGroupEvidence(t *testing.T) {
	// Match the CLI's large-account shape: a complete directory, 500 results
	// across 16 active groups, and a page-limit entry in its failure ledger.
	groups := make([]map[string]string, 1518)
	for i := range groups {
		groups[i] = map[string]string{"openConversationId": fmt.Sprintf("cid-%d", i)}
	}
	messages := make([]map[string]string, 500)
	for i := range messages {
		messages[i] = map[string]string{"conversationId": fmt.Sprintf("cid-%d", i%16)}
	}
	listed, _ := json.Marshal(map[string]any{"complete": true, "groups": groups})
	for _, reportedComplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("reported-complete-%t", reportedComplete), func(t *testing.T) {
			activity, _ := json.Marshal(map[string]any{"complete": reportedComplete, "hasMore": !reportedComplete, "count": 500, "messages": messages, "failedCount": 1, "failures": []map[string]any{{"stage": "search-page-limit", "error": "Reached page-limit of 10."}}})
			botCalls := 0
			a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
				switch args[1] {
				case "+chat-list-all":
					return listed, nil
				case "+search-msg":
					return activity, nil
				case "+chat-bots":
					botCalls++
					if args[3] == "cid-0" || args[3] == "cid-1" {
						return []byte(`{"bots":[{"name":"Bot"}]}`), nil
					}
					return []byte(`{"bots":[]}`), nil
				default:
					t.Fatalf("unexpected provider operation: %v", args)
					return nil, nil
				}
			}}
			result, err := a.DiscoverGroupConversations(context.Background(), channel.Config{Identity: core.ChannelIdentity{DeliveryRobotName: "Bot"}})
			if err != nil || result.Complete || len(result.Groups) != 2 || result.Groups[0].ID != "cid-0" || result.Groups[1].ID != "cid-1" || botCalls != 16 || len(result.Excluded) != 14 {
				t.Fatalf("page-limited positives lost or authorized absence: %+v bots=%d err=%v", result, botCalls, err)
			}
		})
	}
}

func TestPageLimitedActivitySearchFindsOlderGroupsBySplittingWindow(t *testing.T) {
	searches := 0
	a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		switch args[1] {
		case "+chat-list-all":
			return []byte(`{"complete":true,"groups":[{"openConversationId":"cid-older"},{"openConversationId":"cid-newer"}]}`), nil
		case "+search-msg":
			searches++
			flag := func(name string) string {
				for i := 0; i+1 < len(args); i++ {
					if args[i] == name {
						return args[i+1]
					}
				}
				return ""
			}
			start, err := time.Parse(time.RFC3339, flag("--start"))
			if err != nil {
				t.Fatal(err)
			}
			end, err := time.Parse(time.RFC3339, flag("--end"))
			if err != nil {
				t.Fatal(err)
			}
			if end.Sub(start) > 20*24*time.Hour {
				return []byte(`{"complete":false,"hasMore":true,"failedCount":1,"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 10."}],"messages":[{"conversationId":"cid-newer"}]}`), nil
			}
			if end.Before(time.Now().Add(-10 * 24 * time.Hour)) {
				return []byte(`{"complete":true,"messages":[{"conversationId":"cid-older"}]}`), nil
			}
			return []byte(`{"complete":true,"messages":[{"conversationId":"cid-newer"}]}`), nil
		case "+chat-bots":
			return []byte(`{"bots":[{"name":"Bot"}]}`), nil
		default:
			t.Fatalf("unexpected DWS call: %v", args)
			return nil, nil
		}
	}}
	result, err := a.DiscoverGroupConversations(context.Background(), channel.Config{Identity: core.ChannelIdentity{DeliveryRobotName: "Bot"}})
	if err != nil || !result.Complete || searches != 3 || len(result.Groups) != 2 {
		t.Fatalf("older positive group not recovered: result=%+v searches=%d err=%v", result, searches, err)
	}
}

func TestPageLimitedActivitySearchHasFixedBudget(t *testing.T) {
	searches := 0
	a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		switch args[1] {
		case "+chat-list-all":
			return []byte(`{"complete":true,"groups":[{"openConversationId":"cid-observed"}]}`), nil
		case "+search-msg":
			searches++
			limit := ""
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--page-limit" {
					limit = args[i+1]
				}
			}
			if searches == 1 && limit != "10" || searches > 1 && limit != "2" {
				t.Fatalf("unexpected discovery page limit on search %d: %q", searches, limit)
			}
			return []byte(`{"complete":false,"hasMore":true,"failedCount":1,"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 10."}],"messages":[{"conversationId":"cid-observed"}]}`), nil
		case "+chat-bots":
			return []byte(`{"bots":[{"name":"Bot"}]}`), nil
		default:
			t.Fatalf("unexpected DWS call: %v", args)
			return nil, nil
		}
	}}
	result, err := a.DiscoverGroupConversations(context.Background(), channel.Config{Identity: core.ChannelIdentity{DeliveryRobotName: "Bot"}})
	if err != nil || result.Complete || searches > 15 || len(result.Groups) != 1 {
		t.Fatalf("unbounded partial discovery: result=%+v searches=%d err=%v", result, searches, err)
	}
}

func TestActiveGroupWindowTimeoutRetainsEarlierPositiveEvidence(t *testing.T) {
	searches := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &Adapter{Run: func(ctx context.Context, args ...string) ([]byte, error) {
		searches++
		if searches == 1 {
			return []byte(`{"complete":false,"hasMore":true,"failedCount":1,"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 10."}],"messages":[{"conversationId":"cid-positive"}]}`), nil
		}
		cancel()
		return nil, ctx.Err()
	}}
	end := time.Now().UTC().Truncate(time.Second)
	ids, complete, err := a.discoverActiveGroupIDs(ctx, channel.Config{}, end.Add(-30*24*time.Hour), end)
	if err != nil || complete || !ids["cid-positive"] || searches > 2 {
		t.Fatalf("timed out refinement discarded verified observation: ids=%v complete=%v searches=%d err=%v", ids, complete, searches, err)
	}
}

func TestRefinementFailureRetainsOnlyIndependentPositiveEvidence(t *testing.T) {
	searches := 0
	a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		searches++
		if searches == 1 {
			return []byte(`{"complete":false,"hasMore":true,"failedCount":1,"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 10."}],"messages":[{"conversationId":"cid-proved"}]}`), nil
		}
		return []byte(`{"complete":false,"hasMore":true,"failedCount":1,"failures":[{"stage":"search-page","error":"continuation failed"}],"messages":[{"conversationId":"cid-unverified"}]}`), nil
	}}
	end := time.Now().UTC().Truncate(time.Second)
	ids, complete, err := a.discoverActiveGroupIDs(context.Background(), channel.Config{}, end.Add(-30*24*time.Hour), end)
	if err != nil || complete || !ids["cid-proved"] || ids["cid-unverified"] || searches != 2 {
		t.Fatalf("failed child search tainted a prior positive: ids=%v complete=%v searches=%d err=%v", ids, complete, searches, err)
	}
}

func TestInitialContinuationFailureUsesSafeSinglePageRetry(t *testing.T) {
	searches := 0
	a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		searches++
		if searches == 1 {
			return []byte(`{"complete":false,"hasMore":true,"failedCount":1,"failures":[{"stage":"search-page","error":"continuation failed"}],"messages":[{"conversationId":"cid-unverified"}]}`), nil
		}
		if searches == 2 {
			limit := ""
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--page-limit" {
					limit = args[i+1]
				}
			}
			if limit != "1" {
				t.Fatalf("unsafe fallback page limit: %v", args)
			}
			return []byte(`{"complete":false,"hasMore":true,"failedCount":1,"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 1."}],"messages":[{"conversationId":"cid-proved"}]}`), nil
		}
		return []byte(`{"complete":true,"messages":[]}`), nil
	}}
	end := time.Now().UTC().Truncate(time.Second)
	ids, complete, err := a.discoverActiveGroupIDs(context.Background(), channel.Config{}, end.Add(-30*24*time.Hour), end)
	if err != nil || !ids["cid-proved"] || ids["cid-unverified"] || searches < 2 {
		t.Fatalf("single-page proof missing or failed page accepted: ids=%v complete=%v searches=%d err=%v", ids, complete, searches, err)
	}
}

func TestSearchFailureLedgerRejectsUnknownAndIncompleteEvidence(t *testing.T) {
	for name, ledger := range map[string]string{
		"auth":                  `"failedCount":1,"failures":[{"stage":"search-page","code":"auth_required"}]`,
		"unknown":               `"failedCount":1,"failures":[{"stage":"unknown"}]`,
		"mixed":                 `"failedCount":2,"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 10."},{"stage":"search-page","code":"permission_denied"}]`,
		"count mismatch":        `"failedCount":2,"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 10."}]`,
		"missing count":         `"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 10."}]`,
		"missing ledger":        `"failedCount":1`,
		"missing error":         `"failedCount":1,"failures":[{"stage":"search-page-limit"}]`,
		"structured error":      `"failedCount":1,"failures":[{"stage":"search-page-limit","error":{"code":"auth_required"}}]`,
		"additional error code": `"failedCount":1,"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 10.","code":"auth_required"}]`,
		"control in error":      `"failedCount":1,"failures":[{"stage":"search-page-limit","error":"page-limit\n"}]`,
		"missing stage":         `"failedCount":1,"failures":[{}]`,
	} {
		t.Run(name, func(t *testing.T) {
			a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
				switch args[1] {
				case "+chat-list-all":
					return []byte(`{"complete":true,"groups":[{"openConversationId":"cid-seen"}]}`), nil
				case "+search-msg":
					return []byte(`{"complete":false,"hasMore":true,"messages":[{"conversationId":"cid-seen"}],` + ledger + `}`), nil
				default:
					t.Fatal("unsafe evidence reached bot verification")
					return nil, nil
				}
			}}
			if _, err := a.DiscoverGroupConversations(context.Background(), channel.Config{Identity: core.ChannelIdentity{DeliveryRobotName: "Bot"}}); core.ErrorCode(err) != "unavailable" {
				t.Fatalf("untrusted failure ledger accepted: %v", err)
			}
		})
	}
}

func TestRuntimeSetupUsesPartialVerifiedGroupsAndPreservesUnseenIgnore(t *testing.T) {
	botSelected := false
	adapter := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] + " " + args[1] {
		case "profile list":
			return []byte(`{"currentProfile":"corp","profiles":[{"profile":"corp","corpId":"corp"}]}`), nil
		case "contact +me":
			return []byte(`{"data":{"userId":"owner"}}`), nil
		case "chat +bot-search":
			botSelected = true
			return []byte(`{"robots":[]}`), nil
		case "chat +chat-list-all":
			if !botSelected {
				t.Fatal("group discovery ran before target robot selection")
			}
			return []byte(`{"complete":true,"groups":[{"name":"Work","openConversationId":"cid-work"},{"name":"Noise","openConversationId":"cid-noise"},{"name":"Unknown","openConversationId":"cid-unknown"}]}`), nil
		case "chat +search-msg":
			return []byte(`{"complete":false,"hasMore":true,"count":500,"failedCount":1,"failures":[{"stage":"search-page-limit","error":"Reached page-limit of 10."}],"messages":[{"conversationId":"cid-work"}]}`), nil
		case "chat +chat-bots":
			if args[3] != "cid-work" {
				t.Fatal("unobserved group reached robot verification")
			}
			return []byte(`{"bots":[{"name":"Enterprise Bot"}]}`), nil
		default:
			t.Fatalf("unexpected provider operation: %v", args)
			return nil, nil
		}
	}}
	got, err := adapter.DiscoverRuntimeSetup(context.Background(), RuntimeSetupRequest{RobotCode: "enterprise-app", RobotName: "Enterprise Bot", Ignore: []string{"Noise"}})
	if err != nil || got.GroupDiscoveryComplete || len(got.Conversations) != 2 || got.Conversations[0].ID != "cid-work" || got.Conversations[0].Ignored || got.Conversations[1].ID != "cid-noise" || !got.Conversations[1].Ignored {
		t.Fatalf("partial setup scope/ignore lost: %+v err=%v", got, err)
	}
}

func TestRuntimeSetupRejectsUnprovenAutomaticScope(t *testing.T) {
	for name, bots := range map[string]string{"no matching robot": `{"bots":[]}`, "ambiguous group robot": `{"bots":[{"name":"Bot"},{"name":"Bot"}]}`} {
		t.Run(name, func(t *testing.T) {
			adapter := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
				switch args[0] + " " + args[1] {
				case "profile list":
					return []byte(`{"currentProfile":"corp","profiles":[{"profile":"corp","corpId":"corp"}]}`), nil
				case "contact +me":
					return []byte(`{"data":{"userId":"owner"}}`), nil
				case "chat +bot-search":
					return []byte(`{"robots":[{"robotCode":"bot","robotName":"Bot"}]}`), nil
				case "chat +chat-list-all":
					return []byte(`{"complete":true,"groups":[{"openConversationId":"cid-group"}]}`), nil
				case "chat +search-msg":
					return []byte(`{"complete":false,"hasMore":true,"messages":[{"conversationId":"cid-group"}]}`), nil
				case "chat +chat-bots":
					return []byte(bots), nil
				default:
					t.Fatal(args)
					return nil, nil
				}
			}}
			if _, err := adapter.DiscoverRuntimeSetup(context.Background(), RuntimeSetupRequest{RobotCode: "bot"}); core.ErrorCode(err) != "unavailable" {
				t.Fatalf("unproven automatic scope accepted: %v", err)
			}
		})
	}
}

func TestRuntimeSetupNoOwnedRobotExplainsEnterpriseSelection(t *testing.T) {
	adapter := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] + " " + args[1] {
		case "profile list":
			return []byte(`{"currentProfile":"corp","profiles":[{"profile":"corp","corpId":"corp"}]}`), nil
		case "contact +me":
			return []byte(`{"data":{"userId":"owner"}}`), nil
		case "chat +bot-search":
			return []byte(`{"robots":[]}`), nil
		default:
			t.Fatal("group discovery must wait for a selected robot")
			return nil, nil
		}
	}}
	_, err := adapter.DiscoverRuntimeSetup(context.Background(), RuntimeSetupRequest{RobotName: "Enterprise Bot"})
	if core.ErrorCode(err) != "conflict" || !strings.Contains(err.Error(), "--robot-code") || !strings.Contains(err.Error(), "--robot-name") {
		t.Fatalf("missing actionable enterprise robot selection: %v", err)
	}
}

func TestRuntimeSetupWithoutRobotNeverQueriesRobotOrSendCapabilities(t *testing.T) {
	adapter := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] + " " + args[1] {
		case "profile list":
			return []byte(`{"currentProfile":"corp:owner","profiles":[{"profile":"corp:owner","corpId":"corp","userId":"owner"}]}`), nil
		case "contact +me":
			return []byte(`{"data":{"userId":"owner"}}`), nil
		case "chat +chat-list-all":
			return []byte(`{"complete":true,"groups":[{"openConversationId":"cid-active"},{"openConversationId":"cid-idle"}]}`), nil
		case "chat +search-msg":
			return []byte(`{"complete":true,"messages":[{"conversationId":"cid-active"}]}`), nil
		default:
			t.Fatalf("observation-only setup invoked a robot/send path: %v", args)
			return nil, nil
		}
	}}
	out, err := adapter.DiscoverRuntimeSetup(context.Background(), RuntimeSetupRequest{})
	if err != nil || out.RobotCode != "" || out.RobotName != "" || out.OwnerUserID != "owner" || len(out.Conversations) != 1 || out.Conversations[0].ID != "cid-active" {
		t.Fatalf("independent observation: %+v %v", out, err)
	}
}

func TestRuntimeSetupRejectsProfileCurrentUserMismatch(t *testing.T) {
	adapter := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] + " " + args[1] {
		case "profile list":
			return []byte(`{"currentProfile":"corp:owner","profiles":[{"profile":"corp:owner","corpId":"corp","userId":"owner"}]}`), nil
		case "contact +me":
			return []byte(`{"data":{"userId":"someone-else"}}`), nil
		default:
			t.Fatalf("mismatched identity reached discovery: %v", args)
			return nil, nil
		}
	}}
	if _, err := adapter.DiscoverRuntimeSetup(context.Background(), RuntimeSetupRequest{}); core.ErrorCode(err) != "denied" {
		t.Fatalf("identity mismatch accepted: %v", err)
	}
}
