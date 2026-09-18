package dws

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// RuntimeSetupRequest contains the few choices that cannot be inferred from
// the current dws account. Watch may be either a group name or its stable ID.
type RuntimeSetupRequest struct {
	Profile                string
	Watch                  string
	Ignore                 []string
	RobotCode              string
	RobotName              string
	DeliveryConversationID string
}

type RuntimeSetupConversation struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Ignored bool   `json:"ignored"`
}

type RuntimeSetupResult struct {
	GroupDiscoveryComplete bool                       `json:"group_discovery_complete"`
	Profile                string                     `json:"profile"`
	CorpID                 string                     `json:"corp_id"`
	OwnerUserID            string                     `json:"owner_user_id"`
	OwnerName              string                     `json:"owner_name,omitempty"`
	ConversationID         string                     `json:"conversation_id"`
	Conversation           string                     `json:"conversation,omitempty"`
	RobotCode              string                     `json:"robot_code"`
	RobotName              string                     `json:"robot_name,omitempty"`
	DeliveryID             string                     `json:"delivery_conversation_id"`
	Conversations          []RuntimeSetupConversation `json:"conversations"`
}

// DiscoverRuntimeSetup resolves the active profile, current user and watched
// conversations through read-only dws calls. Robot filters are opt-in. Ambiguous
// choices are rejected rather than guessed.
func (a *Adapter) DiscoverRuntimeSetup(ctx context.Context, in RuntimeSetupRequest) (RuntimeSetupResult, error) {
	var out RuntimeSetupResult
	in.Watch = strings.TrimSpace(in.Watch)

	raw, err := a.run(ctx, "profile", "list")
	if err != nil {
		return out, err
	}
	var profiles struct {
		Current  string `json:"currentProfile"`
		Profiles []struct {
			Profile string `json:"profile"`
			CorpID  string `json:"corpId"`
			UserID  string `json:"userId"`
		} `json:"profiles"`
	}
	if json.Unmarshal(raw, &profiles) != nil {
		return out, core.Fail("unavailable", "dws profile list was unreadable")
	}
	want := strings.TrimSpace(in.Profile)
	if want == "" {
		want = profiles.Current
	}
	matches := 0
	for _, p := range profiles.Profiles {
		if p.Profile == want {
			matches++
			out.Profile, out.CorpID, out.OwnerUserID = p.Profile, p.CorpID, p.UserID
		}
	}
	if matches != 1 || out.Profile == "" || out.CorpID == "" {
		return out, core.Fail("invalid_input", "could not select a logged-in dws profile; pass --profile from `dws profile list`")
	}

	meRaw, err := a.run(ctx, withProfile(channelConfig(out.Profile), []string{"contact", "+me"})...)
	if err != nil {
		return out, err
	}
	var me struct {
		Data struct {
			UserID string `json:"userId"`
			Name   string `json:"name"`
		} `json:"data"`
		UserID string `json:"userId"`
		Name   string `json:"name"`
	}
	if json.Unmarshal(meRaw, &me) != nil {
		return out, core.Fail("unavailable", "dws current-user result was unreadable")
	}
	profileOwnerID := out.OwnerUserID
	if me.Data.UserID != "" {
		out.OwnerUserID, out.OwnerName = me.Data.UserID, me.Data.Name
	} else {
		out.OwnerUserID, out.OwnerName = me.UserID, me.Name
	}
	if out.OwnerUserID == "" {
		return out, core.Fail("unavailable", "dws did not return the current user's stable userId")
	}
	if profileOwnerID != "" && profileOwnerID != out.OwnerUserID {
		return out, core.Fail("denied", "selected DWS profile userId does not match the authenticated current user")
	}

	out.RobotCode = strings.TrimSpace(in.RobotCode)
	out.RobotName = strings.TrimSpace(in.RobotName)
	robotFilter := out.RobotCode != "" || out.RobotName != ""
	if robotFilter {
		type robot struct {
			Code      string `json:"robotCode"`
			Name      string `json:"robotName"`
			AltName   string `json:"name"`
			OpenID    string `json:"openDingTalkId"`
			AltOpenID string `json:"openDingtalkId"`
		}
		botRaw, callErr := a.run(ctx, withProfile(channelConfig(out.Profile), []string{"chat", "+bot-search", "--page", "1", "--size", "100"})...)
		if callErr != nil {
			return out, callErr
		}
		var owned struct {
			Robots []robot `json:"robots"`
		}
		if json.Unmarshal(botRaw, &owned) != nil {
			return out, core.Fail("unavailable", "dws robot search result was unreadable")
		}
		selected := []robot{}
		for _, bot := range owned.Robots {
			if bot.Code != "" && (out.RobotCode == "" || bot.Code == out.RobotCode) && (out.RobotName == "" || firstSetupValue(bot.Name, bot.AltName) == out.RobotName) {
				selected = append(selected, bot)
			}
		}
		if out.RobotCode == "" {
			if len(selected) != 1 {
				return out, core.Fail("conflict", "found %d delivery robots; pass --robot-code and --robot-name to select an enterprise application robot", len(selected))
			}
			out.RobotCode = selected[0].Code
		} else if len(selected) > 1 {
			return out, core.Fail("conflict", "the selected robot code matched more than one dws robot")
		}
		if out.RobotName == "" && len(selected) == 1 {
			out.RobotName = firstSetupValue(selected[0].Name, selected[0].AltName)
		}
	}
	if in.Watch == "" {
		if robotFilter && out.RobotName == "" {
			return out, core.Fail("invalid_input", "automatic group takeover needs the delivery robot name; pass --robot-code and --robot-name for an enterprise application robot")
		}
		cfg := channelConfig(out.Profile)
		cfg.Identity.DeliveryRobotName = out.RobotName
		observation, callErr := a.discoverGroupConversations(ctx, cfg, func(directory []channel.GroupConversation) {
			// Retain deny rules from the complete directory even when a capped
			// activity search omits this group. These routes can only be ignored.
			for _, group := range directory {
				for _, ignore := range in.Ignore {
					ignore = strings.TrimSpace(ignore)
					if ignore != "" && (ignore == group.ID || ignore == group.Name) {
						out.Conversations = append(out.Conversations, RuntimeSetupConversation{ID: group.ID, Name: group.Name, Ignored: true})
						break
					}
				}
			}
		})
		if callErr != nil {
			return out, callErr
		}
		out.GroupDiscoveryComplete = observation.Complete
		ignored := out.Conversations
		out.Conversations = nil
		seen := map[string]bool{}
		for _, group := range observation.Groups {
			seen[group.ID] = true
			out.Conversations = append(out.Conversations, RuntimeSetupConversation{ID: group.ID, Name: group.Name})
		}
		for _, group := range ignored {
			if !seen[group.ID] {
				out.Conversations = append(out.Conversations, group)
			}
		}
		if len(observation.Groups) == 0 {
			return out, core.Fail("not_found", "no group has verified recent activity under the selected scope; discovery may be incomplete, retry or check any explicit robot filter")
		}
	} else if strings.HasPrefix(in.Watch, "cid") && !strings.ContainsAny(in.Watch, " \t\r\n") {
		out.ConversationID = in.Watch
		out.Conversations = []RuntimeSetupConversation{{ID: in.Watch}}
	} else {
		chatRaw, callErr := a.run(ctx, withProfile(channelConfig(out.Profile), []string{"chat", "+chat-search", "--query", in.Watch, "--page-all", "--page-limit", "20"})...)
		if callErr != nil {
			return out, callErr
		}
		var found struct {
			Chats []struct {
				ID    string `json:"openConversationId"`
				Name  string `json:"name"`
				Title string `json:"title"`
			} `json:"chats"`
		}
		if json.Unmarshal(chatRaw, &found) != nil {
			return out, core.Fail("unavailable", "dws group search result was unreadable")
		}
		matches := found.Chats
		exact := matches[:0]
		for _, candidate := range matches {
			if candidate.Name == in.Watch || candidate.Title == in.Watch {
				exact = append(exact, candidate)
			}
		}
		if len(exact) > 0 {
			matches = exact
		}
		if len(matches) != 1 {
			return out, core.Fail("conflict", "group search for %q returned %d candidates; use the exact group name or openConversationId", in.Watch, len(matches))
		}
		out.ConversationID = matches[0].ID
		out.Conversation = firstSetupValue(matches[0].Name, matches[0].Title)
		out.Conversations = []RuntimeSetupConversation{{ID: out.ConversationID, Name: out.Conversation}}
	}
	ignored := map[string]bool{}
	for _, value := range in.Ignore {
		value = strings.TrimSpace(value)
		if value != "" {
			ignored[value] = false
		}
	}
	for i := range out.Conversations {
		for _, key := range []string{out.Conversations[i].ID, out.Conversations[i].Name} {
			if _, ok := ignored[key]; ok {
				out.Conversations[i].Ignored = true
				ignored[key] = true
			}
		}
	}
	for value, matched := range ignored {
		if !matched {
			return out, core.Fail("not_found", "ignored group %q was not found in the selected group scope", value)
		}
	}

	out.DeliveryID = strings.TrimSpace(in.DeliveryConversationID)
	if out.DeliveryID == "" {
		// Bot direct messages are addressed by staff user ID in dws. Keeping that
		// address in the notify route also avoids an extra bot lookup and lets an
		// explicitly configured enterprise app robot work even when +bot-search
		// only lists robots created by the current user.
		out.DeliveryID = out.OwnerUserID
	}
	return out, nil
}

// Legacy callers can only replace a scope from a complete observation.
func (a *Adapter) ListGroupConversations(ctx context.Context, cfg channel.Config) ([]channel.GroupConversation, error) {
	observation, err := a.DiscoverGroupConversations(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if !observation.Complete {
		return nil, core.Fail("unavailable", "DWS group discovery is partial; use verified positive observations without replacing the full scope")
	}
	return observation.Groups, nil
}

// DiscoverGroupConversations yields per-group positive evidence independently
// of global search completeness. Missing groups in capped results prove nothing.
func (a *Adapter) DiscoverGroupConversations(ctx context.Context, cfg channel.Config) (channel.GroupDiscovery, error) {
	return a.discoverGroupConversations(ctx, cfg, nil)
}

// observeDirectory is used only to preserve explicit deny rules during setup.
func (a *Adapter) discoverGroupConversations(ctx context.Context, cfg channel.Config, observeDirectory func([]channel.GroupConversation)) (channel.GroupDiscovery, error) {
	out := channel.GroupDiscovery{Groups: []channel.GroupConversation{}}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	raw, err := a.run(ctx, withProfile(cfg, []string{"chat", "+chat-list-all", "--limit", "200", "--page-all", "--page-limit", "50"})...)
	if err != nil {
		return out, err
	}
	var listed struct {
		Groups []struct {
			ID   string `json:"openConversationId"`
			Name string `json:"name"`
		} `json:"groups"`
		Complete    bool              `json:"complete"`
		HasMore     bool              `json:"hasMore"`
		Partial     bool              `json:"partial"`
		FailedCount int               `json:"failedCount"`
		Failures    []json.RawMessage `json:"failures"`
	}
	if json.Unmarshal(raw, &listed) != nil || !listed.Complete || listed.HasMore || listed.Partial || listed.FailedCount > 0 || len(listed.Failures) > 0 {
		return out, core.Fail("unavailable", "DWS did not return a complete group list")
	}
	if observeDirectory != nil {
		directory := make([]channel.GroupConversation, 0, len(listed.Groups))
		for _, group := range listed.Groups {
			if group.ID != "" {
				directory = append(directory, channel.GroupConversation{ID: group.ID, Name: group.Name})
			}
		}
		observeDirectory(directory)
	}
	end := time.Now().UTC().Truncate(time.Second)
	active, complete, err := a.discoverActiveGroupIDs(ctx, cfg, end.Add(-30*24*time.Hour), end)
	if err != nil {
		return out, err
	}
	out.Complete = complete
	// Global capped search still proves each returned group's positive activity.
	// Only these groups need robot reads; 1519 listed groups do not cause 1519 calls.
	checks := 0
	for _, g := range listed.Groups {
		if !active[g.ID] {
			continue
		}
		if cfg.Identity.DeliveryRobotName == "" {
			out.Groups = append(out.Groups, channel.GroupConversation{ID: g.ID, Name: g.Name})
			continue
		}
		if checks >= 64 || ctx.Err() != nil {
			out.Complete = false
			break
		}
		checks++
		group := channel.GroupConversation{ID: g.ID, Name: g.Name}
		verified, e := a.groupsWithRobot(ctx, cfg, []channel.GroupConversation{group}, cfg.Identity.DeliveryRobotName)
		if e != nil {
			out.Complete = false
			continue
		}
		if len(verified) == 1 {
			out.Groups = append(out.Groups, group)
		} else {
			out.Excluded = append(out.Excluded, g.ID)
		}
	}
	if !out.Complete && len(out.Groups) == 0 {
		return out, core.Fail("unavailable", "partial group discovery has no newly verified positive group")
	}
	return out, nil
}

// A capped global search proves activity only for the messages it returned.
// Splitting its fixed 30-day interval lets older active groups be observed
// without treating missing groups in any capped page as inactive. The bounded
// breadth-first pass covers the whole interval before refining busy periods.
func (a *Adapter) discoverActiveGroupIDs(ctx context.Context, cfg channel.Config, start, end time.Time) (map[string]bool, bool, error) {
	type window struct{ start, end time.Time }
	queue := []window{{start, end}}
	active := map[string]bool{}
	complete := true
	const maxSearches = 15
	searches := 0
	for len(queue) > 0 {
		deadline, hasDeadline := ctx.Deadline()
		if searches >= maxSearches || ctx.Err() != nil || searches > 0 && hasDeadline && time.Until(deadline) < 45*time.Second {
			complete = false
			break
		}
		current := queue[0]
		queue = queue[1:]
		searches++
		pageLimit := "2"
		if searches == 1 {
			pageLimit = "10"
		}
		searchCtx := ctx
		cancel := func() {}
		if searches > 1 {
			searchCtx, cancel = context.WithTimeout(ctx, 15*time.Second)
		}
		ids, covered, pageLimited, err := a.searchActiveGroupWindow(searchCtx, cfg, current.start, current.end, pageLimit)
		cancel()
		if err != nil && searches == 1 {
			// A single-page retry can still prove positive recent activity when
			// a later page in the larger initial scan failed. Never use the
			// failed scan's messages or treat its omissions as complete.
			ids, covered, pageLimited, err = a.searchActiveGroupWindow(ctx, cfg, current.start, current.end, "1")
		}
		if err != nil {
			if searches > 1 && len(active) > 0 {
				return active, false, nil
			}
			return nil, false, err
		}
		for id := range ids {
			active[id] = true
		}
		if covered {
			continue
		}
		mid := current.start.Add(current.end.Sub(current.start) / 2).Truncate(time.Second)
		if pageLimited && mid.After(current.start) && mid.Before(current.end) && searches+len(queue)+2 <= maxSearches {
			queue = append(queue, window{current.start, mid}, window{mid, current.end})
			continue
		}
		complete = false
	}
	return active, complete && len(queue) == 0, nil
}

func (a *Adapter) searchActiveGroupWindow(ctx context.Context, cfg channel.Config, start, end time.Time, pageLimit string) (map[string]bool, bool, bool, error) {
	raw, err := a.run(ctx, withProfile(cfg, []string{"chat", "+search-msg", "--conversation-type", "group", "--start", start.Format(time.RFC3339), "--end", end.Format(time.RFC3339), "--no-enrich", "--limit", "100", "--page-all", "--page-limit", pageLimit})...)
	if err != nil {
		return nil, false, false, err
	}
	var activity struct {
		Messages []struct {
			ConversationID string `json:"conversationId"`
		} `json:"messages"`
		Complete               *bool             `json:"complete"`
		HasMore                bool              `json:"hasMore"`
		Partial                bool              `json:"partial"`
		Truncated              bool              `json:"truncated"`
		TruncatedByPageLimit   bool              `json:"truncatedByPageLimit"`
		TruncatedByResultLimit bool              `json:"truncatedByResultLimit"`
		NextPage               string            `json:"nextPage"`
		StopReason             string            `json:"stopReason"`
		FailedCount            int               `json:"failedCount"`
		Failures               []json.RawMessage `json:"failures"`
	}
	if json.Unmarshal(raw, &activity) != nil || activity.Complete == nil {
		return nil, false, false, core.Fail("unavailable", "active-group search lacks reliable evidence")
	}
	pageLimited := activity.FailedCount != 0 || len(activity.Failures) > 0
	if pageLimited && !onlySearchPageLimitFailures(activity.FailedCount, activity.Failures) {
		return nil, false, false, core.Fail("unavailable", "active-group search contains unverified failures")
	}
	covered := !pageLimited && *activity.Complete && !activity.HasMore && !activity.Partial && !activity.Truncated && !activity.TruncatedByPageLimit && !activity.TruncatedByResultLimit && activity.NextPage == "" && (activity.StopReason == "" || activity.StopReason == "source_complete")
	ids := map[string]bool{}
	for _, message := range activity.Messages {
		if message.ConversationID == "" {
			return nil, false, false, core.Fail("unavailable", "active-group message lacks a conversation identity")
		}
		ids[message.ConversationID] = true
	}
	return ids, covered, pageLimited, nil
}

// DWS records its bounded search pagination as a failure ledger entry. This
// exact classification permits positive observations, never proof of absence.
// Unknown failures and incomplete ledgers still invalidate the whole lookup.
func onlySearchPageLimitFailures(count int, failures []json.RawMessage) bool {
	if count <= 0 || count != len(failures) {
		return false
	}
	for _, raw := range failures {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || len(fields) != 2 {
			return false
		}
		var stage, detail string
		if json.Unmarshal(fields["stage"], &stage) != nil || stage != "search-page-limit" || json.Unmarshal(fields["error"], &detail) != nil || len(detail) == 0 || len(detail) > 256 {
			return false
		}
		// Inspect only the bounded classification shape. Provider text is never
		// copied into logs or used as an authorization instruction.
		for _, r := range detail {
			if r < 32 || r == 127 {
				return false
			}
		}
	}
	return true
}

func (a *Adapter) groupsWithRobot(ctx context.Context, cfg channel.Config, groups []channel.GroupConversation, robotName string) ([]channel.GroupConversation, error) {
	robotName = strings.TrimSpace(robotName)
	if robotName == "" {
		return groups, nil
	}
	out := make([]channel.GroupConversation, 0, len(groups))
	for _, group := range groups {
		raw, err := a.run(ctx, withProfile(cfg, []string{"chat", "+chat-bots", "--group", group.ID})...)
		if err != nil {
			return nil, err
		}
		var listed struct {
			Bots *[]struct {
				Name string `json:"name"`
			} `json:"bots"`
			Complete    *bool             `json:"complete"`
			HasMore     bool              `json:"hasMore"`
			Partial     bool              `json:"partial"`
			FailedCount int               `json:"failedCount"`
			Failures    []json.RawMessage `json:"failures"`
		}
		if json.Unmarshal(raw, &listed) != nil || listed.Bots == nil || (listed.Complete != nil && !*listed.Complete) || listed.HasMore || listed.Partial || listed.FailedCount > 0 || len(listed.Failures) > 0 {
			return nil, core.Fail("unavailable", "dws group bot list was unreadable")
		}
		matches := 0
		for _, bot := range *listed.Bots {
			if strings.TrimSpace(bot.Name) == robotName {
				matches++
			}
		}
		if matches > 1 {
			return nil, core.Fail("denied", "robot membership name is ambiguous; explicit identity mapping is required")
		}
		if matches == 1 {
			out = append(out, group)
		}
	}
	return out, nil
}

func channelConfig(profile string) channel.Config {
	return channel.Config{Identity: core.ChannelIdentity{Profile: profile}}
}

func firstSetupValue(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func findSetupString(raw []byte, key string) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(current any) string {
		switch item := current.(type) {
		case map[string]any:
			if found, ok := item[key].(string); ok && found != "" {
				return found
			}
			for _, child := range item {
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range item {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(value)
}
