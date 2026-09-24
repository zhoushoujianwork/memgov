package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

type PlanChange struct {
	Kind                string `json:"kind"`
	Name                string `json:"name"`
	Action              string `json:"action"`
	BeforeDigest        string `json:"before_digest,omitempty"`
	AfterDigest         string `json:"after_digest,omitempty"`
	Drift               bool   `json:"drift"`
	PermissionExpansion bool   `json:"permission_expansion"`
	PermissionReduction bool   `json:"permission_reduction"`
	BoundaryChange      bool   `json:"boundary_change"`
}
type PlanIssue struct {
	Code    string `json:"code"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

// Snapshot is the hash of configuration/authorization state, excluding mutable
// collection cursors and task bodies. It can also populate ManagedConfigObject.
type PlanPrecondition struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	ObjectID string `json:"object_id"`
	Version  int    `json:"version"`
	Status   string `json:"status"`
	Snapshot string `json:"snapshot"`
	Commit   string `json:"commit,omitempty"`
	Clean    bool   `json:"clean,omitempty"`
}
type DualConfigPlan struct {
	GroupMounts          []PlannedGroupMount     `json:"group_mounts"`
	SchemaVersion        int                     `json:"schema_version"`
	AppliedVersion       int                     `json:"applied_version"`
	AppliedDigest        string                  `json:"applied_digest"`
	DesiredDigest        string                  `json:"desired_digest"`
	PlanDigest           string                  `json:"plan_digest"`
	Declaration          DualModeDeclaration     `json:"declaration"`
	DesiredChannels      []core.ChannelInput     `json:"desired_channels"`
	DefaultWorkspace     string                  `json:"default_workspace"`
	Changes              []PlanChange            `json:"changes"`
	Preconditions        []PlanPrecondition      `json:"preconditions"`
	Blockers             []PlanIssue             `json:"blockers"`
	Warnings             []PlanIssue             `json:"warnings"`
	RetentionPreviews    []core.RetentionPreview `json:"retention_previews"`
	UnresolvedReferences []ConfigReference       `json:"unresolved_references"`
	Ready                bool                    `json:"ready"`
	Discovery            string                  `json:"discovery"`
}

func (a *app) dualPlanCommand() *cobra.Command {
	return a.simple("plan", "只读比较双模式配置与当前数据库，生成可复核的变更摘要", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) {
		if err := a.requireCanonicalConfig("config plan"); err != nil {
			return nil, err
		}
		if a.input != "" {
			return nil, core.Fail("invalid_input", "config plan reads --config; --input is not supported")
		}
		// Unlike the general Store opener, this cannot create a lock file, migrate,
		// checkpoint WAL, or acquire a writable SQLite connection.
		if _, err := os.Stat(a.dbPath()); err != nil {
			if os.IsNotExist(err) {
				return nil, core.Fail("not_found", "database not initialized; run memgov init")
			}
			return nil, core.Fail("unavailable", "cannot inspect configuration database")
		}
		u := url.URL{Scheme: "file", Path: a.dbPath()}
		v := u.Query()
		v.Set("mode", "ro")
		v.Add("_pragma", "query_only(1)")
		v.Add("_pragma", "busy_timeout(5000)")
		u.RawQuery = v.Encode()
		db, err := sql.Open("sqlite", u.String())
		if err != nil {
			return nil, err
		}
		defer db.Close()
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		return BuildDualConfigPlan(ctx, a.cfg, a.home, tx)
	})
}

func canonicalDeclaration(d DualModeDeclaration) DualModeDeclaration {
	raw, _ := json.Marshal(d)
	var out DualModeDeclaration
	_ = json.Unmarshal(raw, &out)
	for name, s := range out.DataSources {
		sort.Strings(s.Groups.Ignore)
		out.DataSources[name] = s
	}
	for name, a := range out.Agents {
		sort.Strings(a.Capabilities)
		sort.Strings(a.Directories)
		sort.Strings(a.Skills.Paths)
		if a.KnowledgeMCP != nil {
			sort.Strings(a.KnowledgeMCP.Sources)
		}
		sort.Slice(a.Skills.Resolved, func(i, j int) bool { return a.Skills.Resolved[i].Name < a.Skills.Resolved[j].Name })
		out.Agents[name] = a
	}
	for _, binding := range groupApplications(out.Applications) {
		sort.Slice(binding.App.Bindings, func(i, j int) bool {
			return binding.App.Bindings[i].ConversationID < binding.App.Bindings[j].ConversationID
		})
	}
	return out
}
func declarationObjects(d DualModeDeclaration) map[string]any {
	objects := map[string]any{}
	for name, s := range d.DataSources {
		objects["data_source\x00"+name] = s
	}
	for name, a := range d.Agents {
		objects["agent\x00"+name] = a
	}
	if p := d.Applications.Proactive; p != nil && (p.Source != "" || p.Agent != "" || (p.Enabled != nil && *p.Enabled)) {
		objects["application\x00proactive"] = p
	}
	for _, binding := range groupApplications(d.Applications) {
		g := binding.App
		if g.Source != "" || g.DefaultAgent != "" || (g.Enabled != nil && *g.Enabled) {
			objects["application\x00"+binding.Name] = g
			for _, b := range g.Bindings {
				objects["binding\x00"+groupBindingName(binding.Name, b.ConversationID)] = b
			}
		}
	}
	for _, binding := range ownerApplications(d.Applications) {
		o := binding.App
		if o.Runtime != "" || o.Agent != "" || (o.Enabled != nil && *o.Enabled) {
			objects["application\x00"+binding.Name] = o
		}
	}
	return objects
}
func declarationDisabled(value any) bool {
	switch v := value.(type) {
	case DataSourceConfig:
		return v.Enabled != nil && !*v.Enabled
	case *ProactiveApplication:
		return v.Enabled != nil && !*v.Enabled
	case *GroupMentionApplication:
		return v.Enabled != nil && !*v.Enabled
	case *OwnerPrivateApplication:
		return v.Enabled != nil && !*v.Enabled
	}
	return false
}
func setDifference(before, after []string) (expanded, reduced bool) {
	a, b := map[string]bool{}, map[string]bool{}
	for _, v := range before {
		a[v] = true
	}
	for _, v := range after {
		b[v] = true
	}
	for v := range b {
		if !a[v] {
			expanded = true
		}
	}
	for v := range a {
		if !b[v] {
			reduced = true
		}
	}
	return
}
func classifyPermissionChange(c *PlanChange, before, after any) {
	switch b := before.(type) {
	case AgentDeclaration:
		a, ok := after.(AgentDeclaration)
		if !ok {
			return
		}
		c.PermissionExpansion, c.PermissionReduction = setDifference(b.Capabilities, a.Capabilities)
		expanded, reduced := setDifference(b.Directories, a.Directories)
		c.PermissionExpansion = c.PermissionExpansion || expanded
		c.PermissionReduction = c.PermissionReduction || reduced
		c.PermissionExpansion = c.PermissionExpansion || (!b.Bash && a.Bash) || (b.ExternalActions == "owner_confirmation" && a.ExternalActions != "owner_confirmation")
		c.PermissionReduction = c.PermissionReduction || (b.Bash && !a.Bash) || (b.ExternalActions != "owner_confirmation" && a.ExternalActions == "owner_confirmation")
		beforeSkills, afterSkills := []string{}, []string{}
		for _, skill := range b.Skills.Resolved {
			beforeSkills = append(beforeSkills, skill.Name+":"+skill.Digest)
		}
		for _, skill := range a.Skills.Resolved {
			afterSkills = append(afterSkills, skill.Name+":"+skill.Digest)
		}
		skillExpanded, skillReduced := setDifference(beforeSkills, afterSkills)
		c.PermissionExpansion = c.PermissionExpansion || skillExpanded
		c.PermissionReduction = c.PermissionReduction || skillReduced
		beforeSources, afterSources := []string{}, []string{}
		if b.KnowledgeMCP != nil {
			beforeSources = b.KnowledgeMCP.Sources
		}
		if a.KnowledgeMCP != nil {
			afterSources = a.KnowledgeMCP.Sources
		}
		sourceExpanded, sourceReduced := setDifference(beforeSources, afterSources)
		c.PermissionExpansion = c.PermissionExpansion || sourceExpanded
		c.PermissionReduction = c.PermissionReduction || sourceReduced
		c.BoundaryChange = b.Preset != a.Preset || b.ExternalActions != a.ExternalActions || b.Skills.Inherit != a.Skills.Inherit || core.Digest(b.Skills) != core.Digest(a.Skills) || core.Digest(b.KnowledgeMCP) != core.Digest(a.KnowledgeMCP)
	case DataSourceConfig:
		a, ok := after.(DataSourceConfig)
		if !ok {
			return
		}
		reduced, expanded := setDifference(b.Groups.Ignore, a.Groups.Ignore)
		c.PermissionExpansion = expanded
		c.PermissionReduction = reduced
		beforeDirect := b.Direct.Enabled != nil && *b.Direct.Enabled
		afterDirect := a.Direct.Enabled != nil && *a.Direct.Enabled
		c.PermissionExpansion = c.PermissionExpansion || (!beforeDirect && afterDirect)
		c.PermissionReduction = c.PermissionReduction || (beforeDirect && !afterDirect)
		c.BoundaryChange = b.Channel != a.Channel || b.Groups.MemberRobot != a.Groups.MemberRobot
	case *ProactiveApplication:
		a, ok := after.(*ProactiveApplication)
		if !ok {
			return
		}
		c.BoundaryChange = b.Source != a.Source || b.Agent != a.Agent || b.Owner != a.Owner
	case *GroupMentionApplication:
		a, ok := after.(*GroupMentionApplication)
		if !ok {
			return
		}
		c.BoundaryChange = b.Owner != a.Owner || b.Source != a.Source || b.Channel != a.Channel || b.DefaultAgent != a.DefaultAgent
	case *OwnerPrivateApplication:
		a, ok := after.(*OwnerPrivateApplication)
		if !ok {
			return
		}
		c.BoundaryChange = b.Runtime != a.Runtime || b.Agent != a.Agent
	case GroupAgentBinding:
		a, ok := after.(GroupAgentBinding)
		if ok {
			c.BoundaryChange = b.Agent != a.Agent
		}
	}
}

// BuildDualConfigPlan only reads q and preset Git state. Callers should supply a
// read transaction (or the eventual apply transaction) for a coherent snapshot.
// Profiles remain unresolved references: no shell alias or model is executed.
func BuildDualConfigPlan(ctx context.Context, cfg Config, home string, q core.Queryer) (DualConfigPlan, error) {
	p := DualConfigPlan{SchemaVersion: 1, Changes: []PlanChange{}, Preconditions: []PlanPrecondition{}, Blockers: []PlanIssue{}, Warnings: []PlanIssue{}, RetentionPreviews: []core.RetentionPreview{}, UnresolvedReferences: []ConfigReference{}, Discovery: "known_routes_only; additional eligible groups are discovered by the runtime"}
	valid, err := NormalizeDualModeConfig(cfg)
	if err != nil {
		return p, err
	}
	p.Declaration = canonicalDeclaration(valid.Declaration)
	for name, declaration := range p.Declaration.Agents {
		if declaration.KnowledgeMCP != nil && declaration.KnowledgeMCP.Command != "" {
			info, statErr := os.Stat(declaration.KnowledgeMCP.Command)
			if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				return p, core.Fail("unavailable", "Agent %q knowledge MCP executable is unavailable", name)
			}
		}
		resolved, resolveErr := resolveClaudeSkills(declaration.Skills)
		if resolveErr != nil {
			return p, resolveErr
		}
		declaration.Skills.Resolved = resolved
		p.Declaration.Agents[name] = declaration
	}
	raw, _ := json.Marshal(cfg.Channels)
	_ = json.Unmarshal(raw, &p.DesiredChannels)
	sort.Slice(p.DesiredChannels, func(i, j int) bool { return p.DesiredChannels[i].Name < p.DesiredChannels[j].Name })
	for _, c := range p.DesiredChannels {
		if c.Route != nil {
			sort.Strings(c.Route.Triggers)
		}
		if c.Subscription != nil {
			sort.Strings(c.Subscription.Events)
		}
	}
	p.DefaultWorkspace = cfg.DefaultWorkspace
	if p.DefaultWorkspace == "" {
		p.DefaultWorkspace = "global"
	}
	p.DesiredDigest = core.Digest(map[string]any{"declaration": p.Declaration, "channels": p.DesiredChannels, "default_workspace": p.DefaultWorkspace})
	var schemaVersion int
	if err = q.QueryRowContext(ctx, "SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1").Scan(&schemaVersion); err != nil {
		return p, core.Fail("conflict", "cannot plan against an unrecognized database schema")
	}
	if schemaVersion != core.SchemaVersion {
		return p, core.Fail("conflict", "database schema differs from this binary; migrate explicitly before planning")
	}
	applied, err := core.ReadAppliedConfig(ctx, q, 0)
	if err != nil {
		return p, err
	}
	p.AppliedVersion = applied.Version
	p.AppliedDigest = applied.Digest
	var previous DualModeDeclaration
	if applied.Version > 0 {
		if err = json.Unmarshal(applied.Declaration, &previous); err != nil {
			return p, core.Fail("conflict", "applied declaration cannot be decoded")
		}
		previous = canonicalDeclaration(previous)
	}
	before, after := declarationObjects(previous), declarationObjects(p.Declaration)
	state, err := readDualPlanState(ctx, q)
	if err != nil {
		return p, err
	}
	p.Preconditions = append(p.Preconditions, state.Preconditions...)
	issue := func(block bool, code, kind, name, summary string) {
		v := PlanIssue{Code: code, Kind: kind, Name: name, Summary: summary}
		if block {
			p.Blockers = append(p.Blockers, v)
		} else {
			p.Warnings = append(p.Warnings, v)
		}
	}
	for _, diag := range valid.Diagnostics {
		issue(false, diag.Code, "binding", "", diag.Summary)
	}
	managed := map[string]core.ManagedConfigObject{}
	managedActual := map[string]bool{}
	for _, o := range applied.Objects {
		managed[o.Kind+"\x00"+o.Name] = o
		managedActual[o.ObjectType+"\x00"+o.ObjectID] = true
	}
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	for k, o := range managed {
		if o.Kind == "channel" {
			continue
		}
		keys[k] = true
	}
	for key := range keys {
		parts := strings.SplitN(key, "\x00", 2)
		kind, name := parts[0], parts[1]
		b, bExists := before[key]
		a, aExists := after[key]
		change := PlanChange{Kind: kind, Name: name}
		if bExists {
			change.BeforeDigest = core.Digest(b)
		}
		if aExists {
			change.AfterDigest = core.Digest(a)
		}
		switch {
		case !aExists:
			change.Action = "disable"
			change.PermissionReduction = true
		case !bExists:
			change.Action = "create"
		case change.BeforeDigest == change.AfterDigest:
			change.Action = "unchanged"
		case declarationDisabled(a):
			change.Action = "disable"
			change.PermissionReduction = true
		default:
			change.Action = "update"
		}
		if kind == "agent" && !bExists && aExists {
			agent := a.(AgentDeclaration)
			change.PermissionExpansion = agent.Bash || agent.ExternalActions != "owner_confirmation" || agent.KnowledgeMCP != nil
		}
		if bExists && aExists {
			classifyPermissionChange(&change, b, a)
		}
		if kind == "application" && isOwnerApplication(name) && aExists && !declarationDisabled(a) {
			ownerApp := a.(*OwnerPrivateApplication)
			if current, e := core.ReadRuntime(ctx, q, ownerApp.Runtime); e == nil {
				bash, external := true, "owner_request"
				if ownerApp.Agent != "" {
					agent := p.Declaration.Agents[ownerApp.Agent]
					bash, external = agent.Bash, agent.ExternalActions
				}
				change.PermissionExpansion = change.PermissionExpansion || (!current.AgentBash && bash) || (current.ExternalActions != "owner_request" && external == "owner_request")
				change.PermissionReduction = change.PermissionReduction || (current.AgentBash && !bash) || (current.ExternalActions == "owner_request" && external != "owner_request")
			}
		}
		if o, ok := managed[key]; ok {
			current, found := state.ByObject[o.ObjectType+"\x00"+o.ObjectID]
			// Presets are resolved below; they do not live in SQLite.
			if o.ObjectType != "preset" && o.ObjectType != "agent_preset" {
				if !found || o.Snapshot == "" || (o.Snapshot != current.Snapshot && !legacyProactiveSnapshotMatches(ctx, q, o)) {
					change.Drift = true
					issue(true, "managed_object_drift", kind, name, "Managed object is missing or differs from its applied snapshot.")
				}
				if found && current.Status != "stopped" && (o.ObjectType == "runtime" || o.ObjectType == "data_source") && change.Action != "unchanged" && !proactiveSchedulingOnly(b, a) {
					issue(true, "running_object_change", kind, name, "Stop the managed process before changing its configuration.")
				}
			}
		} else if kind == "data_source" || kind == "application" {
			actualKind := kind
			actualName := name
			if actualKind == "application" {
				actualKind = "runtime"
				actualName = applicationRuntimeName(p.Declaration.Applications, name)
			}
			if current, found := state.ByName[actualKind+"\x00"+actualName]; found {
				if kind == "application" && isOwnerApplication(name) {
					if current.Status != "stopped" {
						issue(true, "running_object_change", kind, name, "Stop the owner-private direct runtime before adopting or changing its Agent policy.")
					}
				} else {
					change.Action = "unmanaged_conflict"
					issue(true, "unmanaged_conflict", kind, name, "A same-named object is not managed by this declaration; explicit adoption is required.")
				}
			}
		}
		if change.BoundaryChange {
			issue(true, "authorization_boundary_change", kind, name, "Changed identity or disclosure boundary requires explicit reauthorization and invalidation of old execution snapshots.")
		}
		if change.PermissionExpansion {
			issue(true, "permission_expansion", kind, name, "Expanded capabilities or admitted scope require an explicit reason before application.")
		}
		p.Changes = append(p.Changes, change)
	}
	for _, c := range state.Preconditions {
		if (c.Kind == "runtime" || c.Kind == "data_source") && !managedActual[c.Kind+"\x00"+c.ObjectID] {
			issue(false, "unmanaged_preserved", c.Kind, c.Name, "Existing unmanaged object will be preserved.")
		}
	}
	for _, r := range valid.UnresolvedReferences {
		if r.Kind == "claude_profile" {
			p.UnresolvedReferences = append(p.UnresolvedReferences, r)
			issue(false, "profile_deferred", r.Kind, r.Name, "Profile syntax is valid; its alias is not executed by this plan.")
			continue
		}
		if r.Kind == "preset" {
			status, e := agent.Status(ctx, home, r.Name)
			if e != nil {
				pre := PlanPrecondition{Kind: "preset", Name: r.Name, ObjectID: r.Name, Status: "unavailable", Snapshot: core.Digest(map[string]any{"name": r.Name, "error": core.ErrorCode(e)})}
				p.Preconditions = append(p.Preconditions, pre)
				issue(true, "preset_unavailable", "preset", r.Name, "Preset is missing or cannot be verified; enable a valid committed preset before applying.")
				continue
			}
			pre := PlanPrecondition{Kind: "preset", Name: r.Name, ObjectID: r.Name, Status: status.Status, Commit: status.Commit, Clean: status.Clean, Snapshot: core.Digest(status)}
			p.Preconditions = append(p.Preconditions, pre)
			if status.Status != "enabled" || !status.Clean || status.Commit == "" {
				issue(true, "preset_not_ready", "preset", r.Name, "Preset must be enabled, clean and committed.")
			}
			for _, o := range applied.Objects {
				if (o.ObjectType == "preset" || o.ObjectType == "agent_preset") && o.ObjectID == r.Name && (o.Snapshot == "" || o.Snapshot != pre.Snapshot) {
					issue(true, "managed_object_drift", o.Kind, o.Name, "Managed preset differs from its applied snapshot.")
					for i := range p.Changes {
						if p.Changes[i].Kind == o.Kind && p.Changes[i].Name == o.Name {
							p.Changes[i].Drift = true
						}
					}
				}
			}
		}
	}
	state = planDualChannels(ctx, &p, state, applied, issue)
	for _, binding := range groupApplications(p.Declaration.Applications) {
		state = planMissingGroupMountsFor(ctx, &p, state, binding, issue)
	}
	for name, source := range p.Declaration.DataSources {
		preview, previewErr := core.PreviewDataSourceRetention(ctx, q, name, *source.Retention.Days, time.Now())
		if core.ErrorCode(previewErr) == "not_found" {
			continue
		}
		if previewErr != nil {
			issue(true, "retention_preview_failed", "data_source", name, "Cannot preview raw messages affected by the retention policy.")
			continue
		}
		p.RetentionPreviews = append(p.RetentionPreviews, preview)
	}
	sort.Slice(p.RetentionPreviews, func(i, j int) bool { return p.RetentionPreviews[i].Source < p.RetentionPreviews[j].Source })
	validateDualPlanReferences(&p, state, issue)
	planProactiveSourceScope(ctx, q, &p, issue)
	sort.Slice(p.Changes, func(i, j int) bool {
		return p.Changes[i].Kind+"\x00"+p.Changes[i].Name < p.Changes[j].Kind+"\x00"+p.Changes[j].Name
	})
	sort.Slice(p.Preconditions, func(i, j int) bool {
		a, b := p.Preconditions[i], p.Preconditions[j]
		return a.Kind+"\x00"+a.Name+"\x00"+a.ObjectID < b.Kind+"\x00"+b.Name+"\x00"+b.ObjectID
	})
	sortIssues := func(items []PlanIssue) {
		sort.Slice(items, func(i, j int) bool {
			a, b := items[i], items[j]
			return a.Kind+"\x00"+a.Name+"\x00"+a.Code < b.Kind+"\x00"+b.Name+"\x00"+b.Code
		})
	}
	sortIssues(p.Blockers)
	sortIssues(p.Warnings)
	p.Ready = len(p.Blockers) == 0
	p.PlanDigest = core.Digest(map[string]any{"schema_version": p.SchemaVersion, "database_schema_version": schemaVersion, "desired_digest": p.DesiredDigest, "applied_version": p.AppliedVersion, "applied_digest": p.AppliedDigest, "preconditions": p.Preconditions, "blockers": p.Blockers, "group_mounts": p.GroupMounts})
	return p, nil
}

func planProactiveSourceScope(ctx context.Context, q core.Queryer, p *DualConfigPlan, issue func(bool, string, string, string, string)) {
	proactive := p.Declaration.Applications.Proactive
	if proactive == nil || !*proactive.Enabled {
		return
	}
	source, err := core.ReadDataSource(ctx, q, proactive.Source)
	if err != nil {
		return // the first apply may create the source in the same transaction
	}
	admitted, err := proactiveSourceRoutes(ctx, q, source)
	if err != nil {
		issue(true, "source_scope_invalid", "application", "proactive", "Source processing routes cannot be verified against its group channel.")
		return
	}
	current, err := core.ReadRuntime(ctx, q, "proactive")
	if core.ErrorCode(err) == "not_found" {
		for i := range p.Changes {
			if p.Changes[i].Kind == "application" && p.Changes[i].Name == "proactive" && p.Changes[i].Action == "unchanged" {
				if len(admitted) == 0 {
					p.Changes[i].Action = "deferred"
				} else {
					p.Changes[i].Action = "create"
				}
			}
		}
		if len(admitted) == 0 {
			issue(false, "proactive_deferred", "application", "proactive", "Source has no admitted groups yet; create it now and reapply after verified discovery.")
		}
		return
	}
	if err != nil {
		issue(true, "proactive_scope_unavailable", "application", "proactive", "Cannot read the proactive runtime scope.")
		return
	}
	bound, linked, linkErr := core.DataSourceForRuntime(ctx, q, current.ID)
	if linkErr != nil || (linked && bound.ID != source.ID) {
		issue(true, "proactive_source_binding", "application", "proactive", "Proactive runtime is bound to a different or unreadable source.")
		return
	}
	if sameRouteScope(admitted, current.RouteIDs) {
		return
	}
	if current.Status != "stopped" {
		issue(true, "dependent_runtime_running", "application", "proactive", "Stop proactive runtime before refreshing discovered source scope.")
	}
	for i := range p.Changes {
		if p.Changes[i].Kind == "application" && p.Changes[i].Name == "proactive" && p.Changes[i].Action == "unchanged" {
			if len(admitted) == 0 {
				p.Changes[i].Action = "defer_scope"
			} else {
				p.Changes[i].Action = "refresh_scope"
			}
		}
	}
	issue(false, "source_scope_refresh", "application", "proactive", "Stopped proactive scope will be refreshed from admitted source groups.")
}
