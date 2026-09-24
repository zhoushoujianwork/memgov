package dws

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// ResolveBotUser reads the bound tenant's directory. It does not send as the
// DWS owner; the application bot remains the only outbound identity.
func (a *Adapter) ResolveBotUser(ctx context.Context, cfg channel.Config, name string) (channel.BotUser, error) {
	name = strings.TrimSpace(name)
	if cfg.Identity.Profile == "" || name == "" || len([]rune(name)) > 100 {
		return channel.BotUser{}, core.Fail("invalid_input", "bot recipient lookup requires a bound profile and a name")
	}
	raw, err := a.run(ctx, withProfile(cfg, []string{"contact", "+lookup", "--name", name})...)
	if err != nil {
		return channel.BotUser{}, err
	}
	type botProfile struct {
		UserID      string `json:"userId"`
		OrgUserID   string `json:"orgUserId"`
		OrgUserName string `json:"orgUserName"`
	}
	var result struct {
		Profile botProfile `json:"profile"`
		Data    struct {
			Profile botProfile `json:"profile"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return channel.BotUser{}, core.Fail("unavailable", "DWS returned an unreadable recipient lookup")
	}
	profile := result.Profile
	if profile.UserID == "" && profile.OrgUserID == "" {
		profile = result.Data.Profile
	} else if result.Data.Profile.UserID != "" || result.Data.Profile.OrgUserID != "" {
		return channel.BotUser{}, core.Fail("denied", "DWS returned conflicting recipient profiles")
	}
	id := profile.UserID
	if id == "" {
		id = profile.OrgUserID
	}
	if id == "" || profile.UserID != "" && profile.OrgUserID != "" && profile.UserID != profile.OrgUserID {
		return channel.BotUser{}, core.Fail("denied", "DWS did not resolve one stable user ID")
	}
	return channel.BotUser{ID: id, Name: profile.OrgUserName}, nil
}

// ListBotGroups reads directory names for later intersection with this bot's
// active routes. A partial directory cannot prove that a group is absent.
func (a *Adapter) ListBotGroups(ctx context.Context, cfg channel.Config) ([]channel.GroupConversation, error) {
	if cfg.Identity.Profile == "" {
		return nil, core.Fail("denied", "bot group lookup requires a bound profile")
	}
	raw, err := a.run(ctx, withProfile(cfg, []string{"chat", "+chat-list-all", "--limit", "200", "--page-all", "--page-limit", "50"})...)
	if err != nil {
		return nil, err
	}
	var result struct {
		Groups []struct {
			ID   string `json:"openConversationId"`
			Name string `json:"name"`
		} `json:"groups"`
		Complete    bool `json:"complete"`
		HasMore     bool `json:"hasMore"`
		Partial     bool `json:"partial"`
		FailedCount int  `json:"failedCount"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.Complete || result.HasMore || result.Partial || result.FailedCount != 0 {
		return nil, core.Fail("unavailable", "DWS did not return a complete group directory")
	}
	groups := make([]channel.GroupConversation, 0, len(result.Groups))
	for _, group := range result.Groups {
		if group.ID != "" {
			groups = append(groups, channel.GroupConversation{ID: group.ID, Name: group.Name})
		}
	}
	return groups, nil
}
