package cli

import (
	"context"
	"sort"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type PlannedGroupMount struct {
	ChannelID      string `json:"channel_id"`
	SourceRouteID  string `json:"source_route_id"`
	ConversationID string `json:"conversation_id"`
	WorkspaceID    string `json:"workspace_id"`
}

// A normal existing assistant route remains an explicit local authorization.
// Missing routes can only inherit authorization from a recent complete source
// discovery bound to the same robot, channel and source configuration versions.
func planMissingGroupMountsFor(ctx context.Context, p *DualConfigPlan, s dualPlanState, binding groupApplicationBinding, issue func(bool, string, string, string, string)) dualPlanState {
	if p.GroupMounts == nil {
		p.GroupMounts = []PlannedGroupMount{}
	}
	g := binding.App
	if g == nil || !*g.Enabled {
		return s
	}
	declaration := p.Declaration.DataSources[g.Source]
	source, err := core.ReadDataSource(ctx, s.Q, g.Source)
	if err != nil {
		return s
	} // Existing reference validation explains missing sources.
	dws, ok := s.Channels[declaration.Channel]
	if !ok {
		return s
	}
	app, ok := s.Channels[g.Channel]
	if !ok {
		return s
	}
	if app.Kind != core.ChannelDingTalkApp || dws.Kind != core.ChannelDwsPersonal || app.Tenant != dws.Tenant || app.Identity.RobotCode == "" || declaration.Groups.MemberRobot != g.Channel {
		return s
	}
	receipt, err := core.ReadSourceGroupDiscovery(ctx, s.Q, source.ID)
	if err != nil {
		issue(false, "group_discovery_required", "application", binding.Name, "Automatic group mounting requires a complete DWS source discovery; preconfigured routes do not prove robot membership.")
		return s
	}
	p.Preconditions = append(p.Preconditions, PlanPrecondition{Kind: "source_group_discovery", Name: g.Source, ObjectID: source.ID, Version: receipt.SourceVersion, Status: "observed", Snapshot: core.Digest(receipt)})
	observed, err := time.Parse(time.RFC3339Nano, receipt.ObservedAt)
	lifetime := time.Duration(source.ReconcileSeconds*2) * time.Second
	if lifetime < 10*time.Minute {
		lifetime = 10 * time.Minute
	}
	if lifetime > time.Hour {
		lifetime = time.Hour
	}
	if !receipt.Valid || err != nil || time.Since(observed) < 0 || time.Since(observed) > lifetime || receipt.SourceVersion != source.Version || receipt.ChannelVersion != dws.ConfigVersion || receipt.RobotCode != app.Identity.RobotCode || receipt.RobotCode != source.MemberRobotCode || receipt.RobotName != dws.Identity.DeliveryRobotName || source.ChannelID != dws.ID {
		issue(false, "group_discovery_stale", "application", binding.Name, "Refresh DWS source discovery before automatically mounting groups; its identity, version or freshness no longer matches.")
		return s
	}
	ignored := map[string]bool{}
	for _, v := range source.Ignore {
		ignored[v] = true
	}
	for _, v := range declaration.Groups.Ignore {
		ignored[v] = true
	}
	// Also hide ignored routes from ordinary reference validation and execution.
	for _, group := range receipt.Groups {
		if ignored[group.ID] || ignored[group.Name] {
			delete(s.Routes[dws.ID], group.ID)
		}
	}
	for _, group := range receipt.Groups {
		if ignored[group.ID] || ignored[group.Name] {
			continue
		}
		sourceRoute, ok := s.Routes[dws.ID][group.ID]
		if !ok || sourceRoute.ConversationType != "group" || sourceRoute.Status != "active" || sourceRoute.Mode == "ignore" {
			continue
		}
		if existing, exists := s.Routes[app.ID][group.ID]; exists {
			if existing.Mode == "ignore" {
				continue
			}
			continue
		} // Existing policies are never overwritten.
		route := core.Route{ID: "planned-group:" + sourceRoute.ID, ChannelID: app.ID, ConversationID: group.ID, ConversationType: "group", WorkspaceID: sourceRoute.WorkspaceID, Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", MemoryPolicy: "explicit_only", SendPolicy: "reply_to_trigger", Status: "active"}
		if s.Routes[app.ID] == nil {
			s.Routes[app.ID] = map[string]core.Route{}
		}
		s.Routes[app.ID][group.ID] = route
		app.Routes = append(app.Routes, route)
		p.GroupMounts = append(p.GroupMounts, PlannedGroupMount{ChannelID: app.ID, SourceRouteID: sourceRoute.ID, ConversationID: group.ID, WorkspaceID: sourceRoute.WorkspaceID})
		p.Changes = append(p.Changes, PlanChange{Kind: "group_mount", Name: group.ID, Action: "create", AfterDigest: core.Digest(route)})
	}
	s.Channels[g.Channel] = app
	if len(p.GroupMounts) > 0 {
		// reply_to_trigger is explicitly authorized by enabling group_mention. The
		// plan shows each discovered group; no separate broad send authorization.
		issue(false, "group_mounts_planned", "application", binding.Name, "Apply will create mention-only reply routes for the listed, verified source groups.")
		if runtime, ok := s.ByName["runtime\x00"+binding.Runtime]; ok && runtime.Status != "stopped" && runtime.Status != "configured" {
			issue(true, "dependent_runtime_running", "application", binding.Name, "Stop the group runtime before applying newly discovered mounts.")
		}
	}
	sort.Slice(p.GroupMounts, func(i, j int) bool { return p.GroupMounts[i].ConversationID < p.GroupMounts[j].ConversationID })
	return s
}

func applyGroupMounts(ctx context.Context, tx *core.Tx, p DualConfigPlan) error {
	for _, mount := range p.GroupMounts {
		source, err := core.ReadRoute(ctx, tx.Conn, mount.SourceRouteID)
		if err != nil {
			return err
		}
		if source.ConversationID != mount.ConversationID || source.WorkspaceID != mount.WorkspaceID || source.Mode == "ignore" || source.Status != "active" {
			return core.Fail("conflict", "source group route changed during application")
		}
		if _, err = tx.AddRoute(ctx, mount.ChannelID, core.RouteInput{ConversationID: mount.ConversationID, ConversationType: "group", Workspace: mount.WorkspaceID, Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", MemoryPolicy: "explicit_only", SendPolicy: "reply_to_trigger"}); err != nil {
			return err
		}
	}
	return nil
}

func sourceGroupIgnored(ctx context.Context, q core.Queryer, source core.DataSource, decl DataSourceConfig, conversation string) bool {
	names := []string{conversation}
	if receipt, err := core.ReadSourceGroupDiscovery(ctx, q, source.ID); err == nil {
		for _, group := range receipt.Groups {
			if group.ID == conversation {
				names = append(names, group.Name)
			}
		}
	}
	for _, rule := range append(append([]string{}, source.Ignore...), decl.Groups.Ignore...) {
		for _, name := range names {
			if rule == name {
				return true
			}
		}
	}
	return false
}

func planMissingGroupMounts(ctx context.Context, p *DualConfigPlan, s dualPlanState, issue func(bool, string, string, string, string)) dualPlanState {
	for _, binding := range groupApplications(p.Declaration.Applications) {
		s = planMissingGroupMountsFor(ctx, p, s, binding, issue)
	}
	return s
}
