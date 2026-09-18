package cli

import (
	"context"
	"sort"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type dualPlanState struct {
	Q             core.Queryer
	Preconditions []PlanPrecondition
	ByObject      map[string]PlanPrecondition
	ByName        map[string]PlanPrecondition
	Channels      map[string]core.Channel
	Routes        map[string]map[string]core.Route
	Workspaces    map[string]core.Workspace
}

func readDualPlanState(ctx context.Context, q core.Queryer) (dualPlanState, error) {
	s := dualPlanState{Q: q, Preconditions: []PlanPrecondition{}, ByObject: map[string]PlanPrecondition{}, ByName: map[string]PlanPrecondition{}, Channels: map[string]core.Channel{}, Routes: map[string]map[string]core.Route{}, Workspaces: map[string]core.Workspace{}}
	add := func(kind, name, id string, version int, status string, value any) {
		p := PlanPrecondition{Kind: kind, Name: name, ObjectID: id, Version: version, Status: status, Snapshot: core.Digest(value)}
		s.Preconditions = append(s.Preconditions, p)
		s.ByObject[kind+"\x00"+id] = p
		s.ByName[kind+"\x00"+name] = p
	}
	channels, err := core.ChannelList(ctx, q)
	if err != nil {
		return s, err
	}
	for _, c := range channels {
		s.Channels[c.Name] = c
		s.Routes[c.ID] = map[string]core.Route{}
		for _, r := range c.Routes {
			s.Routes[c.ID][r.ConversationID] = r
			add("route", c.Name+"/"+r.ConversationID, r.ID, r.Version, r.Status, map[string]any{"channel_id": r.ChannelID, "conversation_id": r.ConversationID, "workspace_id": r.WorkspaceID, "mode": r.Mode, "triggers": r.Triggers, "audience_policy": r.AudiencePolicy, "memory_policy": r.MemoryPolicy, "send_policy": r.SendPolicy, "status": r.Status, "version": r.Version})
		}
		add("channel", c.Name, c.ID, c.ConfigVersion, c.Status, map[string]any{"kind": c.Kind, "tenant": c.Tenant, "identity": c.Identity, "config_version": c.ConfigVersion, "status": c.Status, "capabilities": c.Capabilities, "credential_ref_digest": core.Digest(c.CredentialRef)})
	}
	sources, err := core.DataSourceList(ctx, q)
	if err != nil {
		return s, err
	}
	for _, d := range sources {
		add("data_source", d.Name, d.ID, d.Version, d.Status, map[string]any{"channel_id": d.ChannelID, "workspace_id": d.WorkspaceID, "reconcile_seconds": d.ReconcileSeconds, "ignore": d.Ignore, "member_robot_code": d.MemberRobotCode, "enabled": d.Enabled, "history_enabled": d.HistoryEnabled, "history_days": d.HistoryDays, "version": d.Version})
		// Discovery updates route_ids independently of YAML and config
		// version. Include its digest in the plan fence without treating
		// ordinary collection changes as managed configuration drift.
		scope := append([]string{}, d.RouteIDs...)
		sort.Strings(scope)
		add("data_source_scope", d.Name, d.ID, 0, d.Status, scope)
		receipt, receiptErr := core.ReadSourceGroupDiscovery(ctx, q, d.ID)
		if receiptErr != nil && core.ErrorCode(receiptErr) != "not_found" {
			return s, receiptErr
		}
		// Receipt validity and positive groups may change without the source
		// route list changing. Fence that change without exposing group IDs.
		if receiptErr == nil {
			add("data_source_receipt", d.Name, d.ID, 0, "observed", receipt)
		} else {
			add("data_source_receipt", d.Name, d.ID, 0, "absent", nil)
		}
	}
	runtimes, err := core.RuntimeList(ctx, q)
	if err != nil {
		return s, err
	}
	for _, r := range runtimes {
		add("runtime", r.Name, r.ID, r.Version, r.Status, runtimeManagedSnapshot(r))
	}
	rows, err := q.QueryContext(ctx, "SELECT id,name,coalesce(path,'') FROM workspaces ORDER BY name")
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var w core.Workspace
		if err = rows.Scan(&w.ID, &w.Name, &w.Path); err != nil {
			return s, err
		}
		s.Workspaces[w.Name] = w
		add("workspace", w.Name, w.ID, 0, "active", w)
	}
	return s, rows.Err()
}

// The schema-19 permission columns are absent from schema-18 applied snapshots.
// Omitting them only at their exact old defaults preserves those snapshots while
// still detecting an enabled Bash policy or changed external-action authority.
func runtimeManagedSnapshot(r core.RuntimeConfig) map[string]any {
	value := map[string]any{"channel_id": r.ChannelID, "delivery_route_id": r.DeliveryRouteID, "owner_principal_id": r.OwnerPrincipalID, "owner_id_type": r.OwnerIDType, "owner_id_value": r.OwnerIDValue, "application_mode": r.ApplicationMode, "context_channel_id": r.ContextChannelID, "agent_capabilities": r.AgentCapabilities, "memory_scope": r.MemoryScope, "claude_profile": r.ClaudeProfile, "analysis_model": r.AnalysisModel, "execution_model": r.ExecutionModel, "agent_preset": r.AgentPreset, "item_threshold": r.ItemThreshold, "max_wait_seconds": r.MaxWaitSeconds, "reconcile_seconds": r.ReconcileSeconds}
	if r.AgentBash {
		value["agent_bash"] = true
	}
	if r.ExternalActions != "owner_confirmation" {
		value["external_actions"] = r.ExternalActions
	}
	if r.Concurrency != 1 {
		value["execution_concurrency"] = r.Concurrency
	}
	if r.AnalysisConcurrency != 8 {
		value["analysis_concurrency"] = r.AnalysisConcurrency
	}
	if r.AnalysisTimeoutSeconds != 120 {
		value["analysis_timeout_seconds"] = r.AnalysisTimeoutSeconds
	}
	if r.ExecutionTimeoutSeconds != 900 {
		value["execution_timeout_seconds"] = r.ExecutionTimeoutSeconds
	}
	if r.ReviewTimeoutSeconds != 120 {
		value["review_timeout_seconds"] = r.ReviewTimeoutSeconds
	}
	return value
}

func validateDualPlanReferences(p *DualConfigPlan, s dualPlanState, issue func(bool, string, string, string, string)) {
	for _, change := range p.Changes {
		if change.Kind != "data_source" || change.Action == "unchanged" {
			continue
		}
		source, found := s.ByName["data_source\x00"+change.Name]
		if !found {
			continue
		}
		rows, err := s.Q.QueryContext(context.Background(), "SELECT r.name,r.status FROM runtime_configs r LEFT JOIN runtime_data_sources b ON b.runtime_id=r.id WHERE b.data_source_id=? OR r.context_channel_id=(SELECT channel_id FROM data_sources WHERE id=?)", source.ObjectID, source.ObjectID)
		if err != nil {
			issue(true, "dependent_runtime_check_failed", "data_source", change.Name, "Cannot verify dependent consumers before a source change.")
			continue
		}
		for rows.Next() {
			var name, status string
			if err = rows.Scan(&name, &status); err != nil {
				issue(true, "dependent_runtime_check_failed", "data_source", change.Name, "Cannot verify dependent consumer state.")
				break
			}
			if status != "stopped" {
				issue(true, "dependent_runtime_running", "data_source", change.Name, "Stop dependent AI consumers before changing source scope.")
			}
		}
		rows.Close()
	}
	for _, change := range p.Changes {
		if change.Kind != "agent" || change.Action == "unchanged" {
			continue
		}
		refs := []struct{ agent, runtime string }{{p.Declaration.Applications.Proactive.Agent, "proactive"}}
		for _, binding := range ownerApplications(p.Declaration.Applications) {
			refs = append(refs, struct{ agent, runtime string }{binding.App.Agent, binding.App.Runtime})
		}
		for _, binding := range groupApplications(p.Declaration.Applications) {
			refs = append(refs, struct{ agent, runtime string }{binding.App.DefaultAgent, binding.Runtime})
			for _, b := range binding.App.Bindings {
				refs = append(refs, struct{ agent, runtime string }{b.Agent, binding.Runtime})
			}
		}
		for _, ref := range refs {
			if ref.agent != change.Name {
				continue
			}
			if runtime, ok := s.ByName["runtime\x00"+ref.runtime]; ok && runtime.Status != "stopped" {
				issue(true, "dependent_runtime_running", "agent", change.Name, "Stop the dependent AI runtime before changing its Agent declaration.")
			}
		}
	}
	workspace := s.Workspaces[p.DefaultWorkspace]
	if workspace.ID == "" {
		issue(true, "workspace_missing", "workspace", p.DefaultWorkspace, "Default workspace must already exist.")
	}
	for name, source := range p.Declaration.DataSources {
		dws, ok := s.Channels[source.Channel]
		if !ok {
			issue(true, "channel_missing", "data_source", name, "Declared DWS channel is not installed.")
			continue
		}
		if dws.Kind != core.ChannelDwsPersonal {
			issue(true, "channel_kind", "data_source", name, "Data source requires a personal DWS channel.")
		}
		if source.Groups.MemberRobot != "" {
			robot, ok := s.Channels[source.Groups.MemberRobot]
			if !ok {
				issue(true, "robot_channel_missing", "data_source", name, "Robot application channel is not installed.")
				continue
			}
			if robot.Kind != core.ChannelDingTalkApp || robot.Tenant != dws.Tenant {
				issue(true, "robot_identity", "data_source", name, "Robot and DWS source must belong to the same enterprise.")
			}
			if robot.Identity.RobotCode == "" || dws.Identity.DeliveryRobotCode != robot.Identity.RobotCode || dws.Identity.DeliveryRobotName == "" {
				issue(true, "robot_member_binding", "data_source", name, "DWS channel must name the selected robot and bind its exact robot code before group discovery.")
			}
		}
		if *source.Groups.ActiveDays != 30 {
			issue(true, "active_days_unsupported", "data_source", name, "Runtime discovery currently supports only the verified 30-day group window.")
		}
		if *source.HistoryImport.Enabled && *source.HistoryImport.Days != 30 {
			issue(true, "history_days_unsupported", "data_source", name, "Automatic initial historical import currently supports a 30-day window.")
		}
		if !dws.Capabilities.Verified["receive"] || !dws.Capabilities.Verified["history"] {
			active := *p.Declaration.Applications.Proactive.Enabled && p.Declaration.Applications.Proactive.Source == name
			issue(active, "source_capabilities", "data_source", name, "DWS receive and history capability must be verified before collection or AI starts.")
		}
	}
	activeAgent := map[string]bool{}
	if app := p.Declaration.Applications.Proactive; app != nil && *app.Enabled {
		activeAgent[app.Agent] = true
	}
	for _, binding := range groupApplications(p.Declaration.Applications) {
		if *binding.App.Enabled {
			activeAgent[binding.App.DefaultAgent] = true
			for _, b := range binding.App.Bindings {
				activeAgent[b.Agent] = true
			}
		}
	}
	for _, binding := range ownerApplications(p.Declaration.Applications) {
		if *binding.App.Enabled {
			activeAgent[binding.App.Agent] = true
		}
	}
	for name, a := range p.Declaration.Agents {
		if activeAgent[name] && len(a.Directories) > 0 {
			if a.Bash && (p.Declaration.Applications.Proactive.Agent == name || groupAgentUsed(p.Declaration.Applications, name)) {
				issue(true, "bash_directory_snapshot_incompatible", "agent", name, "Bash-enabled group or proactive Agents cannot use bounded directory snapshots.")
			}
			if proactive := p.Declaration.Applications.Proactive; proactive != nil && *proactive.Enabled && proactive.Agent == name {
				read, test := false, false
				for _, capability := range a.Capabilities {
					if capability == "local_read" {
						read = true
					}
					if capability == "local_test" {
						test = true
					}
				}
				if !read {
					issue(true, "owner_directory_read_required", "agent", name, "Declared owner directories require local_read.")
				}
				if test && !a.Bash {
					issue(true, "owner_directory_test_sandbox_unavailable", "agent", name, "Declared-directory shell tests require a verified OS sandbox; this mode does not execute arbitrary project tests.")
				}
				if !a.Bash {
					issue(false, "owner_directory_copy", "agent", name, "Owner directories are read-only sources; edits and artifacts use isolated copies, and code changes receive a private worktree and local commit.")
				}
			} else if groupAgentUsed(p.Declaration.Applications, name) {
				issue(false, "group_directory_snapshot", "agent", name, "Group directories become bounded text snapshots; shell tools and direct host filesystem access are unavailable.")
			}
		}
		if a.ClaudeProfile != "" {
			issue(false, "profile_requires_runtime_check", "agent", name, "Claude alias is resolved when the runtime starts.")
		}
	}
	for _, binding := range ownerApplications(p.Declaration.Applications) {
		o := binding.App
		if o.Runtime == "" {
			continue
		}
		runtime, err := core.ReadRuntime(context.Background(), s.Q, o.Runtime)
		if err != nil {
			issue(true, "owner_private_runtime_missing", "application", binding.Name, "Referenced owner-private runtime must already exist.")
		} else if runtime.ApplicationMode != "direct" || runtime.Name != o.Runtime || len(runtime.RouteIDs) == 0 {
			issue(true, "owner_private_runtime_kind", "application", binding.Name, "Referenced instance must be an existing direct runtime with an admitted inbox.")
		} else {
			if binding.Channel != "" {
				declaredOwner := p.Declaration.Applications.Bots[binding.Channel].Owner
				if declaredOwner.IDValue != "" && (declaredOwner.IDValue != runtime.OwnerIDValue || declaredOwner.IDType != runtime.OwnerIDType) {
					issue(true, "owner_private_identity_mismatch", "application", binding.Name, "Bot owner must match the verified private runtime owner.")
				}
				bot, e := core.ReadChannel(context.Background(), s.Q, binding.Channel)
				if e != nil || bot.ID != runtime.ChannelID {
					issue(true, "owner_private_channel_mismatch", "application", binding.Name, "Owner-private runtime must belong to the declared robot.")
				}
			}
			for _, routeID := range runtime.RouteIDs {
				if _, err := core.RuntimeOwnerDirectProcessingRoute(context.Background(), s.Q, runtime, routeID); err != nil {
					issue(true, "owner_private_route_unverified", "application", binding.Name, "Owner-private runtime must retain verified inbound owner and outbound robot routes.")
					break
				}
			}
			if *o.Enabled {
				app, channelErr := core.ReadChannel(context.Background(), s.Q, runtime.ChannelID)
				if channelErr == nil && binding.Channel == "" {
					if _, exists := p.Declaration.Applications.Bots[app.Name]; exists {
						issue(true, "application_declaration_conflict", "application", binding.Name, "Use the bot owner_private entry instead of a legacy owner binding for the same robot.")
					}
				}
				if channelErr != nil || !app.Capabilities.Verified["send"] || !app.Capabilities.Verified["receive"] {
					issue(true, "owner_private_bot_capabilities", "application", binding.Name, "Owner-private bot receive and send capabilities must be verified.")
				}
			}
		}
	}
	if a := p.Declaration.Applications.Proactive; a != nil && *a.Enabled {
		source, exists := p.Declaration.DataSources[a.Source]
		if !exists {
			return
		}
		c, ok := s.Channels[source.Channel]
		if !ok {
			return
		}
		if _, ok := s.Workspaces[p.DefaultWorkspace]; !ok {
			return
		}
		var verified int
		if p.Declaration.Agents[a.Agent].ExternalActions == "owner_delegated" {
			err := s.Q.QueryRowContext(context.Background(), "SELECT count(*) FROM identity_aliases WHERE tenant=? AND id_type='user_id' AND id_value=? AND verified=1 AND basis='authenticated_dws_profile'", c.Tenant, a.Owner.IDValue).Scan(&verified)
			if err != nil || verified != 1 || a.Owner.IDType != "user_id" || a.Owner.IDValue != c.Identity.ExpectedUserID {
				issue(true, "delegated_owner_unverified", "application", "proactive", "Owner delegation requires the exact user_id authenticated by the source DWS profile.")
			}
		} else if err := pOwnerVerified(c.Tenant, a.Owner, s, &verified); err != nil || verified == 0 {
			issue(true, "owner_unverified", "application", "proactive", "Owner identity must have a verified platform mapping.")
		}

	}
	for _, binding := range groupApplications(p.Declaration.Applications) {
		validateGroupPlan(p, s, binding, issue)
	}
}

func groupAgentBound(bindings []GroupAgentBinding, name string) bool {
	for _, binding := range bindings {
		if binding.Agent == name {
			return true
		}
	}
	return false
}

// Keep identity verification read-only. The plan's caller supplies one coherent
// read transaction; this lookup cannot create an alias from a YAML claim.
func pOwnerVerified(tenant string, owner ApplicationOwner, s dualPlanState, out *int) error {
	return s.Q.QueryRowContext(context.Background(), "SELECT count(*) FROM identity_aliases WHERE tenant=? AND id_type=? AND id_value=? AND verified=1", tenant, owner.IDType, owner.IDValue).Scan(out)
}

func groupBindingAgentNames(bindings []GroupAgentBinding) []string {
	names := []string{}
	for _, b := range bindings {
		names = append(names, b.Agent)
	}
	return names
}

// Older manifests include the outbound address retired by record_only. Accept
// only an exact match to that previous shape; every permission remains checked.
func legacyProactiveSnapshotMatches(ctx context.Context, q core.Queryer, o core.ManagedConfigObject) bool {
	if o.ObjectType != "runtime" || o.Kind != "application" || o.Name != "proactive" {
		return false
	}
	r, err := core.ReadRuntime(ctx, q, o.ObjectID)
	if err != nil || r.ApplicationMode != "proactive" {
		return false
	}
	var legacyRoute string
	if err = q.QueryRowContext(ctx, "SELECT delivery_route_id FROM runtime_configs WHERE id=?", r.ID).Scan(&legacyRoute); err != nil {
		return false
	}
	value := runtimeManagedSnapshot(r)
	value["delivery_route_id"] = legacyRoute
	return o.Snapshot == core.Digest(value)
}
