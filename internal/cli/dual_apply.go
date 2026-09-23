package cli

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func (a *app) dualApplyCommand() *cobra.Command {
	var expected int
	var digest, reason string
	var authorizeBoundary, authorizeExpansion bool
	cmd := a.write("apply-runtime", "原子应用已预览的双模式声明", cobra.NoArgs, func(ctx context.Context, tx *core.Tx, _ []string, _ json.RawMessage) (any, error) {
		plan, err := BuildDualConfigPlan(ctx, a.cfg, a.home, tx.Conn)
		if err != nil {
			return nil, err
		}
		if expected != plan.AppliedVersion || digest != plan.PlanDigest {
			return nil, core.Fail("conflict", "configuration plan changed; rerun config plan and use its current digest and applied version")
		}
		for _, block := range plan.Blockers {
			if block.Code == "authorization_boundary_change" && authorizeBoundary && reason != "" {
				continue
			}
			if block.Code == "permission_expansion" && authorizeExpansion && reason != "" {
				continue
			}
			return nil, core.Fail("conflict", "configuration is blocked: %s", block.Code)
		}
		applied, err := applyDualModePlan(ctx, tx, plan)
		if err != nil {
			return nil, err
		}
		if applied.Version > plan.AppliedVersion && (authorizeBoundary || authorizeExpansion) {
			if _, err = tx.Audit(ctx, "config.reauthorize", reason, []core.Change{{ObjectType: "applied_config", ObjectID: applied.ID, Before: plan.AppliedVersion, After: applied.Version}}); err != nil {
				return nil, err
			}
		}
		return applied, nil
	})
	cmd.PreRunE = func(_ *cobra.Command, _ []string) error {
		if err := a.requireCanonicalConfig("config apply-runtime"); err != nil {
			return err
		}
		if a.input != "" {
			return core.Fail("invalid_input", "config apply-runtime reads --config; --input is not supported")
		}
		if digest == "" || expected < 0 {
			return core.Fail("invalid_input", "apply-runtime requires --plan-digest and nonnegative --expected-version from config plan")
		}
		if len(reason) > 240 || strings.ContainsAny(reason, "\r\n\x00") {
			return core.Fail("invalid_input", "reason must be a short single line")
		}
		if (authorizeBoundary || authorizeExpansion) && reason == "" {
			return core.Fail("invalid_input", "authorization flags require --reason")
		}
		a.prepared = core.Digest(a.cfg)
		return nil
	}
	cmd.Flags().IntVar(&expected, "expected-version", -1, "已应用配置的预期版本")
	cmd.Flags().StringVar(&digest, "plan-digest", "", "config plan 返回的摘要")
	cmd.Flags().BoolVar(&authorizeBoundary, "authorize-boundary", false, "明确接受身份、披露范围变化并使旧任务失效")
	cmd.Flags().BoolVar(&authorizeExpansion, "authorize-expansion", false, "明确接受能力或群范围扩张")
	cmd.Flags().StringVar(&reason, "reason", "", "授权理由（只保存安全摘要）")
	return cmd
}

func dualChange(plan DualConfigPlan, kind, name string) PlanChange {
	for _, c := range plan.Changes {
		if c.Kind == kind && c.Name == name {
			return c
		}
	}
	return PlanChange{Kind: kind, Name: name, Action: "unchanged"}
}
func planPrecondition(plan DualConfigPlan, kind, id string) (PlanPrecondition, bool) {
	for _, p := range plan.Preconditions {
		if p.Kind == kind && p.ObjectID == id {
			return p, true
		}
	}
	return PlanPrecondition{}, false
}
func sortedNames[T any](items map[string]T) []string {
	names := make([]string, 0, len(items))
	for n := range items {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// The core reader applies positive discovery, authority and fallback checks.
func proactiveSourceRoutes(ctx context.Context, q core.Queryer, source core.DataSource) ([]string, error) {
	return core.ReadOwnerSourceProcessingRoutes(ctx, q, source, time.Now())
}

func sameRouteScope(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	copyA, copyB := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(copyA)
	sort.Strings(copyB)
	for i := range copyA {
		if copyA[i] != copyB[i] {
			return false
		}
	}
	return true
}

// Overlay only the Agent policy on an already verified direct runtime. Its
// callback CID, owner user ID, dispatch route and timing remain in SQLite.
func runtimePolicyInput(current core.RuntimeConfig) core.RuntimeConfigInput {
	bash := current.AgentBash
	return core.RuntimeConfigInput{
		Scheduling: current.Scheduling,
		Name:       current.Name, Channel: current.ChannelID, RouteIDs: current.RouteIDs,
		DeliveryRouteID: current.DeliveryRouteID,
		Owner:           core.Sender{IDType: current.OwnerIDType, IDValue: current.OwnerIDValue},
		ApplicationMode: current.ApplicationMode, AgentCapabilities: current.AgentCapabilities,
		AgentBash: &bash, ExternalActions: current.ExternalActions,
		ClaudeProfile: current.ClaudeProfile,
		AnalysisModel: current.AnalysisModel, ExecutionModel: current.ExecutionModel,
		AgentPreset: current.AgentPreset, ItemThreshold: current.ItemThreshold,
		MaxWaitSeconds: current.MaxWaitSeconds, ReconcileSeconds: current.ReconcileSeconds,
		Concurrency: current.Concurrency, ExpectedVersion: current.Version,
	}
}

func applyDualModePlan(ctx context.Context, tx *core.Tx, plan DualConfigPlan) (core.AppliedConfig, error) {
	old, err := core.ReadAppliedConfig(ctx, tx.Conn, 0)
	if err != nil {
		return old, err
	}
	managed := map[string]core.ManagedConfigObject{}
	for _, o := range old.Objects {
		managed[o.Kind+"\x00"+o.Name] = o
	}
	if err := applyDualChannels(ctx, tx, plan); err != nil {
		return old, err
	}
	sourceChanged := map[string]bool{}
	for _, name := range sortedNames(plan.Declaration.DataSources) {
		s := plan.Declaration.DataSources[name]
		change := dualChange(plan, "data_source", name)
		if !*s.Enabled && change.Action == "create" {
			continue
		}
		if change.Action == "unchanged" {
			continue
		}
		robotCode := ""
		if s.Groups.MemberRobot != "" {
			robot, err := core.ReadChannel(ctx, tx.Conn, s.Groups.MemberRobot)
			if err != nil {
				return old, err
			}
			robotCode = robot.Identity.RobotCode
		}
		_, err = tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: name, Channel: s.Channel, Workspace: plan.DefaultWorkspace, ReconcileSeconds: *s.ReconcileSeconds, Ignore: s.Groups.Ignore, MemberRobotCode: robotCode, Enabled: s.Enabled, HistoryEnabled: s.HistoryImport.Enabled, HistoryDays: *s.HistoryImport.Days, DirectEnabled: s.Direct.Enabled, BackfillAfterEnable: s.BackfillAfterEnable, RetentionDays: *s.Retention.Days})
		if err != nil {
			return old, err
		}
		sourceChanged[name] = true
	}
	if p := plan.Declaration.Applications.Proactive; p != nil {
		change := dualChange(plan, "application", "proactive")
		if *p.Enabled {
			if change.Action != "unchanged" || sourceChanged[p.Source] || dualChange(plan, "agent", p.Agent).Action != "unchanged" {
				source, err := core.ReadDataSource(ctx, tx.Conn, p.Source)
				if err != nil {
					return old, err
				}
				channel, err := core.ReadChannel(ctx, tx.Conn, source.ChannelID)
				if err != nil {
					return old, err
				}
				agent := plan.Declaration.Agents[p.Agent]
				routeIDs, err := proactiveSourceRoutes(ctx, tx.Conn, source)
				if err != nil {
					return old, err
				}
				current, err := core.ReadRuntime(ctx, tx.Conn, "proactive")
				version := 0
				if err == nil {
					version = current.Version
				} else if core.ErrorCode(err) != "not_found" {
					return old, err
				}
				if len(routeIDs) == 0 {
					// A newly configured source has no membership proof yet. Commit
					// the declaration and source, then create the watcher only after
					// a later positive discovery and explicit reapply.
					if version > 0 {
						if current.Status != "stopped" {
							return old, core.Fail("conflict", "stop proactive runtime before removing its admitted scope")
						}
						if err = tx.InvalidateRuntimeConfigWork(ctx, current.ID); err != nil {
							return old, err
						}
						if _, _, err = tx.SyncRuntimeGroupRoutes(ctx, current.ID, nil); err != nil {
							return old, err
						}
					}
				} else {
					runtime, configureErr := tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Scheduling: p.Scheduling, Name: "proactive", Channel: channel.ID, RouteIDs: routeIDs, Owner: core.Sender{IDType: p.Owner.IDType, IDValue: p.Owner.IDValue}, AgentCapabilities: agent.Capabilities, AgentBash: &agent.Bash, ExternalActions: agent.ExternalActions, ClaudeProfile: agent.ClaudeProfile, AnalysisModel: p.AnalysisModel, ExecutionModel: agent.ExecutionModel, AgentPreset: agent.Preset, ItemThreshold: *p.Batch.Items, MaxWaitSeconds: *p.Batch.MaxWaitSeconds, ReconcileSeconds: *plan.Declaration.DataSources[p.Source].ReconcileSeconds, ExpectedVersion: version})
					if configureErr != nil {
						return old, configureErr
					}
					if runtime.Status == "configured" {
						runtime, err = tx.SetRuntimeStatus(ctx, runtime.ID, "stopped", "")
						if err != nil {
							return old, err
						}
					}
					boundSource, bound, boundErr := core.DataSourceForRuntime(ctx, tx.Conn, runtime.ID)
					if boundErr != nil {
						return old, boundErr
					}
					if bound && boundSource.ID != source.ID {
						return old, core.Fail("denied", "proactive runtime is bound to another data source")
					}
					if !bound {
						if _, err = tx.BindRuntimeDataSource(ctx, runtime.ID, source.ID); err != nil {
							return old, err
						}
					}
				}
			} else if sourceChanged[p.Source] {
				runtime, err := core.ReadRuntime(ctx, tx.Conn, "proactive")
				if err == nil {
					if err = tx.InvalidateRuntimeConfigWork(ctx, runtime.ID); err != nil {
						return old, err
					}
				}
			}
		} else if obj, ok := managed["application\x00proactive"]; ok && change.Action != "unchanged" {
			runtime, err := core.ReadRuntime(ctx, tx.Conn, obj.ObjectID)
			if err != nil {
				return old, err
			}
			if err = tx.InvalidateRuntimeConfigWork(ctx, runtime.ID); err != nil {
				return old, err
			}
			if _, err = tx.SetRuntimeStatus(ctx, runtime.ID, "stopped", ""); err != nil {
				return old, err
			}
		}
	}
	for _, binding := range ownerApplications(plan.Declaration.Applications) {
		o := binding.App
		change := dualChange(plan, "application", binding.Name)
		if *o.Enabled && change.Action != "unchanged" || (*o.Enabled && o.Agent != "" && dualChange(plan, "agent", o.Agent).Action != "unchanged") {
			current, err := core.ReadRuntime(ctx, tx.Conn, o.Runtime)
			if err != nil {
				return old, err
			}
			if current.ApplicationMode != "direct" || current.Status != "stopped" {
				return old, core.Fail("conflict", "owner-private reference must be a stopped direct runtime")
			}
			for _, routeID := range current.RouteIDs {
				if _, err := core.RuntimeOwnerDirectProcessingRoute(ctx, tx.Conn, current, routeID); err != nil {
					return old, err
				}
			}
			input := runtimePolicyInput(current)
			if o.Agent == "" {
				bash := true
				input.AgentBash = &bash
				input.ExternalActions = "owner_request"
			} else {
				agent := plan.Declaration.Agents[o.Agent]
				input.AgentCapabilities = agent.Capabilities
				input.AgentBash = &agent.Bash
				input.ExternalActions = agent.ExternalActions
				input.ClaudeProfile = agent.ClaudeProfile
				input.ExecutionModel = agent.ExecutionModel
				input.AgentPreset = agent.Preset
			}
			if _, err = tx.ConfigureRuntime(ctx, input); err != nil {
				return old, err
			}
		} else if !*o.Enabled && change.Action != "unchanged" {
			if obj, ok := managed["application\x00"+binding.Name]; ok {
				current, err := core.ReadRuntime(ctx, tx.Conn, obj.ObjectID)
				if err != nil {
					return old, err
				}
				if current.Status != "stopped" {
					return old, core.Fail("conflict", "stop owner-private runtime before revoking managed policy")
				}
				input := runtimePolicyInput(current)
				bash := false
				input.AgentBash = &bash
				input.ExternalActions = "owner_confirmation"
				if _, err = tx.ConfigureRuntime(ctx, input); err != nil {
					return old, err
				}
			}
		}
	}
	if err := applyGroupMounts(ctx, tx, plan); err != nil {
		return old, err
	}
	for _, binding := range groupApplications(plan.Declaration.Applications) {
		g := binding.App
		change := dualChange(plan, "application", binding.Name)
		agentChanged := dualChange(plan, "agent", g.DefaultAgent).Action != "unchanged"
		for _, binding := range g.Bindings {
			if dualChange(plan, "agent", binding.Agent).Action != "unchanged" {
				agentChanged = true
			}
		}
		if *g.Enabled && (change.Action != "unchanged" || sourceChanged[g.Source] || agentChanged || len(plan.GroupMounts) > 0) {
			app, err := core.ReadChannel(ctx, tx.Conn, g.Channel)
			if err != nil {
				return old, err
			}
			contextChannel, err := groupContextChannel(ctx, tx.Conn, plan.Declaration, g)
			if err != nil {
				return old, err
			}
			agent := plan.Declaration.Agents[g.DefaultAgent]
			routeIDs := []string{}
			for _, r := range app.Routes {
				if groupSourceIgnored(ctx, tx.Conn, plan.Declaration, g, r.ConversationID) {
					continue
				}
				if r.ConversationType != "group" || r.Status != "active" || r.Mode != "assistant" || r.SendPolicy != "reply_to_trigger" || !hasConfigString(r.Triggers, "mention") {
					continue
				}
				if contextChannel != "" {
					dwsRoute, err := core.RouteFor(ctx, tx.Conn, contextChannel, r.ConversationID)
					if err != nil || dwsRoute.Mode == "ignore" || dwsRoute.WorkspaceID != r.WorkspaceID {
						continue
					}
				}
				routeIDs = append(routeIDs, r.ID)
			}
			if len(routeIDs) == 0 {
				return old, core.Fail("denied", "group Agent requires one verified shared group route")
			}
			owner, err := groupApplicationOwner(ctx, tx.Conn, plan.Declaration, binding, app)
			if err != nil {
				return old, err
			}
			current, err := core.ReadRuntime(ctx, tx.Conn, binding.Runtime)
			version := 0
			if err == nil {
				version = current.Version
			} else if core.ErrorCode(err) != "not_found" {
				return old, err
			}
			_, err = tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Scheduling: core.Scheduling{ExecutionTimeoutSeconds: *g.ExecutionTimeoutSeconds}, Name: binding.Runtime, Channel: app.ID, RouteIDs: routeIDs, DeliveryRouteID: routeIDs[0], Owner: owner, ApplicationMode: "group_mention", ContextChannel: contextChannel, AgentCapabilities: agent.Capabilities, AgentBash: &agent.Bash, ExternalActions: agent.ExternalActions, ClaudeProfile: agent.ClaudeProfile, ExecutionModel: agent.ExecutionModel, AgentPreset: agent.Preset, Concurrency: *g.ExecutionConcurrency, ExpectedVersion: version})
			if err != nil {
				return old, err
			}
		} else if !*g.Enabled {
			if obj, ok := managed["application\x00"+binding.Name]; ok && change.Action != "unchanged" {
				runtime, err := core.ReadRuntime(ctx, tx.Conn, obj.ObjectID)
				if err != nil {
					return old, err
				}
				if err = tx.InvalidateRuntimeConfigWork(ctx, runtime.ID); err != nil {
					return old, err
				}
				if _, err = tx.SetRuntimeStatus(ctx, runtime.ID, "stopped", ""); err != nil {
					return old, err
				}
			}
		}
	}
	// Removed bot entries revoke their managed workers even though no current
	// declaration remains to visit in the loops above.
	desiredObjects := declarationObjects(plan.Declaration)
	for _, obj := range old.Objects {
		if obj.Kind != "application" || obj.ObjectType != "runtime" {
			continue
		}
		if _, exists := desiredObjects["application\x00"+obj.Name]; exists {
			continue
		}
		if runtimeRebound(ctx, tx.Conn, plan.Declaration.Applications, obj.ObjectID) {
			continue
		}
		if err := tx.InvalidateRuntimeConfigWork(ctx, obj.ObjectID); err != nil {
			return old, err
		}
		if _, err := tx.SetRuntimeStatus(ctx, obj.ObjectID, "stopped", ""); err != nil {
			return old, err
		}
	}
	// Build the immutable applied manifest from the actual transaction state.
	state, err := readDualPlanState(ctx, tx.Conn)
	if err != nil {
		return old, err
	}
	objects := []core.ManagedConfigObject{}
	appendActual := func(kind, name, objType, objID string) error {
		pre, ok := state.ByObject[objType+"\x00"+objID]
		if !ok {
			return core.Fail("conflict", "managed object vanished before manifest commit")
		}
		objects = append(objects, core.ManagedConfigObject{Kind: kind, Name: name, ObjectType: objType, ObjectID: objID, Snapshot: pre.Snapshot})
		return nil
	}
	for _, c := range plan.DesiredChannels {
		actual, err := core.ReadChannel(ctx, tx.Conn, c.Name)
		if err != nil {
			return old, err
		}
		objects = append(objects, core.ManagedConfigObject{Kind: "channel", Name: c.Name, ObjectType: "channel", ObjectID: actual.ID, Snapshot: dualChannelManagedSnapshot(actual, c)})
	}
	for _, c := range plan.Changes {
		if c.Kind == "channel" && c.Action == "preserve" {
			if obj, ok := managed["channel\x00"+c.Name]; ok {
				objects = append(objects, obj)
			}
		}
	}
	for _, name := range sortedNames(plan.Declaration.DataSources) {
		source, err := core.ReadDataSource(ctx, tx.Conn, name)
		if core.ErrorCode(err) == "not_found" && !*plan.Declaration.DataSources[name].Enabled {
			continue
		}
		if err != nil {
			return old, err
		}
		if err = appendActual("data_source", name, "data_source", source.ID); err != nil {
			return old, err
		}
	}
	for _, name := range sortedNames(plan.Declaration.Agents) {
		a := plan.Declaration.Agents[name]
		pre, ok := planPrecondition(plan, "preset", a.Preset)
		if !ok {
			return old, core.Fail("conflict", "preset verification changed")
		}
		objects = append(objects, core.ManagedConfigObject{Kind: "agent", Name: name, ObjectType: "preset", ObjectID: a.Preset, Snapshot: pre.Snapshot})
	}
	applicationNames := []string{"proactive"}
	enabledApplications := map[string]bool{"proactive": *plan.Declaration.Applications.Proactive.Enabled}
	for _, binding := range groupApplications(plan.Declaration.Applications) {
		applicationNames = append(applicationNames, binding.Name)
		enabledApplications[binding.Name] = *binding.App.Enabled
	}
	for _, binding := range ownerApplications(plan.Declaration.Applications) {
		applicationNames = append(applicationNames, binding.Name)
		enabledApplications[binding.Name] = *binding.App.Enabled
	}
	for _, obj := range old.Objects {
		if obj.Kind == "application" && obj.ObjectType == "runtime" && !hasConfigString(applicationNames, obj.Name) && !runtimeRebound(ctx, tx.Conn, plan.Declaration.Applications, obj.ObjectID) {
			applicationNames = append(applicationNames, obj.Name)
		}
	}
	for _, name := range applicationNames {
		if !enabledApplications[name] {
			if obj, ok := managed["application\x00"+name]; ok {
				if runtimeRebound(ctx, tx.Conn, plan.Declaration.Applications, obj.ObjectID) {
					continue
				}
				if err = appendActual("application", name, "runtime", obj.ObjectID); err != nil {
					return old, err
				}
			}
			continue
		}
		actualName := applicationRuntimeName(plan.Declaration.Applications, name)
		runtime, err := core.ReadRuntime(ctx, tx.Conn, actualName)
		if err != nil {
			if name == "proactive" && core.ErrorCode(err) == "not_found" {
				source, sourceErr := core.ReadDataSource(ctx, tx.Conn, plan.Declaration.Applications.Proactive.Source)
				if sourceErr != nil {
					return old, sourceErr
				}
				routes, routeErr := proactiveSourceRoutes(ctx, tx.Conn, source)
				if routeErr != nil {
					return old, routeErr
				}
				if len(routes) == 0 {
					continue // enabled declaration, creation deferred until proof
				}
			}
			return old, err
		}
		if err = appendActual("application", name, "runtime", runtime.ID); err != nil {
			return old, err
		}
	}
	for _, binding := range groupApplications(plan.Declaration.Applications) {
		g := binding.App
		if !*g.Enabled {
			continue
		}
		for _, b := range g.Bindings {
			if groupSourceIgnored(ctx, tx.Conn, plan.Declaration, g, b.ConversationID) {
				continue
			}
			app, err := core.ReadChannel(ctx, tx.Conn, g.Channel)
			if err != nil {
				return old, err
			}
			route, err := core.RouteFor(ctx, tx.Conn, app.ID, b.ConversationID)
			if err != nil {
				return old, err
			}
			if route.Mode == "ignore" {
				continue
			}
			if err = appendActual("binding", groupBindingName(binding.Name, b.ConversationID), "route", route.ID); err != nil {
				return old, err
			}
		}
	}
	declaration, _ := json.Marshal(map[string]any{"data_sources": plan.Declaration.DataSources, "agents": plan.Declaration.Agents, "applications": plan.Declaration.Applications, "channels": plan.DesiredChannels, "default_workspace": plan.DefaultWorkspace})
	return tx.CommitAppliedConfig(ctx, plan.AppliedVersion, plan.SchemaVersion, declaration, objects)
}
