package cli

import (
	"context"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// Bot declarations bind presentation to a robot; permissions remain specific
// to its owner-private or group entry point.
func fullOwnerAgent(external string, base AgentDeclaration) AgentDeclaration {
	return AgentDeclaration{Preset: base.Preset, ClaudeProfile: base.ClaudeProfile, ExecutionModel: base.ExecutionModel,
		Capabilities: []string{"conversation_history_read", "artifact_create", "local_read", "local_write", "local_test"},
		Bash:         true, ExternalActions: external, Skills: core.RuntimeSkillPolicy{Inherit: "executor", Paths: []string{}}}
}

func normalizeBotApplications(d *DualModeDeclaration, diagnostics *[]ConfigDiagnostic) error {
	if len(d.Applications.Bots) == 0 {
		return nil
	}
	if d.Agents == nil {
		d.Agents = map[string]AgentDeclaration{}
	}
	usedRuntimes := map[string]bool{}
	if o := d.Applications.OwnerPrivate; o != nil && (o.Runtime != "" || o.Enabled != nil && *o.Enabled) {
		runtime := o.Runtime
		if runtime == "" {
			runtime = "owner-private"
		}
		usedRuntimes[runtime] = true
	}
	for _, channel := range sortedNames(d.Applications.Bots) {
		if !dataSourceDeclarationName.MatchString(channel) || strings.Contains(channel, "_") {
			return dualInvalid("applications.bots", "bot keys must be valid channel names of at most 63 characters")
		}
		b := d.Applications.Bots[channel]
		if legacy := d.Applications.GroupMention; legacy != nil && legacy.Channel == channel {
			return dualInvalid("applications.bots", "the same robot cannot also be configured through applications.group_mention")
		}
		if b.GroupMention != nil {
			g := b.GroupMention
			if g.Channel != "" && g.Channel != channel {
				return dualInvalid("applications.bots.group_mention.channel", "must match the bot map key")
			}
			g.Channel = channel
			g.Owner = b.Owner
			if g.Agent != "" && g.DefaultAgent != "" && g.Agent != g.DefaultAgent {
				return dualInvalid("applications.bots.group_mention.agent", "conflicts with default_agent")
			}
			if g.Agent != "" {
				g.DefaultAgent = g.Agent
			}
			g.Agent = ""
			defaultString(&g.DefaultAgent, b.DefaultAgent)
			defaultBool(&g.Enabled, false)
			defaultString(&g.Trigger, "mention")
			defaultString(&g.ReplyPolicy, "reply_to_trigger")
			defaultInt(&g.ExecutionConcurrency, 4)
			if !bounded(g.ExecutionConcurrency, 1, 32) {
				return dualInvalid("applications.bots.group_mention.execution_concurrency", "must be 1..32")
			}
		}
		if b.OwnerPrivate != nil {
			o := b.OwnerPrivate
			defaultBool(&o.Enabled, false)
			if *o.Enabled {
				defaultString(&o.Runtime, botRuntimeName(channel, "owner"))
			}
			if o.Runtime != "" {
				if usedRuntimes[o.Runtime] {
					return dualInvalid("applications.bots.owner_private.runtime", "runtime is bound more than once")
				}
				usedRuntimes[o.Runtime] = true
			}
			if *o.Enabled && o.Agent == "" {
				base, ok := d.Agents[b.DefaultAgent]
				if !ok {
					return dualInvalid("applications.bots.default_agent", "owner persona requires a declared default_agent")
				}
				name := botRuntimeName(channel, "owner")
				if _, exists := d.Agents[name]; exists {
					return dualInvalid("applications.bots.owner_private.agent", "reserved generated owner Agent name conflicts; use an explicit agent")
				}
				d.Agents[name] = fullOwnerAgent("owner_request", base)
				o.Agent = name
			}
		}
		d.Applications.Bots[channel] = b
	}
	return nil
}

func validateBotApplications(d *DualModeDeclaration, ref func(string, string) error, agentRef func(string, bool, bool) error, sourceRef func(string, bool) error, diagnostics *[]ConfigDiagnostic) error {
	for _, channel := range sortedNames(d.Applications.Bots) {
		b := d.Applications.Bots[channel]
		if err := ref("channel", channel); err != nil {
			return err
		}
		if b.DefaultAgent != "" {
			if err := agentRef(b.DefaultAgent, true, false); err != nil {
				return err
			}
		}
		if b.Owner.IDType != "" || b.Owner.IDValue != "" {
			if b.Owner.IDType != "user_id" || !stableConfigID.MatchString(b.Owner.IDValue) {
				return dualInvalid("applications.bots.owner", "requires a stable DWS-verified user_id")
			}
		}
		if o := b.OwnerPrivate; o != nil {
			if o.Runtime != "" && !declarationName.MatchString(o.Runtime) {
				return dualInvalid("applications.bots.owner_private.runtime", "invalid runtime name")
			}
			if err := agentRef(o.Agent, *o.Enabled, false); err != nil {
				return err
			}
			if o.Agent != "" {
				a := d.Agents[o.Agent]
				if a.ExternalActions == "owner_delegated" {
					return dualInvalid("applications.bots.owner_private.agent", "requires owner-private action policy")
				}
			}
		}
		if g := b.GroupMention; g != nil {
			if g.Trigger != "mention" || g.ReplyPolicy != "reply_to_trigger" {
				return dualInvalid("applications.bots.group_mention", "requires mention and reply_to_trigger")
			}
			if err := sourceRef(g.Source, false); err != nil {
				return err
			}
			if err := agentRef(g.DefaultAgent, *g.Enabled, true); err != nil {
				return err
			}
			if g.DefaultAgent != "" && d.Agents[g.DefaultAgent].ExternalActions != "owner_confirmation" {
				return dualInvalid("applications.bots.group_mention.agent", "group agents require owner_confirmation")
			}
			seen := map[string]bool{}
			for _, binding := range g.Bindings {
				if !stableConfigID.MatchString(binding.ConversationID) || seen[binding.ConversationID] {
					return dualInvalid("applications.bots.group_mention.bindings", "requires unique stable conversation IDs")
				}
				seen[binding.ConversationID] = true
				if err := agentRef(binding.Agent, true, true); err != nil {
					return err
				}
				if d.Agents[binding.Agent].ExternalActions != "owner_confirmation" {
					return dualInvalid("applications.bots.group_mention.bindings", "group agents require owner_confirmation")
				}
			}
		}
	}
	return nil
}

type groupApplicationBinding struct {
	Name, Runtime, Channel string
	Owner                  ApplicationOwner
	App                    *GroupMentionApplication
}
type ownerApplicationBinding struct {
	Name, Channel string
	App           *OwnerPrivateApplication
}

func groupApplications(a ApplicationDeclarations) []groupApplicationBinding {
	out := []groupApplicationBinding{}
	if a.GroupMention != nil {
		out = append(out, groupApplicationBinding{Name: "group_mention", Runtime: "group-mention", Channel: a.GroupMention.Channel, App: a.GroupMention})
	}
	for _, channel := range sortedNames(a.Bots) {
		b := a.Bots[channel]
		if b.GroupMention != nil {
			out = append(out, groupApplicationBinding{Name: "bot:" + channel + ":group", Runtime: botRuntimeName(channel, "group"), Channel: channel, Owner: b.Owner, App: b.GroupMention})
		}
	}
	return out
}
func ownerApplications(a ApplicationDeclarations) []ownerApplicationBinding {
	out := []ownerApplicationBinding{}
	if a.OwnerPrivate != nil {
		out = append(out, ownerApplicationBinding{Name: "owner_private", App: a.OwnerPrivate})
	}
	for _, channel := range sortedNames(a.Bots) {
		if o := a.Bots[channel].OwnerPrivate; o != nil {
			out = append(out, ownerApplicationBinding{Name: "bot:" + channel + ":owner", Channel: channel, App: o})
		}
	}
	return out
}
func ownerAgentBound(a ApplicationDeclarations, name string) bool {
	for _, o := range ownerApplications(a) {
		if o.App.Agent == name {
			return true
		}
	}
	return false
}
func groupAgentUsed(a ApplicationDeclarations, name string) bool {
	for _, g := range groupApplications(a) {
		if g.App.DefaultAgent == name || groupAgentBound(g.App.Bindings, name) {
			return true
		}
	}
	return false
}
func groupBindingName(app, conversation string) string {
	if app == "group_mention" {
		return conversation
	}
	return app + ":" + conversation
}
func applicationRuntimeName(a ApplicationDeclarations, name string) string {
	if name == "proactive" {
		return name
	}
	for _, g := range groupApplications(a) {
		if g.Name == name {
			return g.Runtime
		}
	}
	for _, o := range ownerApplications(a) {
		if o.Name == name {
			return o.App.Runtime
		}
	}
	return ""
}
func isOwnerApplication(name string) bool {
	return name == "owner_private" || strings.HasPrefix(name, "bot:") && strings.HasSuffix(name, ":owner")
}

// Owner identity can be verified without running a data source. An explicitly
// supplied identity, existing private runtime, or channel history binding must
// lead to the same authenticated DWS owner, never to a message-text claim.
func groupApplicationOwner(ctx context.Context, q core.Queryer, d DualModeDeclaration, b groupApplicationBinding, app core.Channel) (core.Sender, error) {
	owner := core.Sender{IDType: b.Owner.IDType, IDValue: b.Owner.IDValue}
	if owner.IDValue == "" {
		for _, o := range ownerApplications(d.Applications) {
			if o.Channel != b.Channel && o.Channel != "" {
				continue
			}
			r, e := core.ReadRuntime(ctx, q, o.App.Runtime)
			if e == nil && r.ChannelID == app.ID {
				owner = core.Sender{IDType: r.OwnerIDType, IDValue: r.OwnerIDValue}
				break
			}
		}
	}
	if owner.IDValue == "" {
		history := app.Identity.HistoryChannel
		if source, ok := d.DataSources[b.App.Source]; ok {
			history = source.Channel
		}
		if history != "" {
			c, e := core.ReadChannel(ctx, q, history)
			if e != nil {
				return owner, e
			}
			if c.Kind != core.ChannelDwsPersonal || c.Tenant != app.Tenant {
				return owner, core.Fail("denied", "robot owner identity is outside the application tenant")
			}
			owner = core.Sender{IDType: "user_id", IDValue: c.Identity.ExpectedUserID}
		}
	}
	if owner.IDType != "user_id" || owner.IDValue == "" {
		return owner, core.Fail("denied", "robot requires an authenticated DWS owner identity")
	}
	var count int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM identity_aliases WHERE tenant=? AND id_type=? AND id_value=? AND verified=1 AND basis='authenticated_dws_profile'", app.Tenant, owner.IDType, owner.IDValue).Scan(&count); err != nil {
		return owner, err
	}
	if count != 1 {
		return owner, core.Fail("denied", "robot owner is not authenticated by DWS")
	}
	return owner, nil
}

func groupContextChannel(ctx context.Context, q core.Queryer, d DualModeDeclaration, g *GroupMentionApplication) (string, error) {
	if g.Source == "" {
		return "", nil
	}
	source, ok := d.DataSources[g.Source]
	if !ok {
		return "", core.Fail("denied", "group context source is undeclared")
	}
	c, err := core.ReadChannel(ctx, q, source.Channel)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}
func groupSourceIgnored(ctx context.Context, q core.Queryer, d DualModeDeclaration, g *GroupMentionApplication, conversation string) bool {
	if g.Source == "" {
		return false
	}
	decl := d.DataSources[g.Source]
	source, err := core.ReadDataSource(ctx, q, g.Source)
	if err == nil {
		return sourceGroupIgnored(ctx, q, source, decl, conversation)
	}
	for _, id := range decl.Groups.Ignore {
		if id == conversation {
			return true
		}
	}
	return false
}

func validateGroupPlan(p *DualConfigPlan, s dualPlanState, b groupApplicationBinding, issue func(bool, string, string, string, string)) {
	g := b.App
	if !*g.Enabled {
		return
	}
	app, ok := s.Channels[g.Channel]
	if !ok {
		issue(true, "application_channel_missing", "application", b.Name, "Application bot channel is not installed.")
		return
	}
	if app.Kind != core.ChannelDingTalkApp {
		issue(true, "application_channel_kind", "application", b.Name, "Bot interaction requires an application robot.")
	}
	if !app.Capabilities.Verified["receive"] || !app.Capabilities.Verified["send"] {
		issue(true, "group_bot_capabilities", "application", b.Name, "Bot receive and send capabilities must be verified.")
	}
	if _, err := groupApplicationOwner(context.Background(), s.Q, p.Declaration, b, app); err != nil {
		issue(true, "group_owner_unverified", "application", b.Name, "Robot owner requires authenticated DWS identity; collection may remain disabled.")
	}
	contextChannel := ""
	if g.Source != "" {
		source := p.Declaration.DataSources[g.Source]
		dws, exists := s.Channels[source.Channel]
		if !exists || dws.Kind != core.ChannelDwsPersonal || dws.Tenant != app.Tenant {
			issue(true, "group_context_identity", "application", b.Name, "Optional context source must use same-enterprise DWS.")
			return
		}
		contextChannel = dws.ID
	}
	admitted := 0
	for _, r := range app.Routes {
		if r.ConversationType != "group" || r.Status != "active" || r.Mode != "assistant" || r.SendPolicy != "reply_to_trigger" || !hasConfigString(r.Triggers, "mention") || groupSourceIgnored(context.Background(), s.Q, p.Declaration, g, r.ConversationID) {
			continue
		}
		if contextChannel != "" {
			cr, exists := s.Routes[contextChannel][r.ConversationID]
			if !exists || cr.Status != "active" || cr.Mode == "ignore" || cr.WorkspaceID != r.WorkspaceID {
				continue
			}
		}
		admitted++
	}
	if admitted == 0 {
		issue(true, "group_routes_missing", "application", b.Name, "At least one explicit mention reply route is required; optional context must match that group.")
	}
	for _, binding := range g.Bindings {
		if groupSourceIgnored(context.Background(), s.Q, p.Declaration, g, binding.ConversationID) {
			continue
		}
		r, exists := s.Routes[app.ID][binding.ConversationID]
		if exists && r.Mode == "ignore" {
			continue
		}
		if !exists || r.Status != "active" || r.Mode != "assistant" || !hasConfigString(r.Triggers, "mention") || r.SendPolicy != "reply_to_trigger" {
			issue(true, "binding_missing_bot_route", "binding", groupBindingName(b.Name, binding.ConversationID), "Explicit group binding lacks an admitted application mention route.")
		}
		if contextChannel != "" {
			cr, exists := s.Routes[contextChannel][binding.ConversationID]
			if !exists || cr.Mode == "ignore" || cr.Status != "active" || cr.WorkspaceID != r.WorkspaceID {
				issue(true, "binding_missing_dws_route", "binding", groupBindingName(b.Name, binding.ConversationID), "Explicit group binding lacks its configured context route.")
			}
		}
	}
	for _, name := range append([]string{g.DefaultAgent}, groupBindingAgentNames(g.Bindings)...) {
		a := p.Declaration.Agents[name]
		if len(a.Directories) > 0 && !hasConfigString(a.Capabilities, "local_read") {
			issue(true, "group_directory_read_required", "agent", name, "Group directory snapshots require local_read.")
		}
		if a.Bash && len(a.Directories) > 0 {
			issue(true, "bash_directory_snapshot_incompatible", "agent", name, "Bash cannot be combined with bounded directory snapshots.")
		}
		if hasConfigString(a.Capabilities, "local_test") && !a.Bash {
			issue(true, "group_shell_capability_unsupported", "agent", name, "Shell tests require configured Bash permission.")
		}
	}
}
func hasConfigString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func botRuntimeName(channel, mode string) string {
	name := "bot-" + channel + "-" + mode
	if len(name) <= 63 {
		return name
	}
	return "bot-" + channel[:40] + "-" + core.Hash([]byte(channel))[:8] + "-" + mode
}
func runtimeRebound(ctx context.Context, q core.Queryer, a ApplicationDeclarations, id string) bool {
	r, err := core.ReadRuntime(ctx, q, id)
	if err != nil {
		return false
	}
	for _, b := range ownerApplications(a) {
		if b.App.Enabled != nil && *b.App.Enabled && b.App.Runtime == r.Name {
			return true
		}
	}
	for _, b := range groupApplications(a) {
		if b.App.Enabled != nil && *b.App.Enabled && b.Runtime == r.Name {
			return true
		}
	}
	return a.Proactive != nil && a.Proactive.Enabled != nil && *a.Proactive.Enabled && r.Name == "proactive"
}
