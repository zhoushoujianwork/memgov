package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var profileName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

// RuntimeConfigInput is the durable contract for one unattended watcher. Route
// IDs and the owner identity must already be verified by the channel layer.
type RuntimeConfigInput struct {
	Scheduling
	Name              string   `json:"name"`
	Channel           string   `json:"channel"`
	RouteIDs          []string `json:"route_ids"`
	DeliveryRouteID   string   `json:"delivery_route_id"`
	Owner             Sender   `json:"owner"`
	ApplicationMode   string   `json:"application_mode,omitempty"`
	ContextChannel    string   `json:"context_channel,omitempty"`
	AgentCapabilities []string `json:"agent_capabilities,omitempty"`
	MemoryScope       string   `json:"memory_scope,omitempty"`
	AgentBash         *bool    `json:"agent_bash,omitempty"`
	ExternalActions   string   `json:"external_actions,omitempty"`
	ClaudeProfile     string   `json:"claude_profile,omitempty"`
	AnalysisModel     string   `json:"analysis_model,omitempty"`
	ExecutionModel    string   `json:"execution_model,omitempty"`
	AgentPreset       string   `json:"agent_preset,omitempty"`
	ItemThreshold     int      `json:"item_threshold,omitempty"`
	MaxWaitSeconds    int      `json:"max_wait_seconds,omitempty"`
	ReconcileSeconds  int      `json:"reconcile_seconds,omitempty"`
	Concurrency       int      `json:"concurrency,omitempty"`
	ExpectedVersion   int      `json:"expected_version,omitempty"`
}

type RuntimeConfig struct {
	Scheduling
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	ChannelID         string   `json:"channel_id"`
	RouteIDs          []string `json:"route_ids"`
	DeliveryRouteID   string   `json:"delivery_route_id"`
	OwnerPrincipalID  string   `json:"owner_principal_id"`
	OwnerIDType       string   `json:"owner_id_type"`
	OwnerIDValue      string   `json:"owner_id_value"`
	ApplicationMode   string   `json:"application_mode"`
	CompletionPolicy  string   `json:"completion_policy"`
	ContextChannelID  string   `json:"context_channel_id,omitempty"`
	AgentCapabilities []string `json:"agent_capabilities"`
	MemoryScope       string   `json:"memory_scope"`
	AgentBash         bool     `json:"agent_bash"`
	ExternalActions   string   `json:"external_actions"`
	ClaudeProfile     string   `json:"claude_profile,omitempty"`
	AnalysisModel     string   `json:"analysis_model"`
	ExecutionModel    string   `json:"execution_model,omitempty"`
	AgentPreset       string   `json:"agent_preset"`
	ItemThreshold     int      `json:"item_threshold"`
	MaxWaitSeconds    int      `json:"max_wait_seconds"`
	ReconcileSeconds  int      `json:"reconcile_seconds"`
	Concurrency       int      `json:"concurrency"`
	Status            string   `json:"status"`
	DegradedReason    string   `json:"degraded_reason,omitempty"`
	Version           int      `json:"version"`
	BootstrapAt       string   `json:"bootstrap_at"`
	LastStartedAt     string   `json:"last_started_at,omitempty"`
	LastStoppedAt     string   `json:"last_stopped_at,omitempty"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
}

const runtimeConfigColumns = "id,name,channel_id,route_ids,delivery_route_id,owner_principal_id,owner_id_type,owner_id_value,claude_profile,analysis_model,execution_model,agent_preset,item_threshold,max_wait_seconds,reconcile_seconds,concurrency,status,degraded_reason,version,bootstrap_at,last_started_at,last_stopped_at,created_at,updated_at,application_mode,context_channel_id,agent_capabilities,memory_scope,agent_bash,external_actions,analysis_concurrency,analysis_timeout_seconds,execution_timeout_seconds,review_timeout_seconds"

func scanRuntimeConfig(row scanner) (RuntimeConfig, error) {
	var c RuntimeConfig
	var routes, capabilities string
	err := row.Scan(&c.ID, &c.Name, &c.ChannelID, &routes, &c.DeliveryRouteID, &c.OwnerPrincipalID,
		&c.OwnerIDType, &c.OwnerIDValue, &c.ClaudeProfile, &c.AnalysisModel, &c.ExecutionModel, &c.AgentPreset,
		&c.ItemThreshold, &c.MaxWaitSeconds, &c.ReconcileSeconds, &c.Concurrency, &c.Status,
		&c.DegradedReason, &c.Version, &c.BootstrapAt, &c.LastStartedAt, &c.LastStoppedAt, &c.CreatedAt, &c.UpdatedAt,
		&c.ApplicationMode, &c.ContextChannelID, &capabilities, &c.MemoryScope, &c.AgentBash, &c.ExternalActions, &c.AnalysisConcurrency, &c.AnalysisTimeoutSeconds, &c.ExecutionTimeoutSeconds, &c.ReviewTimeoutSeconds)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal([]byte(routes), &c.RouteIDs); err != nil {
		return c, err
	}
	c.CompletionPolicy = RuntimeCompletionPolicy(c)
	c.ExecutionConcurrency = c.Concurrency
	if c.ApplicationMode == "proactive" {
		// The legacy NOT NULL foreign key is retained as a storage anchor only.
		// Background consumers have no automatic delivery destination.
		c.DeliveryRouteID = ""
	}
	return c, json.Unmarshal([]byte(capabilities), &c.AgentCapabilities)
}

// RuntimeCompletionPolicy controls runtime-generated replies, not explicitly
// requested, independently audited Agent communication actions.
func RuntimeCompletionPolicy(c RuntimeConfig) string {
	if c.ApplicationMode == "proactive" {
		return "record_only"
	}
	return "reply_to_trigger"
}

func ReadRuntime(ctx context.Context, q Queryer, value string) (RuntimeConfig, error) {
	c, err := scanRuntimeConfig(q.QueryRowContext(ctx, "SELECT "+runtimeConfigColumns+" FROM runtime_configs WHERE id=? OR name=?", value, value))
	if errors.Is(err, sql.ErrNoRows) {
		return c, Fail("not_found", "runtime %q not found", value)
	}
	return c, err
}

func RuntimeList(ctx context.Context, q Queryer) ([]RuntimeConfig, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+runtimeConfigColumns+" FROM runtime_configs ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RuntimeConfig{}
	for rows.Next() {
		c, err := scanRuntimeConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func normalizeRuntimeInput(in *RuntimeConfigInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if !channelName.MatchString(in.Name) {
		return Fail("invalid_input", "runtime name must be lowercase letters, digits and dashes")
	}
	if len(in.RouteIDs) == 0 {
		return Fail("invalid_input", "route_ids must name at least one monitored conversation")
	}
	seen := map[string]bool{}
	for _, id := range in.RouteIDs {
		if !identifier.MatchString(id) || seen[id] {
			return Fail("invalid_input", "route_ids must contain unique stable route IDs")
		}
		seen[id] = true
	}
	if in.Owner.IDType == "" || !identifier.MatchString(in.Owner.IDValue) {
		return Fail("invalid_input", "owner requires a stable id_type and id_value")
	}
	if in.ApplicationMode == "" {
		in.ApplicationMode = "proactive"
	}
	if !contains([]string{"proactive", "direct", "group_mention"}, in.ApplicationMode) {
		return Fail("invalid_input", "application_mode must be proactive, direct or group_mention")
	}
	if in.ApplicationMode != "proactive" && !identifier.MatchString(in.DeliveryRouteID) {
		return Fail("invalid_input", "interactive runtimes require delivery_route_id")
	}
	if in.AgentBash == nil {
		enabled := in.ApplicationMode != "group_mention"
		in.AgentBash = &enabled
	}
	if in.MemoryScope == "" {
		in.MemoryScope = "owner_authorized"
	}
	if !contains([]string{"owner_authorized", "conversation_published"}, in.MemoryScope) {
		return Fail("invalid_input", "memory_scope must be owner_authorized or conversation_published")
	}
	if in.ExternalActions == "" {
		in.ExternalActions = "owner_confirmation"
		if in.ApplicationMode == "direct" && *in.AgentBash {
			in.ExternalActions = "owner_request"
		} else if in.ApplicationMode == "proactive" {
			in.ExternalActions = "owner_delegated"
		}
	}
	if !contains([]string{"owner_confirmation", "owner_request", "owner_delegated"}, in.ExternalActions) {
		return Fail("invalid_input", "external_actions must be owner_confirmation, owner_request or owner_delegated")
	}
	if in.ExternalActions == "owner_request" && in.ApplicationMode != "direct" {
		return Fail("denied", "owner_request is allowed only for verified owner direct chat")
	}
	if in.ExternalActions == "owner_delegated" && in.ApplicationMode != "proactive" {
		return Fail("denied", "owner_delegated is allowed only for a proactive owner consumer")
	}
	seenCapabilities := map[string]bool{}
	for _, capability := range in.AgentCapabilities {
		if !contains([]string{"conversation_history_read", "memory_read", "artifact_create", "local_read", "local_write", "local_test"}, capability) || seenCapabilities[capability] {
			return Fail("invalid_input", "agent_capabilities contains an unsupported or duplicate capability")
		}
		seenCapabilities[capability] = true
	}
	if in.AnalysisModel == "" {
		in.AnalysisModel = "haiku"
	}
	if in.ClaudeProfile != "" && !profileName.MatchString(in.ClaudeProfile) {
		return Fail("invalid_input", "claude_profile must be a shell alias name")
	}
	if in.AnalysisModel == "profile" && in.ClaudeProfile == "" {
		return Fail("invalid_input", "analysis_model profile requires claude_profile")
	}
	if in.ExecutionModel == "profile" && in.ClaudeProfile == "" {
		return Fail("invalid_input", "execution_model profile requires claude_profile")
	}
	if in.AgentPreset == "" {
		in.AgentPreset = "claude-default"
	}
	if in.ItemThreshold == 0 {
		in.ItemThreshold = 20
	}
	if in.MaxWaitSeconds == 0 {
		in.MaxWaitSeconds = 30
	}
	if in.ReconcileSeconds == 0 {
		in.ReconcileSeconds = 300
	}
	if in.Concurrency == 0 {
		if in.ExecutionConcurrency == 0 {
			in.Concurrency = 1
		}
	}
	if err := in.Scheduling.Normalize(in.Concurrency); err != nil {
		return err
	}
	in.Concurrency = in.ExecutionConcurrency
	if in.ItemThreshold < 1 || in.ItemThreshold > 100 || in.MaxWaitSeconds < 1 || in.MaxWaitSeconds > 86400 || in.ReconcileSeconds < 10 || in.ReconcileSeconds > 86400 {
		return Fail("invalid_input", "threshold must be 1..100, max wait 1..86400s and reconciliation 10..86400s")
	}
	return nil
}

func (tx *Tx) ConfigureRuntime(ctx context.Context, in RuntimeConfigInput) (RuntimeConfig, error) {
	if err := normalizeRuntimeInput(&in); err != nil {
		return RuntimeConfig{}, err
	}
	channel, err := ReadChannel(ctx, tx.Conn, in.Channel)
	if err != nil {
		return RuntimeConfig{}, err
	}
	if in.ApplicationMode == "proactive" {
		// Preserve the old column without requiring an outbound conversation.
		in.DeliveryRouteID = in.RouteIDs[0]
	}
	var delivery Route
	for _, id := range append(append([]string{}, in.RouteIDs...), in.DeliveryRouteID) {
		r, err := ReadRoute(ctx, tx.Conn, id)
		if err != nil {
			return RuntimeConfig{}, err
		}
		if r.ChannelID != channel.ID || r.Status != "active" {
			return RuntimeConfig{}, Fail("denied", "route %s is not active on channel %s", id, channel.Name)
		}
		// Every route a group_mention runtime processes can end up bound to a
		// runtime task and later used for delivery (see PrepareTaskDelivery),
		// so each one must independently satisfy the same safe-reply
		// contract as the delivery route, not just the default group.
		if in.ApplicationMode == "group_mention" && contains(in.RouteIDs, id) {
			if r.ConversationType != "group" || r.Mode != "assistant" || r.SendPolicy != "reply_to_trigger" || !contains(r.Triggers, "mention") {
				return RuntimeConfig{}, Fail("denied", "group_mention route %s must be an application group route with mention trigger and reply_to_trigger delivery", id)
			}
		}
		if id == in.DeliveryRouteID {
			delivery = r
		}
	}
	if in.ApplicationMode == "group_mention" {
		if channel.Kind != ChannelDingTalkApp || delivery.ConversationType != "group" || delivery.Mode != "assistant" || delivery.SendPolicy != "reply_to_trigger" || !contains(delivery.Triggers, "mention") {
			return RuntimeConfig{}, Fail("denied", "group_mention requires an application group route with mention trigger and reply_to_trigger delivery")
		}
		if !contains(in.RouteIDs, delivery.ID) {
			return RuntimeConfig{}, Fail("invalid_input", "group_mention delivery route must also be its processing route")
		}
		if in.ContextChannel != "" {
			history, historyErr := ReadChannel(ctx, tx.Conn, in.ContextChannel)
			if historyErr != nil || history.Kind != ChannelDwsPersonal || history.Tenant != channel.Tenant {
				return RuntimeConfig{}, Fail("denied", "group_mention requires an explicit same-enterprise DWS context channel")
			}
			historyRoute, routeErr := RouteFor(ctx, tx.Conn, history.ID, delivery.ConversationID)
			if routeErr != nil || historyRoute.WorkspaceID != delivery.WorkspaceID || historyRoute.Mode == "ignore" {
				return RuntimeConfig{}, Fail("denied", "group_mention context must be bound to the same group and workspace on DWS")
			}
			in.ContextChannel = history.ID
		}
		in.ItemThreshold = 1
		in.MemoryScope = "conversation_published"
		if in.AgentCapabilities == nil {
			in.AgentCapabilities = []string{"conversation_history_read", "memory_read", "artifact_create"}
		}
	} else {
		if in.ApplicationMode == "direct" && (delivery.ConversationType != "direct" || delivery.SendPolicy != "dispatch_only") {
			return RuntimeConfig{}, Fail("denied", "owner runtime delivery route must be a direct conversation with dispatch_only enabled")
		}
		in.ContextChannel = ""
		if in.AgentCapabilities == nil {
			in.AgentCapabilities = []string{"conversation_history_read", "memory_read", "artifact_create", "local_read", "local_write", "local_test"}
		}
	}
	owner, weak, err := tx.PrincipalFor(ctx, channel.Tenant, in.Owner)
	if err != nil {
		return RuntimeConfig{}, err
	}
	if weak {
		return RuntimeConfig{}, Fail("denied", "runtime owner identity must be verified")
	}
	if in.ApplicationMode == "direct" {
		candidate := RuntimeConfig{ChannelID: channel.ID, RouteIDs: in.RouteIDs, DeliveryRouteID: in.DeliveryRouteID, OwnerPrincipalID: owner, OwnerIDType: in.Owner.IDType, OwnerIDValue: in.Owner.IDValue, ApplicationMode: "direct"}
		for _, routeID := range in.RouteIDs {
			if _, err := RuntimeOwnerDirectProcessingRoute(ctx, tx.Conn, candidate, routeID); err != nil {
				return RuntimeConfig{}, err
			}
		}
	}
	if channel.Kind == ChannelDwsPersonal && in.Owner.IDType != "user_id" && in.Owner.IDType != "staff_id" {
		return RuntimeConfig{}, Fail("invalid_input", "dws runtime owner must use a stable user_id or staff_id")
	}
	now := Now()
	current, err := ReadRuntime(ctx, tx.Conn, in.Name)
	if err == nil {
		onlyScheduling := schedulingOnlyChange(current, in, channel.ID, owner)
		if current.Status == "running" && !onlyScheduling {
			return current, Fail("conflict", "pause or stop runtime %s before changing its configuration", current.Name)
		}
		if in.ExpectedVersion < 1 || in.ExpectedVersion != current.Version {
			return current, Fail("conflict", "expected runtime version %d, current %d", in.ExpectedVersion, current.Version)
		}
		if !onlyScheduling {
			if err = tx.InvalidateRuntimeConfigWork(ctx, current.ID); err != nil {
				return current, err
			}
		}
		_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET channel_id=?,route_ids=?,delivery_route_id=?,owner_principal_id=?,owner_id_type=?,owner_id_value=?,claude_profile=?,analysis_model=?,execution_model=?,agent_preset=?,item_threshold=?,max_wait_seconds=?,reconcile_seconds=?,concurrency=?,application_mode=?,context_channel_id=?,agent_capabilities=?,memory_scope=?,agent_bash=?,external_actions=?,degraded_reason='',version=version+1,updated_at=? WHERE id=? AND version=?",
			channel.ID, JSON(in.RouteIDs), in.DeliveryRouteID, owner, in.Owner.IDType, in.Owner.IDValue,
			in.ClaudeProfile, in.AnalysisModel, in.ExecutionModel, in.AgentPreset, in.ItemThreshold, in.MaxWaitSeconds, in.ReconcileSeconds,
			in.Concurrency, in.ApplicationMode, in.ContextChannel, JSON(in.AgentCapabilities), in.MemoryScope, *in.AgentBash, in.ExternalActions, now, current.ID, current.Version)
		if err != nil {
			return current, err
		}
		if err = tx.saveScheduling(ctx, current.ID, in.Scheduling); err != nil {
			return current, err
		}
		_, err = tx.Audit(ctx, "runtime.configure", in.Name, []Change{{ObjectType: "runtime", ObjectID: current.ID, Before: current.Version, After: current.Version + 1}})
		if err != nil {
			return current, err
		}
		return ReadRuntime(ctx, tx.Conn, current.ID)
	}
	if ErrorCode(err) != "not_found" {
		return RuntimeConfig{}, err
	}
	c := RuntimeConfig{ID: NewID(), Name: in.Name, ChannelID: channel.ID, RouteIDs: in.RouteIDs,
		DeliveryRouteID: in.DeliveryRouteID, OwnerPrincipalID: owner, OwnerIDType: in.Owner.IDType,
		OwnerIDValue: in.Owner.IDValue, ClaudeProfile: in.ClaudeProfile, AnalysisModel: in.AnalysisModel, ExecutionModel: in.ExecutionModel,
		AgentPreset: in.AgentPreset, ItemThreshold: in.ItemThreshold, MaxWaitSeconds: in.MaxWaitSeconds,
		ApplicationMode: in.ApplicationMode, ContextChannelID: in.ContextChannel, AgentCapabilities: in.AgentCapabilities, MemoryScope: in.MemoryScope, AgentBash: *in.AgentBash, ExternalActions: in.ExternalActions,
		ReconcileSeconds: in.ReconcileSeconds, Concurrency: in.Concurrency, Status: "configured", Version: 1,
		BootstrapAt: now, CreatedAt: now, UpdatedAt: now}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_configs("+runtimeConfigColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		c.ID, c.Name, c.ChannelID, JSON(c.RouteIDs), c.DeliveryRouteID, c.OwnerPrincipalID, c.OwnerIDType,
		c.OwnerIDValue, c.ClaudeProfile, c.AnalysisModel, c.ExecutionModel, c.AgentPreset, c.ItemThreshold, c.MaxWaitSeconds,
		c.ReconcileSeconds, c.Concurrency, c.Status, c.DegradedReason, c.Version, c.BootstrapAt, c.LastStartedAt,
		c.LastStoppedAt, c.CreatedAt, c.UpdatedAt, c.ApplicationMode, c.ContextChannelID, JSON(c.AgentCapabilities), c.MemoryScope, c.AgentBash, c.ExternalActions, in.AnalysisConcurrency, in.AnalysisTimeoutSeconds, in.ExecutionTimeoutSeconds, in.ReviewTimeoutSeconds)
	if err != nil {
		return c, err
	}
	_, err = tx.Audit(ctx, "runtime.configure", in.Name, objectChange("runtime", c.ID))
	if err != nil {
		return c, err
	}
	return ReadRuntime(ctx, tx.Conn, c.ID)
}

func (tx *Tx) SetRuntimeStatus(ctx context.Context, value, status, reason string) (RuntimeConfig, error) {
	c, err := ReadRuntime(ctx, tx.Conn, value)
	if err != nil {
		return c, err
	}
	if !contains([]string{"configured", "running", "paused", "stopped", "degraded"}, status) {
		return c, Fail("invalid_input", "invalid runtime status %q", status)
	}
	now, started, stopped, bootstrap := Now(), c.LastStartedAt, c.LastStoppedAt, c.BootstrapAt
	if status == "running" {
		if len(c.RouteIDs) == 0 {
			return c, Fail("denied", "runtime has no admitted processing routes")
		}
		if c.LastStartedAt == "" {
			bootstrap = now
		}
		started = now
		reason = ""
	}
	if status == "stopped" {
		stopped = now
	}
	if status != "degraded" {
		reason = ""
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET status=?,degraded_reason=?,bootstrap_at=?,last_started_at=?,last_stopped_at=?,updated_at=? WHERE id=?", status, reason, bootstrap, started, stopped, now, c.ID)
	if err != nil {
		return c, err
	}
	_, err = tx.Audit(ctx, "runtime."+status, reason, objectChange("runtime", c.ID))
	if err != nil {
		return c, err
	}
	return ReadRuntime(ctx, tx.Conn, c.ID)
}

// SyncRuntimeGroupRoutes makes the runtime's processing set match the currently
// active groups. Existing routes and their evidence remain stored; ignored
// routes keep their policy and are never restored to the processing set.
func (tx *Tx) SyncRuntimeGroupRoutes(ctx context.Context, value string, conversationIDs []string) (RuntimeConfig, int, error) {
	c, err := ReadRuntime(ctx, tx.Conn, value)
	if err != nil {
		return c, 0, err
	}
	channelValue, err := ReadChannel(ctx, tx.Conn, c.ChannelID)
	if err != nil {
		return c, 0, err
	}
	if channelValue.Kind != ChannelDwsPersonal || len(c.RouteIDs) == 0 {
		return c, 0, nil
	}
	base, err := ReadRoute(ctx, tx.Conn, c.RouteIDs[0])
	if err != nil {
		return c, 0, err
	}
	existing := map[string]bool{}
	active := map[string]bool{}
	for _, route := range channelValue.Routes {
		existing[route.ConversationID] = true
	}
	added := 0
	for _, id := range conversationIDs {
		if !identifier.MatchString(id) {
			return c, added, Fail("invalid_input", "discovered group contains an invalid conversation ID")
		}
		active[id] = true
		if existing[id] {
			continue
		}
		if _, err = tx.AddRoute(ctx, channelValue.ID, RouteInput{ConversationID: id, ConversationType: "group", Workspace: base.WorkspaceID, Mode: "collect"}); err != nil {
			return c, added, err
		}
		existing[id] = true
		added++
	}
	channelValue, err = ReadChannel(ctx, tx.Conn, channelValue.ID)
	if err != nil {
		return c, added, err
	}
	routeIDs := []string{}
	for _, route := range channelValue.Routes {
		if route.Status == "active" && route.ConversationType == "group" && route.Mode != "ignore" && active[route.ConversationID] {
			routeIDs = append(routeIDs, route.ID)
		}
	}
	if JSON(routeIDs) == JSON(c.RouteIDs) {
		return c, added, nil
	}
	c.Version++
	c.RouteIDs = routeIDs
	c.UpdatedAt = Now()
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET route_ids=?,version=?,updated_at=? WHERE id=?", JSON(routeIDs), c.Version, c.UpdatedAt, c.ID); err != nil {
		return c, added, err
	}
	_, err = tx.Audit(ctx, "runtime.groups.sync", "synchronize groups active in the last 30 days", objectChange("runtime", c.ID))
	return c, added, err
}

// SyncGroupMentionRoutes makes a group_mention runtime's route_ids match the
// DWS-discovered set of groups active in the last 30 days that still contain
// the target robot. The application Stream channel remains the only receive
// and send path; the DWS channel supplied here is read-only discovery input.
// For every qualifying DWS conversation this creates or reuses a same
// conversation, same workspace assistant route on the runtime's own
// application channel, fixed to triggers:[mention], audience_policy=
// conversation, memory_policy=explicit_only and send_policy=reply_to_trigger.
// An app or DWS route marked ignore is excluded from the processing set but
// never restored, widened or deleted; a group that drops out of the active
// set is removed from route_ids while its route and audit trail stay intact.
func (tx *Tx) SyncGroupMentionRoutes(ctx context.Context, value string, dwsConversationIDs []string) (RuntimeConfig, int, error) {
	c, err := ReadRuntime(ctx, tx.Conn, value)
	if err != nil {
		return c, 0, err
	}
	if c.ApplicationMode != "group_mention" || c.ContextChannelID == "" {
		return c, 0, nil
	}
	appChannel, err := ReadChannel(ctx, tx.Conn, c.ChannelID)
	if err != nil {
		return c, 0, err
	}
	if appChannel.Kind != ChannelDingTalkApp {
		return c, 0, nil
	}
	dwsChannel, err := ReadChannel(ctx, tx.Conn, c.ContextChannelID)
	if err != nil {
		return c, 0, err
	}
	appByConversation := map[string]Route{}
	for _, route := range appChannel.Routes {
		appByConversation[route.ConversationID] = route
	}
	dwsByConversation := map[string]Route{}
	for _, route := range dwsChannel.Routes {
		dwsByConversation[route.ConversationID] = route
	}
	active := map[string]bool{}
	added := 0
	for _, id := range dwsConversationIDs {
		if !identifier.MatchString(id) {
			return c, added, Fail("invalid_input", "discovered group contains an invalid conversation ID")
		}
		dwsRoute, bound := dwsByConversation[id]
		if !bound || dwsRoute.Status != "active" || dwsRoute.Mode == "ignore" {
			// Not yet bound on the DWS side, or explicitly excluded there; never
			// guess a workspace or create an app route without that binding.
			continue
		}
		if existing, hasApp := appByConversation[id]; hasApp {
			if existing.Mode == "ignore" {
				continue
			}
			active[id] = true
			continue
		}
		created, addErr := tx.AddRoute(ctx, appChannel.ID, RouteInput{ConversationID: id, ConversationType: "group",
			Workspace: dwsRoute.WorkspaceID, Mode: "assistant", Triggers: []string{"mention"},
			AudiencePolicy: "conversation", MemoryPolicy: "explicit_only", SendPolicy: "reply_to_trigger"})
		if addErr != nil {
			return c, added, addErr
		}
		appByConversation[id] = created
		active[id] = true
		added++
	}
	appChannel, err = ReadChannel(ctx, tx.Conn, appChannel.ID)
	if err != nil {
		return c, added, err
	}
	routeIDs := []string{}
	for _, route := range appChannel.Routes {
		if route.Status == "active" && route.ConversationType == "group" && route.Mode == "assistant" &&
			route.SendPolicy == "reply_to_trigger" && contains(route.Triggers, "mention") && active[route.ConversationID] {
			routeIDs = append(routeIDs, route.ID)
		}
	}
	if JSON(routeIDs) == JSON(c.RouteIDs) {
		return c, added, nil
	}
	c.Version++
	c.RouteIDs = routeIDs
	c.UpdatedAt = Now()
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET route_ids=?,version=?,updated_at=? WHERE id=?", JSON(routeIDs), c.Version, c.UpdatedAt, c.ID); err != nil {
		return c, added, err
	}
	_, err = tx.Audit(ctx, "runtime.groups.sync", "synchronize group_mention routes active in the last 30 days", objectChange("runtime", c.ID))
	return c, added, err
}

type RuntimeMessage struct {
	ID           string        `json:"id"`
	Revision     int           `json:"revision"`
	SentAt       string        `json:"sent_at"`
	Sender       string        `json:"sender_principal"`
	SelfAuthored bool          `json:"self_authored"`
	Addressed    bool          `json:"addressed,omitempty"`
	Body         string        `json:"body"`
	Quote        *MessageQuote `json:"quote,omitempty"`
	SourceID     string        `json:"source_id,omitempty"`
	FragmentID   string        `json:"fragment_id,omitempty"`
	SHA256       string        `json:"sha256,omitempty"`
	Evidence     []Evidence    `json:"evidence,omitempty"`
	// Provider addressing is needed only for DingTalk reply affordances. It is
	// deliberately excluded from model payloads and structured logs.
	ProviderMessageID string `json:"-"`
	ConversationID    string `json:"-"`
}

type RuntimeSyncResult struct {
	Discovered      int `json:"discovered"`
	Pending         int `json:"pending"`
	Context         int `json:"context"`
	Revised         int `json:"revised"`
	Recalled        int `json:"recalled"`
	Staled          int `json:"staled_tasks"`
	WaitingReceipts int `json:"waiting_receipts"`
}

func (tx *Tx) SyncRuntimeMessages(ctx context.Context, value string) (RuntimeSyncResult, error) {
	var out RuntimeSyncResult
	processed := 0
	c, err := ReadRuntime(ctx, tx.Conn, value)
	if err != nil {
		return out, err
	}
	for _, routeID := range runtimeProcessingRouteIDs(c) {
		if processed >= 100 {
			break
		}
		r, err := ReadRoute(ctx, tx.Conn, routeID)
		if err != nil {
			return out, err
		}
		if r.Mode == "ignore" {
			if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_message_states SET state='ignored',processed_at=? WHERE runtime_id=? AND route_id=? AND state='pending'", Now(), c.ID, routeID); err != nil {
				return out, err
			}
			continue
		}
		// Revisit only messages whose derived runtime state can change. Scanning the
		// complete conversation while holding BEGIN IMMEDIATE made old, busy DWS
		// histories monopolize SQLite's single writer and starved bot receipts,
		// receiver leases, and Agent completion records.
		retained := retainedMessagePredicate("m")
		rows, err := tx.Conn.QueryContext(ctx, `SELECT m.id,m.current_revision,m.sent_at,m.created_at,
CASE WHEN `+retained+` THEN m.availability ELSE 'expired' END,
m.self_authored,m.addressed,m.context_only,m.provider_message_id
FROM messages m
LEFT JOIN runtime_message_states rms ON rms.runtime_id=? AND rms.message_id=m.id
WHERE m.channel_id=? AND m.conversation_id=? AND (
 rms.message_id IS NULL OR
 rms.revision<>m.current_revision OR
 rms.state='waiting_receipt' OR
 (rms.state='pending' AND m.context_only=1) OR
 (m.availability<>'available' AND rms.state<>'recalled') OR
 (NOT (`+retained+`) AND rms.state<>'recalled')
)
ORDER BY m.created_at,m.rowid LIMIT ?`, c.ID, c.ChannelID, r.ConversationID, 100-processed)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var id, sentAt, receivedAt, availability, providerMessageID string
			var revision, self, addressed, contextOnly int
			waitingReceipt := false
			if err = rows.Scan(&id, &revision, &sentAt, &receivedAt, &availability, &self, &addressed, &contextOnly, &providerMessageID); err != nil {
				rows.Close()
				return out, err
			}
			processed++
			if c.ApplicationMode == "proactive" {
				echo, echoErr := RuntimeAgentMessageEcho(ctx, tx.Conn, c.ChannelID, r.ConversationID, providerMessageID)
				if echoErr != nil {
					rows.Close()
					return out, echoErr
				}
				if echo {
					contextOnly = 1
				} else if self == 1 {
					waitingReceipt, echoErr = RuntimeAgentMessageAwaitingReceipt(ctx, tx.Conn, c.ChannelID, r.ConversationID, sentAt)
					if echoErr != nil {
						rows.Close()
						return out, echoErr
					}
				}
			}
			// Approval tokens are control messages, including when a separate
			// direct runtime scans before the proactive runtime claims approval.
			if r.ConversationType == "direct" || c.ApplicationMode == "group_mention" {
				control, controlErr := runtimeMessageIsConfirmation(ctx, tx.Conn, id, c.ApplicationMode == "group_mention")
				if controlErr != nil {
					rows.Close()
					return out, controlErr
				}
				if control {
					contextOnly = 1
				}
			}
			var priorRev int
			var priorState string
			err = tx.Conn.QueryRowContext(ctx, "SELECT revision,state FROM runtime_message_states WHERE runtime_id=? AND message_id=?", c.ID, id).Scan(&priorRev, &priorState)
			if errors.Is(err, sql.ErrNoRows) {
				state := "pending"
				if waitingReceipt {
					state = "waiting_receipt"
				}
				if c.ApplicationMode == "group_mention" && (addressed == 0 || self == 1) {
					state = "ignored"
				}
				if contextOnly == 1 || sentAt == "" || atOrBefore(sentAt, c.BootstrapAt) {
					state = "context"
				}
				if availability != "available" {
					state = "recalled"
				}
				_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_message_states(runtime_id,route_id,message_id,revision,state,first_seen_at) VALUES(?,?,?,?,?,?)", c.ID, routeID, id, revision, state, receivedAt)
				if err != nil {
					rows.Close()
					return out, err
				}
				out.Discovered++
				if state == "pending" {
					out.Pending++
				} else if state == "context" {
					out.Context++
				} else if state == "waiting_receipt" {
					out.WaitingReceipts++
				} else if state == "recalled" {
					out.Recalled++
				}
				continue
			}
			if err != nil {
				rows.Close()
				return out, err
			}
			nextState := priorState
			changed := priorRev != revision
			if availability != "available" {
				nextState = "recalled"
			} else if changed || priorState == "waiting_receipt" || (contextOnly == 1 && priorState == "pending") {
				nextState = "pending"
				if waitingReceipt {
					nextState = "waiting_receipt"
				}
				if c.ApplicationMode == "group_mention" && (addressed == 0 || self == 1) {
					nextState = "ignored"
				}
				if contextOnly == 1 || sentAt == "" || atOrBefore(sentAt, c.BootstrapAt) {
					nextState = "context"
				}
			}
			if nextState != priorState || changed {
				_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_message_states SET revision=?,state=?,batch_id='',processed_at='',first_seen_at=?,retry_count=0,next_run_at='' WHERE runtime_id=? AND message_id=?", revision, nextState, Now(), c.ID, id)
				if err != nil {
					rows.Close()
					return out, err
				}
				status := "stale"
				linkQuery := "SELECT task_id FROM runtime_task_messages WHERE message_id=? AND revision=?"
				linkArgs := []any{id, priorRev}
				if availability != "available" {
					status = "cancelled"
					out.Recalled++
					linkQuery = "SELECT task_id FROM runtime_task_messages WHERE message_id=?"
					linkArgs = []any{id}
				} else {
					out.Revised++
				}
				taskRows, queryErr := tx.Conn.QueryContext(ctx, linkQuery, linkArgs...)
				if queryErr != nil {
					rows.Close()
					return out, queryErr
				}
				taskIDs := []string{}
				for taskRows.Next() {
					var taskID string
					if queryErr = taskRows.Scan(&taskID); queryErr != nil {
						taskRows.Close()
						rows.Close()
						return out, queryErr
					}
					taskIDs = append(taskIDs, taskID)
				}
				taskRows.Close()
				for _, taskID := range taskIDs {
					res, updateErr := tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status=?,version=version+1,updated_at=? WHERE id=? AND status<>'cancelled'", status, Now(), taskID)
					if updateErr != nil {
						rows.Close()
						return out, updateErr
					}
					n, _ := res.RowsAffected()
					if n > 0 {
						if updateErr = tx.staleRuntimeTaskWork(ctx, taskID); updateErr != nil {
							rows.Close()
							return out, updateErr
						}
						out.Staled++
					}
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func atOrBefore(value, cutoff string) bool {
	valueTime, valueErr := time.Parse(time.RFC3339Nano, value)
	cutoffTime, cutoffErr := time.Parse(time.RFC3339Nano, cutoff)
	if valueErr != nil || cutoffErr != nil {
		return true
	}
	return !valueTime.After(cutoffTime)
}

type RuntimeBatch struct {
	ID        string           `json:"id"`
	RuntimeID string           `json:"runtime_id"`
	RouteID   string           `json:"route_id"`
	Workspace string           `json:"workspace_id"`
	Status    string           `json:"status"`
	Model     string           `json:"model"`
	Mode      string           `json:"application_mode,omitempty"`
	Digest    string           `json:"input_digest"`
	Messages  []RuntimeMessage `json:"messages"`
	Context   []RuntimeMessage `json:"context"`
	Matters   []RuntimeMatter  `json:"existing_matters,omitempty"`
	CreatedAt string           `json:"created_at"`
}

type RuntimeMatter struct {
	CanonicalKey string `json:"canonical_key"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion,omitempty"`
	Version      int    `json:"version"`
}

func runtimeMessages(ctx context.Context, q Queryer, ids []string) ([]RuntimeMessage, error) {
	out := []RuntimeMessage{}
	for _, id := range ids {
		var m RuntimeMessage
		var self, addressed int
		err := q.QueryRowContext(ctx, `SELECT m.id,m.current_revision,m.sent_at,m.sender_principal,m.self_authored,m.addressed,m.provider_message_id,m.conversation_id,
CASE WHEN m.availability='available' THEN mr.body ELSE '' END,
CASE WHEN m.availability='available' THEN coalesce(so.source_id,'') ELSE '' END,
CASE WHEN m.availability='available' THEN coalesce((SELECT id FROM fragments WHERE source_id=so.source_id ORDER BY rowid LIMIT 1),'') ELSE '' END,
CASE WHEN m.availability='available' THEN coalesce((SELECT digest FROM fragments WHERE source_id=so.source_id ORDER BY rowid LIMIT 1),'') ELSE '' END
FROM messages m JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision
LEFT JOIN source_origins so ON so.message_id=m.id AND so.revision=m.current_revision WHERE m.id=?`, id).Scan(&m.ID, &m.Revision, &m.SentAt, &m.Sender, &self, &addressed, &m.ProviderMessageID, &m.ConversationID, &m.Body, &m.SourceID, &m.FragmentID, &m.SHA256)
		if err != nil {
			return nil, err
		}
		m.SelfAuthored = self == 1
		m.Addressed = addressed == 1
		if m.SourceID != "" {
			expired, expiryErr := sourceRawExpired(ctx, q, m.SourceID)
			if expiryErr != nil {
				return nil, expiryErr
			}
			if expired {
				m.Body = ""
				m.SourceID = ""
				m.FragmentID = ""
				m.SHA256 = ""
			}
		}
		if m.SourceID != "" {
			rows, queryErr := q.QueryContext(ctx, "SELECT id,digest FROM fragments WHERE source_id=? ORDER BY rowid", m.SourceID)
			if queryErr != nil {
				return nil, queryErr
			}
			for rows.Next() {
				var evidence Evidence
				evidence.SourceID = m.SourceID
				if queryErr = rows.Scan(&evidence.FragmentID, &evidence.SHA256); queryErr != nil {
					rows.Close()
					return nil, queryErr
				}
				m.Evidence = append(m.Evidence, evidence)
			}
			if queryErr = rows.Err(); queryErr != nil {
				rows.Close()
				return nil, queryErr
			}
			rows.Close()
		}
		m.Quote, err = runtimeMessageQuote(ctx, q, m.ID, m.Revision)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (tx *Tx) ClaimRuntimeBatch(ctx context.Context, value string, at time.Time) (RuntimeBatch, error) {
	var out RuntimeBatch
	c, err := ReadRuntime(ctx, tx.Conn, value)
	if err != nil {
		return out, err
	}
	if c.Status != "running" {
		return out, nil
	}
	if !inMemoryCapacity(ctx) {
		if available, e := PoolAvailable(ctx, tx.Conn, c, "analysis"); e != nil || !available {
			return out, e
		}
	}
	routes, e := eligibleAnalysisRoutes(ctx, tx.Conn, c, at)
	if e != nil {
		return out, e
	}
	cutoff := at.UTC().Add(-time.Duration(c.MaxWaitSeconds) * time.Second).Format(time.RFC3339Nano)
	for _, routeID := range routes {
		route, routeErr := ReadRoute(ctx, tx.Conn, routeID)
		if routeErr != nil {
			return out, routeErr
		}
		if route.Mode == "ignore" {
			continue
		}
		if c.ApplicationMode == "proactive" {
			// A verified @ callback belongs to the active group Agent even if
			// its sync tick has not run yet. The DWS copy is context, not a
			// second owner task. A late proof cannot retract an earlier batch.
			if _, err = tx.Conn.ExecContext(ctx, `UPDATE runtime_message_states SET state='linked_duplicate',processed_at=?
WHERE runtime_id=? AND route_id=? AND state='pending' AND EXISTS (
 SELECT 1 FROM verified_message_associations a
 JOIN messages bot ON bot.id=CASE WHEN a.first_message_id=runtime_message_states.message_id THEN a.second_message_id ELSE a.first_message_id END
 JOIN runtime_configs g ON g.channel_id=bot.channel_id AND g.application_mode='group_mention' AND g.status='running'
 JOIN channel_routes gr ON gr.channel_id=g.channel_id AND gr.conversation_id=bot.conversation_id
 WHERE (a.first_message_id=runtime_message_states.message_id OR a.second_message_id=runtime_message_states.message_id)
 AND bot.addressed=1 AND bot.context_only=0 AND bot.availability='available'
 AND gr.status='active' AND gr.mode='assistant' AND gr.send_policy='reply_to_trigger'
 AND (gr.id=g.delivery_route_id OR gr.id IN (SELECT value FROM json_each(g.route_ids))))`, Now(), c.ID, routeID); err != nil {
				return out, err
			}
		}
		// A verified cross-transport association can only be processed once.
		// Unverified messages remain independent; no text/time matching occurs.
		if _, err = tx.Conn.ExecContext(ctx, `UPDATE runtime_message_states SET state='linked_duplicate',processed_at=?
WHERE runtime_id=? AND route_id=? AND state='pending' AND EXISTS (
 SELECT 1 FROM verified_message_associations a WHERE
 (a.first_message_id=runtime_message_states.message_id OR a.second_message_id=runtime_message_states.message_id)
 AND a.claimed_message_id<>'' AND (a.claimed_message_id<>runtime_message_states.message_id OR a.claimed_runtime_id<>runtime_message_states.runtime_id))`, Now(), c.ID, routeID); err != nil {
			return out, err
		}
		var count int
		var earliest string
		err = tx.Conn.QueryRowContext(ctx, "SELECT count(*),coalesce(min(first_seen_at),'') FROM runtime_message_states WHERE runtime_id=? AND route_id=? AND state='pending'", c.ID, routeID).Scan(&count, &earliest)
		if err != nil {
			return out, err
		}
		threshold := c.ItemThreshold
		if c.ApplicationMode == "direct" || c.ApplicationMode == "group_mention" || routeID == c.DeliveryRouteID {
			threshold = 1
		}
		if count == 0 || (count < threshold && earliest > cutoff) {
			continue
		}
		limit := c.ItemThreshold
		if c.ApplicationMode == "direct" || c.ApplicationMode == "group_mention" {
			limit = 1
		}
		rows, err := tx.Conn.QueryContext(ctx, "SELECT message_id FROM runtime_message_states WHERE runtime_id=? AND route_id=? AND state='pending' ORDER BY first_seen_at,rowid LIMIT ?", c.ID, routeID, limit)
		if err != nil {
			return out, err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return out, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		messages, err := runtimeMessages(ctx, tx.Conn, ids)
		if err != nil {
			return out, err
		}
		out = RuntimeBatch{ID: NewID(), RuntimeID: c.ID, RouteID: routeID, Workspace: route.WorkspaceID, Status: "analyzing", Model: c.AnalysisModel, Mode: c.ApplicationMode, Messages: messages, CreatedAt: Now()}
		if c.ApplicationMode == "group_mention" {
			out.Model = "local-mention-routing"
		}
		for _, m := range messages {
			// SQLite's write transaction serializes competing runtime workers.
			// The pair can be claimed only after platform lookup inserted it.
			if _, err = tx.Conn.ExecContext(ctx, `UPDATE verified_message_associations SET claimed_message_id=?,claimed_runtime_id=?,claimed_batch_id=?,claimed_at=?
WHERE (first_message_id=? OR second_message_id=?) AND claimed_message_id=''`, m.ID, c.ID, out.ID, Now(), m.ID, m.ID); err != nil {
				return out, err
			}
		}
		out.Digest = Digest(messages)
		if err = tx.claimWork(ctx, c, "analysis", out.ID, routeID, "", 0); err != nil {
			return out, err
		}
		_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_batches(id,runtime_id,route_id,status,input_digest,message_count,model,created_at,started_at) VALUES(?,?,?,?,?,?,?,?,?)", out.ID, out.RuntimeID, out.RouteID, out.Status, out.Digest, len(messages), out.Model, out.CreatedAt, out.CreatedAt)
		if err != nil {
			return out, err
		}
		if _, err = tx.Conn.ExecContext(ctx, `UPDATE runtime_batches SET attempt=1+coalesce((SELECT max(retry_count) FROM runtime_message_states WHERE runtime_id=? AND route_id=? AND state='pending'),0),retry_key=? WHERE id=?`, c.ID, routeID, out.Digest, out.ID); err != nil {
			return out, err
		}
		for i, m := range messages {
			if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_batch_messages VALUES(?,?,?,?)", out.ID, m.ID, m.Revision, i); err != nil {
				return out, err
			}
			if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_message_states SET state='batched',batch_id=? WHERE runtime_id=? AND message_id=? AND revision=? AND state='pending'", out.ID, c.ID, m.ID, m.Revision); err != nil {
				return out, err
			}
		}
		contextChannel, contextConversation := c.ChannelID, route.ConversationID
		if c.ApplicationMode == "group_mention" && c.ContextChannelID != "" {
			contextChannel = c.ContextChannelID
		}
		contextRows, err := tx.Conn.QueryContext(ctx, `SELECT m.id FROM messages m WHERE m.channel_id=? AND m.conversation_id=? AND m.availability='available'
AND m.id NOT IN (SELECT message_id FROM runtime_batch_messages WHERE batch_id=?)
AND NOT EXISTS (SELECT 1 FROM verified_message_associations a JOIN runtime_batch_messages rbm
 ON rbm.message_id=CASE WHEN a.first_message_id=m.id THEN a.second_message_id ELSE a.first_message_id END
 WHERE rbm.batch_id=? AND (a.first_message_id=m.id OR a.second_message_id=m.id))
ORDER BY m.sent_at DESC,m.id DESC LIMIT 30`, contextChannel, contextConversation, out.ID, out.ID)
		if err != nil {
			return out, err
		}
		contextIDs := []string{}
		for contextRows.Next() {
			var id string
			if err = contextRows.Scan(&id); err != nil {
				contextRows.Close()
				return out, err
			}
			contextIDs = append(contextIDs, id)
		}
		contextRows.Close()
		for i, j := 0, len(contextIDs)-1; i < j; i, j = i+1, j-1 {
			contextIDs[i], contextIDs[j] = contextIDs[j], contextIDs[i]
		}
		out.Context, err = runtimeMessages(ctx, tx.Conn, contextIDs)
		if err != nil {
			return out, err
		}
		matterRows, matterErr := tx.Conn.QueryContext(ctx, "SELECT id,canonical_key,title,status,result_summary,version FROM runtime_tasks WHERE runtime_id=? AND route_id=? ORDER BY updated_at DESC,id LIMIT 50", c.ID, routeID)
		if matterErr != nil {
			return out, matterErr
		}
		matterTasks := []RuntimeTask{}
		for matterRows.Next() {
			var t RuntimeTask
			if matterErr = matterRows.Scan(&t.ID, &t.CanonicalKey, &t.Title, &t.Status, &t.ResultSummary, &t.Version); matterErr != nil {
				matterRows.Close()
				return out, matterErr
			}
			matterTasks = append(matterTasks, t)
		}
		matterErr = matterRows.Err()
		matterRows.Close()
		if matterErr != nil {
			return out, matterErr
		}
		for i := range matterTasks {
			t := &matterTasks[i]
			if matterErr = redactReadRuntimeTask(ctx, tx.Conn, t); matterErr != nil {
				return out, matterErr
			}
			out.Matters = append(out.Matters, RuntimeMatter{CanonicalKey: t.CanonicalKey, Title: t.Title, Status: t.Status, Conclusion: t.ResultSummary, Version: t.Version})
		}
		return out, err
	}
	return out, nil
}

func runtimeProcessingRouteIDs(c RuntimeConfig) []string {
	ids := append([]string{}, c.RouteIDs...)
	if c.ApplicationMode == "proactive" || c.DeliveryRouteID == "" {
		return ids
	}
	for _, id := range ids {
		if id == c.DeliveryRouteID {
			return ids
		}
	}
	return append(ids, c.DeliveryRouteID)
}

type RuntimeDecision struct {
	Kind               string   `json:"kind"`
	CanonicalKey       string   `json:"canonical_key,omitempty"`
	Title              string   `json:"title,omitempty"`
	Instructions       string   `json:"instructions,omitempty"`
	MessageIDs         []string `json:"message_ids,omitempty"`
	NeedsClarification bool     `json:"needs_clarification,omitempty"`
}
type RuntimeAnalysis struct {
	Decisions []RuntimeDecision `json:"decisions"`
}

func (tx *Tx) CompleteRuntimeBatch(ctx context.Context, batch RuntimeBatch, analysis RuntimeAnalysis) ([]RuntimeTask, error) {
	if err := CheckWorkLease(ctx, tx.Conn, batch.ID); err != nil {
		return nil, err
	}
	if len(analysis.Decisions) > 100 {
		return nil, Fail("invalid_input", "runtime analysis exceeds 100 decisions")
	}
	var storedStatus, storedDigest string
	if err := tx.Conn.QueryRowContext(ctx, "SELECT status,input_digest FROM runtime_batches WHERE id=? AND runtime_id=?", batch.ID, batch.RuntimeID).Scan(&storedStatus, &storedDigest); err != nil {
		return nil, err
	}
	if storedStatus != "analyzing" || storedDigest != batch.Digest {
		return nil, Fail("conflict", "runtime batch changed while analysis was running")
	}
	allowed := map[string]int{}
	route, routeErr := ReadRoute(ctx, tx.Conn, batch.RouteID)
	if routeErr != nil {
		return nil, routeErr
	}
	for _, m := range batch.Messages {
		allowed[m.ID] = m.Revision
		var currentRevision int
		var availability string
		if err := tx.Conn.QueryRowContext(ctx, "SELECT current_revision,CASE WHEN "+retainedMessagePredicate("m")+" THEN availability ELSE 'expired' END FROM messages m WHERE id=?", m.ID).Scan(&currentRevision, &availability); err != nil {
			return nil, err
		}
		if currentRevision != m.Revision || availability != "available" {
			return nil, Fail("conflict", "runtime batch message changed while analysis was running")
		}
	}
	tasks := []RuntimeTask{}
	for _, d := range analysis.Decisions {
		if !contains([]string{"task", "update", "cancel", "complete", "memory", "context"}, d.Kind) {
			return nil, Fail("invalid_input", "unsupported runtime decision %q", d.Kind)
		}
		if d.Kind == "context" {
			continue
		}
		if len([]rune(d.CanonicalKey)) > 200 || len([]rune(d.Title)) > 500 || len([]rune(d.Instructions)) > 20000 {
			return nil, Fail("invalid_input", "runtime decision exceeds its size limit")
		}
		if d.CanonicalKey == "" {
			d.CanonicalKey = Hash([]byte(strings.ToLower(strings.TrimSpace(d.Title))))
		}
		if strings.TrimSpace(d.CanonicalKey) == "" {
			return nil, Fail("invalid_input", "actionable decisions require a canonical key")
		}
		if batch.Mode == "proactive" && route.ConversationType == "direct" {
			prefix := "direct:" + batch.RouteID + ":"
			if !strings.HasPrefix(d.CanonicalKey, prefix) {
				if len(d.CanonicalKey) > 150 {
					d.CanonicalKey = Hash([]byte(d.CanonicalKey))
				}
				d.CanonicalKey = prefix + d.CanonicalKey
			}
		}
		if len(d.MessageIDs) == 0 {
			for _, m := range batch.Messages {
				d.MessageIDs = append(d.MessageIDs, m.ID)
			}
		}
		for _, id := range d.MessageIDs {
			if _, ok := allowed[id]; !ok {
				return nil, Fail("denied", "decision cites a message outside its batch")
			}
		}
		var existing RuntimeTask
		err := scanRuntimeTask(tx.Conn.QueryRowContext(ctx, "SELECT "+runtimeTaskColumns+" FROM runtime_tasks WHERE runtime_id=? AND canonical_key=?", batch.RuntimeID, d.CanonicalKey), &existing)
		if d.Kind == "cancel" || d.Kind == "complete" || (err == nil && existing.Status == "completed" && d.Kind == "task") {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			status := "completed"
			if d.Kind == "cancel" {
				status = "cancelled"
			}
			changed := existing.Status != status
			if changed {
				if err = tx.staleRuntimeTaskWork(ctx, existing.ID); err != nil {
					return nil, err
				}
				summary := existing.ResultSummary
				if d.Kind == "complete" && d.Instructions != "" {
					summary = d.Instructions
				}
				_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status=?,version=version+1,result_summary=?,updated_at=? WHERE id=?", status, summary, Now(), existing.ID)
				if err != nil {
					return nil, err
				}
				existing.Status = status
				existing.Version++
				existing.ResultSummary = summary
			}
			for _, id := range d.MessageIDs {
				if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_task_messages(task_id,message_id,revision,role) VALUES(?,?,?,'followup') ON CONFLICT(task_id,message_id) DO UPDATE SET revision=excluded.revision", existing.ID, id, allowed[id]); err != nil {
					return nil, err
				}
			}
			if changed && d.Kind == "complete" {
				tasks = append(tasks, existing)
			}
			continue
		}
		status := "pending"
		if d.NeedsClarification && batch.Mode != "proactive" {
			status = "clarification"
		}
		kind := d.Kind
		if kind == "update" {
			kind = existing.Kind
		}
		if kind == "" {
			kind = "task"
		}
		if strings.TrimSpace(d.Title) == "" || strings.TrimSpace(d.Instructions) == "" {
			return nil, Fail("invalid_input", "actionable decisions require title and instructions")
		}
		if errors.Is(err, sql.ErrNoRows) {
			existing = RuntimeTask{ID: NewID(), RuntimeID: batch.RuntimeID, RouteID: batch.RouteID, CanonicalKey: d.CanonicalKey, Kind: kind, Title: d.Title, Instructions: d.Instructions, Status: status, NeedsClarification: d.NeedsClarification, Version: 1, CreatedAt: Now(), UpdatedAt: Now()}
			if status == "clarification" {
				existing.ResultSummary = "需要补充信息：" + d.Instructions
			}
			_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,kind,title,instructions,status,needs_clarification,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", existing.ID, existing.RuntimeID, existing.RouteID, existing.CanonicalKey, existing.Kind, existing.Title, existing.Instructions, existing.Status, boolInt(existing.NeedsClarification), existing.Version, existing.CreatedAt, existing.UpdatedAt)
			if err == nil && existing.ResultSummary != "" {
				_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET result_summary=? WHERE id=?", existing.ResultSummary, existing.ID)
			}
		} else if err == nil {
			if err = tx.staleRuntimeTaskWork(ctx, existing.ID); err != nil {
				return nil, err
			}
			existing.Version++
			existing.RouteID, existing.Kind, existing.Title, existing.Instructions, existing.Status, existing.NeedsClarification, existing.UpdatedAt = batch.RouteID, kind, d.Title, d.Instructions, status, d.NeedsClarification, Now()
			if status == "clarification" {
				existing.ResultSummary = "需要补充信息：" + d.Instructions
			}
			_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET route_id=?,kind=?,title=?,instructions=?,status=?,needs_clarification=?,version=?,result='',result_summary='',error_code='',updated_at=? WHERE id=?", existing.RouteID, existing.Kind, existing.Title, existing.Instructions, existing.Status, boolInt(existing.NeedsClarification), existing.Version, existing.UpdatedAt, existing.ID)
			if err == nil && existing.ResultSummary != "" {
				_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET result_summary=? WHERE id=?", existing.ResultSummary, existing.ID)
			}
		}
		if err != nil {
			return nil, err
		}
		for _, id := range d.MessageIDs {
			_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_task_messages(task_id,message_id,revision,role) VALUES(?,?,?,'trigger') ON CONFLICT(task_id,message_id) DO UPDATE SET revision=excluded.revision", existing.ID, id, allowed[id])
			if err != nil {
				return nil, err
			}
		}
		tasks = append(tasks, existing)
	}
	now := Now()
	if _, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_batches SET status='completed',output=?,finished_at=? WHERE id=? AND status='analyzing'", JSON(analysis), now, batch.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_message_states SET state='processed',processed_at=? WHERE runtime_id=? AND batch_id=?", now, batch.RuntimeID, batch.ID); err != nil {
		return nil, err
	}
	if err := tx.ReleaseWorkLease(ctx, batch.ID); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (tx *Tx) FailRuntimeBatch(ctx context.Context, id, code string) error {
	if code == "" {
		code = "internal"
	}
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_batches SET status='failed',error_code=?,finished_at=? WHERE id=? AND status='analyzing'", code, Now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	var managed int
	if err = tx.Conn.QueryRowContext(ctx, `SELECT count(*) FROM runtime_work_leases WHERE id=?`, id).Scan(&managed); err != nil {
		return err
	}
	if managed > 0 {
		var attempt int
		if err = tx.Conn.QueryRowContext(ctx, `SELECT attempt FROM runtime_batches WHERE id=?`, id).Scan(&attempt); err != nil {
			return err
		}
		var mode string
		if err = tx.Conn.QueryRowContext(ctx, `SELECT application_mode FROM runtime_configs c JOIN runtime_batches b ON b.runtime_id=c.id WHERE b.id=?`, id).Scan(&mode); err != nil {
			return err
		}
		retry := code == "unavailable" || code == "analysis_timeout" || code == "runtime_restarted"
		state, next := "analysis_failed", ""
		if code == "conflict" {
			state = "pending"
		} else if retry && attempt < 3 {
			state = "pending"
			if !(mode == "group_mention" && code == "runtime_restarted") {
				delay := 5 * time.Second
				if attempt > 1 {
					delay = 30 * time.Second
				}
				delay += time.Duration(time.Now().UnixNano()%1000) * time.Millisecond
				next = time.Now().UTC().Add(delay).Format(time.RFC3339Nano)
			}
		}
		if _, err = tx.Conn.ExecContext(ctx, `UPDATE runtime_batches SET next_run_at=? WHERE id=?`, next, id); err != nil {
			return err
		}
		_, err = tx.Conn.ExecContext(ctx, `UPDATE runtime_message_states SET state=?,batch_id='',retry_count=retry_count+1,next_run_at=? WHERE batch_id=? AND state='batched'`, state, next, id)
		return err
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_message_states SET state='pending',batch_id='' WHERE batch_id=? AND state='batched'", id)
	return err
}

type RuntimeAction struct {
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Payload string `json:"payload"`
}
type RuntimeAttemptResult struct {
	Result    string          `json:"result"`
	Summary   string          `json:"summary"`
	Artifacts []string        `json:"artifacts,omitempty"`
	ToolKinds []string        `json:"tool_kinds,omitempty"`
	Usage     map[string]any  `json:"usage,omitempty"`
	Actions   []RuntimeAction `json:"pending_actions,omitempty"`
	Candidate *CandidateInput `json:"candidate,omitempty"`
}
type RuntimeTask struct {
	MemoryStatus       string                 `json:"memory_status,omitempty"`
	MemoryErrorCode    string                 `json:"memory_error_code,omitempty"`
	ID                 string                 `json:"id"`
	RuntimeID          string                 `json:"runtime_id"`
	RouteID            string                 `json:"route_id"`
	CanonicalKey       string                 `json:"canonical_key"`
	Kind               string                 `json:"kind"`
	Title              string                 `json:"title"`
	Instructions       string                 `json:"instructions"`
	Status             string                 `json:"status"`
	NeedsClarification bool                   `json:"needs_clarification"`
	Version            int                    `json:"version"`
	CandidateID        string                 `json:"candidate_id,omitempty"`
	Result             string                 `json:"result,omitempty"`
	ResultSummary      string                 `json:"result_summary,omitempty"`
	ErrorCode          string                 `json:"error_code,omitempty"`
	AnalysisDurationMS int64                  `json:"analysis_duration_ms,omitempty"`
	AnalysisModel      string                 `json:"analysis_model,omitempty"`
	CreatedAt          string                 `json:"created_at"`
	UpdatedAt          string                 `json:"updated_at"`
	Messages           []RuntimeMessage       `json:"messages,omitempty"`
	Associations       []MessageAssociation   `json:"verified_message_associations,omitempty"`
	Attempts           []RuntimeAttempt       `json:"attempts,omitempty"`
	Actions            []RuntimePendingAction `json:"pending_actions,omitempty"`
	Communications     []RuntimeMessageAction `json:"communications,omitempty"`
	Resume             *RuntimeTaskResume     `json:"resume,omitempty"`
}

const runtimeTaskColumns = "id,runtime_id,route_id,canonical_key,kind,title,instructions,status,needs_clarification,version,candidate_id,result,result_summary,error_code,created_at,updated_at,memory_status,memory_error_code"

func scanRuntimeTask(row scanner, t *RuntimeTask) error {
	var clarify int
	err := row.Scan(&t.ID, &t.RuntimeID, &t.RouteID, &t.CanonicalKey, &t.Kind, &t.Title, &t.Instructions, &t.Status, &clarify, &t.Version, &t.CandidateID, &t.Result, &t.ResultSummary, &t.ErrorCode, &t.CreatedAt, &t.UpdatedAt, &t.MemoryStatus, &t.MemoryErrorCode)
	t.NeedsClarification = clarify == 1
	return err
}

type RuntimeAttempt struct {
	ID                   string              `json:"id"`
	TaskID               string              `json:"task_id"`
	TaskVersion          int                 `json:"task_version"`
	AppliedConfigVersion int                 `json:"applied_config_version,omitempty"`
	Status               string              `json:"status"`
	Model                string              `json:"model"`
	PresetName           string              `json:"preset_name,omitempty"`
	PresetCommit         string              `json:"preset_commit,omitempty"`
	WorkspaceDir         string              `json:"workspace_dir,omitempty"`
	Summary              string              `json:"summary,omitempty"`
	Artifacts            []string            `json:"artifacts,omitempty"`
	ToolKinds            []string            `json:"tool_kinds,omitempty"`
	Usage                map[string]any      `json:"usage,omitempty"`
	ErrorCode            string              `json:"error_code,omitempty"`
	StartedAt            string              `json:"started_at"`
	FinishedAt           string              `json:"finished_at,omitempty"`
	AgentSession         RuntimeAgentSession `json:"agent_session"`
}
type RuntimePendingAction struct {
	ID                 string                 `json:"id"`
	TaskID             string                 `json:"task_id"`
	TaskVersion        int                    `json:"task_version"`
	Kind               string                 `json:"kind"`
	Target             string                 `json:"target"`
	Payload            string                 `json:"payload"`
	PayloadDigest      string                 `json:"payload_digest"`
	Status             string                 `json:"status"`
	ConfirmedBy        string                 `json:"confirmed_by,omitempty"`
	ConfirmationOrigin string                 `json:"confirmation_origin,omitempty"`
	CreatedAt          string                 `json:"created_at"`
	UpdatedAt          string                 `json:"updated_at"`
	Attempts           []RuntimeActionAttempt `json:"attempts,omitempty"`
}

type RuntimeActionAttempt struct {
	ID                   string   `json:"id"`
	ActionID             string   `json:"action_id"`
	TaskID               string   `json:"task_id"`
	TaskVersion          int      `json:"task_version"`
	AppliedConfigVersion int      `json:"applied_config_version,omitempty"`
	Status               string   `json:"status"`
	Model                string   `json:"model,omitempty"`
	WorkspaceDir         string   `json:"workspace_dir,omitempty"`
	Result               string   `json:"result,omitempty"`
	Summary              string   `json:"summary,omitempty"`
	ToolKinds            []string `json:"tool_kinds,omitempty"`
	ErrorCode            string   `json:"error_code,omitempty"`
	StartedAt            string   `json:"started_at"`
	FinishedAt           string   `json:"finished_at,omitempty"`
}

func ReadRuntimeTask(ctx context.Context, q Queryer, id string) (RuntimeTask, error) {
	var t RuntimeTask
	err := scanRuntimeTask(q.QueryRowContext(ctx, "SELECT "+runtimeTaskColumns+" FROM runtime_tasks WHERE id=?", id), &t)
	if errors.Is(err, sql.ErrNoRows) {
		return t, Fail("not_found", "runtime task %q not found", id)
	}
	if err != nil {
		return t, err
	}
	if err = redactReadRuntimeTask(ctx, q, &t); err != nil {
		return t, err
	}
	rows, err := q.QueryContext(ctx, "SELECT message_id FROM runtime_task_messages WHERE task_id=? ORDER BY rowid", id)
	if err != nil {
		return t, err
	}
	ids := []string{}
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			rows.Close()
			return t, err
		}
		ids = append(ids, v)
	}
	rows.Close()
	t.Messages, err = runtimeMessages(ctx, q, ids)
	if err != nil {
		return t, err
	}
	seenAssociations := map[string]bool{}
	for _, messageID := range ids {
		association, associationErr := ReadMessageAssociation(ctx, q, messageID)
		if ErrorCode(associationErr) == "not_found" {
			continue
		}
		if associationErr != nil {
			return t, associationErr
		}
		if !seenAssociations[association.ID] {
			t.Associations = append(t.Associations, association)
			seenAssociations[association.ID] = true
		}
	}
	var analysisStarted, analysisFinished string
	err = q.QueryRowContext(ctx, `SELECT rb.model,rb.started_at,rb.finished_at
FROM runtime_batches rb
JOIN runtime_batch_messages rbm ON rbm.batch_id=rb.id
JOIN runtime_task_messages rtm ON rtm.message_id=rbm.message_id
WHERE rtm.task_id=? AND rb.status='completed' AND rb.finished_at<>''
ORDER BY rb.finished_at DESC,rb.id DESC LIMIT 1`, id).Scan(&t.AnalysisModel, &analysisStarted, &analysisFinished)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return t, err
	}
	if err == nil {
		started, startErr := time.Parse(time.RFC3339Nano, analysisStarted)
		finished, finishErr := time.Parse(time.RFC3339Nano, analysisFinished)
		if startErr == nil && finishErr == nil && finished.After(started) {
			t.AnalysisDurationMS = finished.Sub(started).Milliseconds()
		}
	}
	var resume string
	if err = q.QueryRowContext(ctx, "SELECT resume FROM runtime_tasks WHERE id=?", id).Scan(&resume); err != nil {
		return t, err
	}
	if resume != "" {
		if err = json.Unmarshal([]byte(resume), &t.Resume); err != nil {
			return t, err
		}
		if t.Resume != nil && t.Resume.TaskVersion != t.Version {
			t.Resume = nil
		}
	}
	rows, err = q.QueryContext(ctx, "SELECT id,task_id,task_version,applied_config_version,status,model,preset_name,preset_commit,workspace_dir,summary,artifacts,tool_kinds,usage,error_code,started_at,finished_at,agent_session FROM runtime_attempts WHERE task_id=? ORDER BY started_at,id", id)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var a RuntimeAttempt
		var artifacts, tools, usage, agentSession string
		if err = rows.Scan(&a.ID, &a.TaskID, &a.TaskVersion, &a.AppliedConfigVersion, &a.Status, &a.Model, &a.PresetName, &a.PresetCommit, &a.WorkspaceDir, &a.Summary, &artifacts, &tools, &usage, &a.ErrorCode, &a.StartedAt, &a.FinishedAt, &agentSession); err != nil {
			rows.Close()
			return t, err
		}
		_ = json.Unmarshal([]byte(artifacts), &a.Artifacts)
		_ = json.Unmarshal([]byte(tools), &a.ToolKinds)
		_ = json.Unmarshal([]byte(usage), &a.Usage)
		if err = json.Unmarshal([]byte(agentSession), &a.AgentSession); err != nil {
			rows.Close()
			return t, err
		}
		t.Attempts = append(t.Attempts, a)
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, "SELECT id,task_id,task_version,kind,target,payload,payload_digest,status,confirmed_by,confirmation_origin,created_at,updated_at FROM runtime_pending_actions WHERE task_id=? ORDER BY created_at", id)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var a RuntimePendingAction
		if err = rows.Scan(&a.ID, &a.TaskID, &a.TaskVersion, &a.Kind, &a.Target, &a.Payload, &a.PayloadDigest, &a.Status, &a.ConfirmedBy, &a.ConfirmationOrigin, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return t, err
		}
		t.Actions = append(t.Actions, a)
	}
	if err = rows.Err(); err != nil {
		return t, err
	}
	rows.Close()
	for i := range t.Actions {
		attemptRows, queryErr := q.QueryContext(ctx, "SELECT id,action_id,task_id,task_version,applied_config_version,status,model,workspace_dir,result,summary,tool_kinds,error_code,started_at,finished_at FROM runtime_action_attempts WHERE action_id=? ORDER BY started_at", t.Actions[i].ID)
		if queryErr != nil {
			return t, queryErr
		}
		for attemptRows.Next() {
			var a RuntimeActionAttempt
			var tools string
			if err = attemptRows.Scan(&a.ID, &a.ActionID, &a.TaskID, &a.TaskVersion, &a.AppliedConfigVersion, &a.Status, &a.Model, &a.WorkspaceDir, &a.Result, &a.Summary, &tools, &a.ErrorCode, &a.StartedAt, &a.FinishedAt); err != nil {
				attemptRows.Close()
				return t, err
			}
			_ = json.Unmarshal([]byte(tools), &a.ToolKinds)
			t.Actions[i].Attempts = append(t.Actions[i].Attempts, a)
		}
		if err = attemptRows.Err(); err != nil {
			attemptRows.Close()
			return t, err
		}
		attemptRows.Close()
	}
	if err = redactReadRuntimeTask(ctx, q, &t); err != nil {
		return t, err
	}
	t.Communications, err = RuntimeMessageActions(ctx, q, t.ID)
	return t, err
}

func (tx *Tx) staleRuntimeTaskWork(ctx context.Context, id string) error {
	now := Now()
	if _, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_reviews SET status='failed',error_code='memory_version_conflict',finished_at=? WHERE task_id=? AND status IN ('pending','running')", now, id); err != nil {
		return err
	}
	if _, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET memory_status='',memory_error_code='',candidate_id='' WHERE id=?", id); err != nil {
		return err
	}
	if _, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='stale',updated_at=? WHERE task_id=? AND status IN ('pending','confirmed')", now, id); err != nil {
		return err
	}
	if _, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='unknown',updated_at=? WHERE task_id=? AND status='executing'", now, id); err != nil {
		return err
	}
	if _, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_action_attempts SET status='unknown',error_code='task_changed',finished_at=? WHERE task_id=? AND status='running'", now, id); err != nil {
		return err
	}
	_, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_attempts SET status='stale',error_code='task_changed',finished_at=? WHERE task_id=? AND status='running'", now, id)
	return err
}

func RuntimeTaskList(ctx context.Context, q Queryer, runtimeValue, status string, limit int) ([]RuntimeTask, error) {
	if limit < 1 || limit > 500 {
		return nil, Fail("invalid_input", "limit must be 1..500")
	}
	c, err := ReadRuntime(ctx, q, runtimeValue)
	if err != nil {
		return nil, err
	}
	query := "SELECT " + runtimeTaskColumns + " FROM runtime_tasks WHERE runtime_id=?"
	args := []any{c.ID}
	if status != "" {
		query += " AND status=?"
		args = append(args, status)
	}
	query += " ORDER BY updated_at DESC,id LIMIT ?"
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RuntimeTask{}
	for rows.Next() {
		var t RuntimeTask
		if err = scanRuntimeTask(rows, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err = redactReadRuntimeTask(ctx, q, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (tx *Tx) ClaimRuntimeTask(ctx context.Context, value, attemptID, model, preset, commit, workspace string) (RuntimeTask, RuntimeAttempt, error) {
	var t RuntimeTask
	var a RuntimeAttempt
	c, err := ReadRuntime(ctx, tx.Conn, value)
	if err != nil {
		return t, a, err
	}
	if c.Status != "running" {
		return t, a, nil
	}
	var active int
	if err = tx.Conn.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM runtime_attempts ra JOIN runtime_tasks rt ON rt.id=ra.task_id WHERE rt.runtime_id=? AND ra.status='running')+
(SELECT count(*) FROM runtime_action_attempts aa JOIN runtime_tasks rt ON rt.id=aa.task_id WHERE rt.runtime_id=? AND aa.status='running')`, c.ID, c.ID).Scan(&active); err != nil {
		return t, a, err
	}
	if (c.ApplicationMode != "proactive" && c.ApplicationMode != "group_mention" && active > 0) || (c.ApplicationMode != "group_mention" && active >= c.Concurrency) {
		return t, a, nil
	}
	if !inMemoryCapacity(ctx) {
		if available, e := PoolAvailable(ctx, tx.Conn, c, "execution"); e != nil || !available {
			return t, a, e
		}
	}
	err = scanRuntimeTask(tx.Conn.QueryRowContext(ctx, "SELECT "+runtimeTaskColumns+` FROM runtime_tasks WHERE runtime_id=? AND status='pending'
AND route_id IN (SELECT value FROM json_each(?))
AND EXISTS (SELECT 1 FROM channel_routes r WHERE r.id=runtime_tasks.route_id AND r.channel_id=? AND r.status='active' AND r.mode<>'ignore')
AND NOT EXISTS(SELECT 1 FROM runtime_work_leases l WHERE l.task_id=runtime_tasks.id AND l.released=0)
AND (kind<>'memory' OR (SELECT count(*) FROM runtime_work_leases l JOIN runtime_tasks mt ON mt.id=l.task_id WHERE l.released=0 AND mt.kind='memory')<2)
ORDER BY CASE kind WHEN 'memory' THEN 1 ELSE 0 END,
max(updated_at,coalesce((SELECT max(a.started_at) FROM runtime_attempts a JOIN runtime_tasks previous ON previous.id=a.task_id WHERE previous.runtime_id=runtime_tasks.runtime_id AND previous.route_id=runtime_tasks.route_id),'')),updated_at,id LIMIT 1`, c.ID, JSON(c.RouteIDs), c.ChannelID), &t)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeTask{}, RuntimeAttempt{}, nil
	}
	if err != nil {
		return t, a, err
	}
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status='running',updated_at=? WHERE id=? AND version=? AND status='pending'", Now(), t.ID, t.Version)
	if err != nil {
		return t, a, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return RuntimeTask{}, RuntimeAttempt{}, nil
	}
	if model == "" {
		model = c.ExecutionModel
	}
	if attemptID == "" {
		attemptID = NewID()
	}
	if err = tx.claimWork(ctx, c, "execution", attemptID, t.RouteID, t.ID, t.Version); err != nil {
		return t, a, err
	}
	applied, err := ReadAppliedConfig(ctx, tx.Conn, 0)
	if err != nil {
		return t, a, err
	}
	a = RuntimeAttempt{ID: attemptID, TaskID: t.ID, TaskVersion: t.Version, AppliedConfigVersion: applied.Version, Status: "running", Model: model, PresetName: preset, PresetCommit: commit, WorkspaceDir: workspace, StartedAt: Now()}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_attempts(id,task_id,task_version,applied_config_version,status,model,preset_name,preset_commit,workspace_dir,input_digest,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", a.ID, a.TaskID, a.TaskVersion, a.AppliedConfigVersion, a.Status, a.Model, a.PresetName, a.PresetCommit, a.WorkspaceDir, Digest(map[string]any{"title": t.Title, "instructions": t.Instructions, "version": t.Version}), a.StartedAt)
	t.Status = "running"
	return t, a, err
}

func (tx *Tx) SetRuntimeAttemptWorkspace(ctx context.Context, id, workspace string) error {
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_attempts SET workspace_dir=? WHERE id=? AND status='running'", workspace, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Fail("conflict", "runtime attempt is no longer running")
	}
	return nil
}

func (tx *Tx) CompleteRuntimeTask(ctx context.Context, taskID string, version int, attemptID string, result RuntimeAttemptResult, candidateID string) (RuntimeTask, error) {
	t, err := ReadRuntimeTask(ctx, tx.Conn, taskID)
	if err != nil {
		return t, err
	}
	if t.Version != version || t.Status != "running" {
		return t, Fail("conflict", "task changed while attempt was running")
	}
	if err = tx.CheckRuntimeAttemptPolicy(ctx, attemptID, taskID, version); err != nil {
		return t, err
	}
	current, err := runtimeTaskMessagesCurrent(ctx, tx.Conn, taskID)
	if err != nil {
		return t, err
	}
	if !current {
		return t, Fail("conflict", "task source messages changed while attempt was running")
	}
	if len([]rune(result.Result)) > 200000 || len([]rune(result.Summary)) > 2000 || len(result.Artifacts) > 32 || len(result.ToolKinds) > 32 || len(result.Actions) > 20 {
		return t, Fail("invalid_input", "runtime result exceeds its size limit")
	}
	status := "completed"
	var prepared int
	if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM runtime_pending_actions WHERE task_id=? AND task_version=? AND status='pending'", taskID, version).Scan(&prepared); err != nil {
		return t, err
	}
	if len(result.Actions)+prepared > 0 {
		status = "awaiting_confirmation"
		config, readErr := ReadRuntime(ctx, tx.Conn, t.RuntimeID)
		if readErr != nil {
			return t, readErr
		}
		if config.ApplicationMode == "proactive" {
			status = "blocked"
			allDestructive := prepared == 0 && len(result.Actions) > 0
			for _, action := range result.Actions {
				if action.Kind != RuntimeDestructiveAction {
					allDestructive = false
				} else if e := validateDestructiveAction(action); e != nil {
					return t, e
				}
			}
			if allDestructive {
				status = "awaiting_confirmation"
			}
		}
	}
	now := Now()
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status=?,candidate_id=?,result=?,result_summary=?,error_code='',updated_at=? WHERE id=? AND version=? AND status='running'", status, candidateID, result.Result, result.Summary, now, taskID, version)
	if err != nil {
		return t, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return t, Fail("conflict", "task changed while attempt was completing")
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_attempts SET status='completed',output=?,summary=?,artifacts=?,tool_kinds=?,usage=?,finished_at=? WHERE id=? AND task_version=?", result.Result, result.Summary, JSON(result.Artifacts), JSON(result.ToolKinds), JSON(result.Usage), now, attemptID, version)
	if err != nil {
		return t, err
	}
	for _, action := range result.Actions {
		if strings.TrimSpace(action.Kind) == "" || strings.TrimSpace(action.Target) == "" || strings.TrimSpace(action.Payload) == "" {
			return t, Fail("invalid_input", "pending actions require kind, target and payload")
		}
		if len([]rune(action.Kind)) > 64 || len([]rune(action.Target)) > 2000 || len([]rune(action.Payload)) > 50000 {
			return t, Fail("invalid_input", "pending action exceeds its size limit")
		}
		id := NewID()
		_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_pending_actions(id,task_id,task_version,kind,target,payload,payload_digest,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'pending',?,?)", id, taskID, version, action.Kind, action.Target, action.Payload, Hash([]byte(action.Payload)), now, now)
		if err != nil {
			return t, err
		}
	}
	if err := tx.ReleaseWorkLease(ctx, attemptID); err != nil {
		return t, err
	}
	return ReadRuntimeTask(ctx, tx.Conn, taskID)
}

func (tx *Tx) FailRuntimeTask(ctx context.Context, taskID string, version int, attemptID, code string) (RuntimeTask, error) {
	if code == "" {
		code = "internal"
	}
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status='failed',error_code=?,updated_at=? WHERE id=? AND version=? AND status='running'", code, Now(), taskID, version)
	if err != nil {
		return RuntimeTask{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return RuntimeTask{}, Fail("conflict", "task changed while attempt was running")
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_attempts SET status='failed',error_code=?,finished_at=? WHERE id=?", code, Now(), attemptID)
	if err != nil {
		return RuntimeTask{}, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_message_actions SET state='unknown',detail=?,updated_at=? WHERE task_id=? AND attempt_id=? AND state='sending'", code, Now(), taskID, attemptID); err != nil {
		return RuntimeTask{}, err
	}
	return ReadRuntimeTask(ctx, tx.Conn, taskID)
}

func (tx *Tx) SetRuntimeTaskStatus(ctx context.Context, id, status string) (RuntimeTask, error) {
	t, err := ReadRuntimeTask(ctx, tx.Conn, id)
	if err != nil {
		return t, err
	}
	switch status {
	case "cancelled":
		if t.Status == "completed" {
			return t, Fail("conflict", "completed task cannot be cancelled")
		}
	case "pending":
		if !contains([]string{"failed", "stale", "cancelled", "clarification", "blocked", "action_failed", "action_unknown"}, t.Status) {
			return t, Fail("conflict", "task %s cannot be retried", t.Status)
		}
		var uncertain int
		if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM runtime_message_actions WHERE task_id=? AND state IN ('sending','unknown')", t.ID).Scan(&uncertain); err != nil {
			return t, err
		}
		if uncertain > 0 {
			return t, Fail("conflict", "resolve the Agent communication outcome before retrying")
		}
	default:
		return t, Fail("invalid_input", "unsupported task transition")
	}
	errorCode := ""
	if status == "cancelled" {
		errorCode = "cancelled"
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status=?,needs_clarification=0,version=version+1,error_code=?,resume='',updated_at=? WHERE id=?", status, errorCode, Now(), id)
	if err != nil {
		return t, err
	}
	if err = tx.staleRuntimeTaskWork(ctx, id); err != nil {
		return t, err
	}
	return ReadRuntimeTask(ctx, tx.Conn, id)
}

func ConfirmationToken(a RuntimePendingAction) string {
	digest := a.PayloadDigest
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return "确认操作 " + a.ID + " " + digest
}

func runtimeTaskMessagesCurrent(ctx context.Context, q Queryer, taskID string) (bool, error) {
	var stale int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_task_messages tm
JOIN messages m ON m.id=tm.message_id
WHERE tm.task_id=? AND (m.current_revision<>tm.revision OR m.availability<>'available' OR NOT (`+retainedMessagePredicate("m")+`))`, taskID).Scan(&stale)
	if err != nil || stale != 0 {
		return false, err
	}
	return RuntimeDirectTurnCurrent(ctx, q, taskID)
}

// ConfirmRuntimeActionFromMessage binds approval to a stored message in the
// task origin group or configured owner direct conversation. Identity claims
// never authorize an action.
func (tx *Tx) ConfirmRuntimeActionFromMessage(ctx context.Context, actionID, messageID string) (RuntimePendingAction, error) {
	var a RuntimePendingAction
	err := tx.Conn.QueryRowContext(ctx, "SELECT id,task_id,task_version,kind,target,payload,payload_digest,status,confirmed_by,confirmation_origin,created_at,updated_at FROM runtime_pending_actions WHERE id=?", actionID).Scan(&a.ID, &a.TaskID, &a.TaskVersion, &a.Kind, &a.Target, &a.Payload, &a.PayloadDigest, &a.Status, &a.ConfirmedBy, &a.ConfirmationOrigin, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, Fail("not_found", "pending action not found")
	}
	if err != nil {
		return a, err
	}
	t, err := ReadRuntimeTask(ctx, tx.Conn, a.TaskID)
	if err != nil {
		return a, err
	}
	c, err := ReadRuntime(ctx, tx.Conn, t.RuntimeID)
	if err != nil {
		return a, err
	}
	if c.ApplicationMode == "proactive" && (t.Status != "awaiting_confirmation" || a.Kind != RuntimeDestructiveAction) {
		return a, Fail("denied", "background approval is only available for a specific destructive proposal")
	}
	var cardCount int
	if err = tx.Conn.QueryRowContext(ctx, `SELECT count(*) FROM outbox o,json_each(CASE WHEN o.format='confirmation_card' THEN o.content ELSE '{}' END,'$.actions') a WHERE o.job_id=? AND o.format='confirmation_card' AND json_extract(a.value,'$.id')=?`, t.ID, a.ID).Scan(&cardCount); err != nil {
		return a, err
	}
	if cardCount > 0 {
		return a, Fail("denied", "this action requires the origin card's owner-only confirmation button")
	}
	admitted, err := runtimeTaskTriggerAdmitted(ctx, tx.Conn, c, t)
	if err != nil {
		return a, err
	}
	if !admitted {
		return a, Fail("conflict", "pending action trigger route left the runtime processing scope")
	}
	routes, err := runtimeConfirmationRoutes(ctx, tx.Conn, c)
	if err != nil {
		return a, err
	}
	var channelID, conversationID, principal, sentAt, availability, body string
	var revision, addressed int
	err = tx.Conn.QueryRowContext(ctx, `SELECT m.channel_id,m.conversation_id,m.sender_principal,m.sent_at,m.availability,m.current_revision,mr.body,m.addressed
FROM messages m JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision WHERE m.id=?`, messageID).Scan(&channelID, &conversationID, &principal, &sentAt, &availability, &revision, &body, &addressed)
	if errors.Is(err, sql.ErrNoRows) {
		return a, Fail("not_found", "confirmation message not found")
	}
	if err != nil {
		return a, err
	}
	var r Route
	for _, candidate := range routes {
		if candidate.ChannelID == channelID && candidate.ConversationID == conversationID && (c.ApplicationMode != "group_mention" || candidate.ID == t.RouteID) {
			r = candidate
			break
		}
	}
	verified, err := runtimeConfirmationSenderCurrent(ctx, tx.Conn, channelID, messageID, c.OwnerPrincipalID)
	if err != nil {
		return a, err
	}
	if r.ID == "" || principal != c.OwnerPrincipalID || availability != "available" || !verified {
		return a, Fail("denied", "confirmation must be an available message from the verified owner in the task origin group or explicitly bound direct inbox")
	}
	if c.ApplicationMode == "proactive" {
		var fresh int
		if err = tx.Conn.QueryRowContext(ctx, `SELECT count(*) FROM messages m WHERE id=? AND (SELECT origin FROM inbox_events e WHERE e.message_id=m.id ORDER BY received_at,e.rowid LIMIT 1)='stream' AND context_only=0 AND julianday(created_at)>=julianday(?)`, messageID, a.CreatedAt).Scan(&fresh); err != nil {
			return a, err
		}
		if fresh != 1 {
			return a, Fail("denied", "destructive approval must be a new live owner message, not historical context")
		}
	}
	if c.ApplicationMode == "group_mention" {
		body = runtimeGroupConfirmationBody(body, addressed != 0)
	}
	sentTime, sentErr := time.Parse(time.RFC3339Nano, sentAt)
	createdTime, createdErr := time.Parse(time.RFC3339Nano, a.CreatedAt)
	if sentErr != nil || createdErr != nil || sentTime.Before(createdTime) || strings.TrimSpace(body) != ConfirmationToken(a) {
		return a, Fail("denied", "confirmation message does not match the current displayed action")
	}
	if a.Status != "pending" || t.Status != "awaiting_confirmation" || a.TaskVersion != t.Version || Hash([]byte(a.Payload)) != a.PayloadDigest {
		return a, Fail("conflict", "pending action is stale or was already handled")
	}
	current, err := runtimeTaskMessagesCurrent(ctx, tx.Conn, t.ID)
	if err != nil {
		return a, err
	}
	if !current {
		return a, Fail("conflict", "pending action source messages changed")
	}
	now := Now()
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='confirmed',confirmed_by=?,confirmation_origin=?,updated_at=? WHERE id=? AND status='pending'", principal, "dingtalk_message:"+messageID, now, a.ID)
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return a, Fail("conflict", "pending action was already handled")
	}
	_, err = tx.Conn.ExecContext(ctx, `INSERT INTO runtime_message_states(runtime_id,route_id,message_id,revision,state,first_seen_at,processed_at)
VALUES(?,?,?,?, 'processed',?,?) ON CONFLICT(runtime_id,message_id) DO UPDATE SET state='processed',revision=excluded.revision,processed_at=excluded.processed_at`, c.ID, r.ID, messageID, revision, now, now)
	if err != nil {
		return a, err
	}
	a.Status, a.ConfirmedBy, a.ConfirmationOrigin, a.UpdatedAt = "confirmed", principal, "dingtalk_message:"+messageID, now
	return a, nil
}

// ProcessRuntimeConfirmations finds exact approval tokens that arrived through
// the task origin group or trusted direct route, without free-form approval.
func (tx *Tx) ProcessRuntimeConfirmations(ctx context.Context, value string) ([]RuntimePendingAction, error) {
	c, err := ReadRuntime(ctx, tx.Conn, value)
	if err != nil {
		return nil, err
	}
	routes, err := runtimeConfirmationRoutes(ctx, tx.Conn, c)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Conn.QueryContext(ctx, `SELECT a.id,a.payload_digest,a.created_at,t.route_id FROM runtime_pending_actions a
JOIN runtime_tasks t ON t.id=a.task_id WHERE t.runtime_id=? AND t.status='awaiting_confirmation' AND t.version=a.task_version AND a.status='pending'
AND t.route_id IN (SELECT value FROM json_each(?))
AND EXISTS (SELECT 1 FROM channel_routes r WHERE r.id=t.route_id AND r.channel_id=? AND r.status='active' AND r.mode<>'ignore')
ORDER BY a.created_at`, c.ID, JSON(c.RouteIDs), c.ChannelID)
	if err != nil {
		return nil, err
	}
	type candidate struct{ id, digest, created, routeID string }
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.id, &item.digest, &item.created, &item.routeID); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := []RuntimePendingAction{}
	for _, item := range candidates {
		digest := item.digest
		if len(digest) > 12 {
			digest = digest[:12]
		}
		token := "确认操作 " + item.id + " " + digest
		var messageID string
		for _, r := range routes {
			if c.ApplicationMode == "group_mention" && r.ID != item.routeID {
				continue
			}
			predicate := "trim(mr.body)=?"
			args := []any{r.ChannelID, r.ConversationID, c.OwnerPrincipalID, item.created, token}
			if c.ApplicationMode == "proactive" {
				predicate += " AND (SELECT origin FROM inbox_events e WHERE e.message_id=m.id ORDER BY received_at,e.rowid LIMIT 1)='stream' AND m.context_only=0 AND julianday(m.created_at)>=julianday(?)"
				args = append(args, item.created)
			}
			if c.ApplicationMode == "group_mention" {
				predicate = "(" + predicate + " OR (m.addressed=1 AND substr(trim(mr.body),1,1)='@' AND instr(trim(mr.body),' ')>1 AND trim(substr(trim(mr.body),instr(trim(mr.body),' ')+1))=?))"
				args = append(args, token)
			}
			err = tx.Conn.QueryRowContext(ctx, `SELECT m.id FROM messages m
JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision
JOIN channels ch ON ch.id=m.channel_id
JOIN identity_aliases ia ON ia.tenant=ch.tenant AND ia.id_type=m.sender_id_type AND ia.id_value=m.sender_id_value
WHERE m.channel_id=? AND m.conversation_id=? AND m.sender_principal=? AND ia.principal_id=m.sender_principal AND ia.verified=1
AND m.availability='available' AND julianday(m.sent_at)>=julianday(?) AND `+predicate+` ORDER BY m.sent_at,m.id LIMIT 1`, args...).Scan(&messageID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return out, err
			}
			break
		}
		if messageID == "" {
			continue
		}
		action, confirmErr := tx.ConfirmRuntimeActionFromMessage(ctx, item.id, messageID)
		if confirmErr != nil {
			return out, confirmErr
		}
		out = append(out, action)
	}
	return out, nil
}

func (tx *Tx) ClaimRuntimeAction(ctx context.Context, value, attemptID, model string) (RuntimePendingAction, RuntimeActionAttempt, error) {
	var action RuntimePendingAction
	var attempt RuntimeActionAttempt
	c, err := ReadRuntime(ctx, tx.Conn, value)
	if err != nil {
		return action, attempt, err
	}
	if c.Status != "running" {
		return action, attempt, nil
	}
	if !inMemoryCapacity(ctx) {
		if available, e := PoolAvailable(ctx, tx.Conn, c, "execution"); e != nil || !available {
			return action, attempt, e
		}
	}
	var active int
	if err = tx.Conn.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM runtime_attempts ra JOIN runtime_tasks rt ON rt.id=ra.task_id WHERE rt.runtime_id=? AND ra.status='running')+
(SELECT count(*) FROM runtime_action_attempts aa JOIN runtime_tasks rt ON rt.id=aa.task_id WHERE rt.runtime_id=? AND aa.status='running')`, c.ID, c.ID).Scan(&active); err != nil {
		return action, attempt, err
	}
	if (c.ApplicationMode != "proactive" && c.ApplicationMode != "group_mention" && active > 0) || (c.ApplicationMode != "group_mention" && active >= c.Concurrency) {
		return action, attempt, nil
	}
	err = tx.Conn.QueryRowContext(ctx, `SELECT a.id,a.task_id,a.task_version,a.kind,a.target,a.payload,a.payload_digest,a.status,a.confirmed_by,a.confirmation_origin,a.created_at,a.updated_at
FROM runtime_pending_actions a JOIN runtime_tasks t ON t.id=a.task_id
WHERE t.runtime_id=? AND t.status='awaiting_confirmation' AND t.version=a.task_version AND a.status='confirmed'
AND NOT EXISTS(SELECT 1 FROM runtime_work_leases l WHERE l.task_id=t.id AND l.released=0)
AND t.route_id IN (SELECT value FROM json_each(?))
AND EXISTS (SELECT 1 FROM channel_routes r WHERE r.id=t.route_id AND r.channel_id=? AND r.status='active' AND r.mode<>'ignore')
ORDER BY a.updated_at,a.id LIMIT 1`, c.ID, JSON(c.RouteIDs), c.ChannelID).Scan(&action.ID, &action.TaskID, &action.TaskVersion, &action.Kind, &action.Target, &action.Payload, &action.PayloadDigest, &action.Status, &action.ConfirmedBy, &action.ConfirmationOrigin, &action.CreatedAt, &action.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimePendingAction{}, RuntimeActionAttempt{}, nil
	}
	if err != nil {
		return action, attempt, err
	}
	if c.ApplicationMode == "proactive" && action.Kind != RuntimeDestructiveAction {
		return action, attempt, Fail("denied", "only a specific destructive proposal can resume through background confirmation")
	}
	if err = destructiveApprovalCurrent(ctx, tx.Conn, action); err != nil {
		return action, attempt, err
	}
	if strings.HasPrefix(action.ConfirmationOrigin, "dingtalk_card:") {
		if err = runtimeCardActionCurrent(ctx, tx.Conn, c, action); err != nil {
			return action, attempt, err
		}
	}
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='executing',updated_at=? WHERE id=? AND status='confirmed'", Now(), action.ID)
	if err != nil {
		return action, attempt, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return RuntimePendingAction{}, RuntimeActionAttempt{}, nil
	}
	if attemptID == "" {
		attemptID = NewID()
	}
	if err = tx.claimWork(ctx, c, "execution", attemptID, "", action.TaskID, action.TaskVersion); err != nil {
		return action, attempt, err
	}
	if model == "" {
		model = c.ExecutionModel
	}
	applied, err := ReadAppliedConfig(ctx, tx.Conn, 0)
	if err != nil {
		return action, attempt, err
	}
	attempt = RuntimeActionAttempt{ID: attemptID, ActionID: action.ID, TaskID: action.TaskID, TaskVersion: action.TaskVersion, AppliedConfigVersion: applied.Version, Status: "running", Model: model, StartedAt: Now()}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_action_attempts(id,action_id,task_id,task_version,applied_config_version,status,model,started_at) VALUES(?,?,?,?,?,?,?,?)", attempt.ID, attempt.ActionID, attempt.TaskID, attempt.TaskVersion, attempt.AppliedConfigVersion, attempt.Status, attempt.Model, attempt.StartedAt)
	action.Status = "executing"
	return action, attempt, err
}

func (tx *Tx) CompleteRuntimeAction(ctx context.Context, actionID, attemptID string, version int, result RuntimeAttemptResult) (RuntimeTask, error) {
	var taskID, actionStatus, attemptStatus string
	var taskVersion int
	err := tx.Conn.QueryRowContext(ctx, `SELECT a.task_id,a.task_version,a.status,aa.status FROM runtime_pending_actions a
JOIN runtime_action_attempts aa ON aa.id=? AND aa.action_id=a.id WHERE a.id=?`, attemptID, actionID).Scan(&taskID, &taskVersion, &actionStatus, &attemptStatus)
	if err != nil {
		return RuntimeTask{}, err
	}
	if taskVersion != version || actionStatus != "executing" || attemptStatus != "running" {
		return RuntimeTask{}, Fail("conflict", "confirmed action changed while it was executing")
	}
	if err = tx.CheckRuntimeActionAttemptPolicy(ctx, attemptID, actionID, version); err != nil {
		return RuntimeTask{}, err
	}
	now := Now()
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_action_attempts SET status='completed',result=?,summary=?,tool_kinds=?,finished_at=? WHERE id=? AND status='running'", result.Result, result.Summary, JSON(result.ToolKinds), now, attemptID); err != nil {
		return RuntimeTask{}, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='executed',updated_at=? WHERE id=? AND status='executing'", now, actionID); err != nil {
		return RuntimeTask{}, err
	}
	var remaining int
	if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM runtime_pending_actions WHERE task_id=? AND task_version=? AND status<>'executed'", taskID, version).Scan(&remaining); err != nil {
		return RuntimeTask{}, err
	}
	if remaining == 0 {
		addition := strings.TrimSpace(result.Result)
		if addition == "" {
			addition = strings.TrimSpace(result.Summary)
		}
		_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status='completed',result=result||CASE WHEN result='' THEN '' ELSE char(10)||char(10) END||?,result_summary=?,updated_at=? WHERE id=? AND version=? AND status='awaiting_confirmation'", addition, result.Summary, now, taskID, version)
	}
	if err != nil {
		return RuntimeTask{}, err
	}
	return ReadRuntimeTask(ctx, tx.Conn, taskID)
}

func (tx *Tx) FailRuntimeAction(ctx context.Context, actionID, attemptID string, version int, code string) (RuntimeTask, error) {
	if code == "" {
		code = "internal"
	}
	var taskID string
	var currentVersion int
	err := tx.Conn.QueryRowContext(ctx, "SELECT task_id,task_version FROM runtime_pending_actions WHERE id=? AND status='executing'", actionID).Scan(&taskID, &currentVersion)
	if err != nil {
		return RuntimeTask{}, err
	}
	if currentVersion != version {
		return RuntimeTask{}, Fail("conflict", "confirmed action changed while it was executing")
	}
	now := Now()
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_action_attempts SET status='failed',error_code=?,finished_at=? WHERE id=? AND status='running'", code, now, attemptID); err != nil {
		return RuntimeTask{}, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='failed',updated_at=? WHERE id=? AND status='executing'", now, actionID); err != nil {
		return RuntimeTask{}, err
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status='action_failed',error_code=?,updated_at=? WHERE id=? AND version=? AND status='awaiting_confirmation'", code, now, taskID, version)
	if err != nil {
		return RuntimeTask{}, err
	}
	return ReadRuntimeTask(ctx, tx.Conn, taskID)
}

func (tx *Tx) UnknownRuntimeAction(ctx context.Context, actionID, attemptID string, version int, code string) (RuntimeTask, error) {
	if code == "" {
		code = "unavailable"
	}
	var taskID string
	var currentVersion int
	err := tx.Conn.QueryRowContext(ctx, "SELECT task_id,task_version FROM runtime_pending_actions WHERE id=? AND status='executing'", actionID).Scan(&taskID, &currentVersion)
	if err != nil {
		return RuntimeTask{}, err
	}
	if currentVersion != version {
		return RuntimeTask{}, Fail("conflict", "confirmed action changed while it was executing")
	}
	now := Now()
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_action_attempts SET status='unknown',error_code=?,finished_at=? WHERE id=? AND status='running'", code, now, attemptID); err != nil {
		return RuntimeTask{}, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='unknown',updated_at=? WHERE id=? AND status='executing'", now, actionID); err != nil {
		return RuntimeTask{}, err
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status='action_unknown',error_code=?,updated_at=? WHERE id=? AND version=? AND status='awaiting_confirmation'", code, now, taskID, version)
	if err != nil {
		return RuntimeTask{}, err
	}
	return ReadRuntimeTask(ctx, tx.Conn, taskID)
}

type RuntimeRecovery struct {
	Batches           int      `json:"batches"`
	Tasks             int      `json:"tasks"`
	ActionsUnknown    int      `json:"actions_unknown"`
	DeliveriesUnknown int      `json:"deliveries_unknown"`
	FailedTaskIDs     []string `json:"failed_task_ids,omitempty"`
}

func RuntimeStageAcknowledgementTaskIDs(ctx context.Context, q Queryer, runtimeID, purpose string) ([]string, error) {
	statusClause := ""
	switch purpose {
	case RuntimeProcessingReceiptPurpose:
		statusClause = "('running','awaiting_confirmation')"
	case RuntimeCompletionReceiptPurpose:
		statusClause = "('completed')"
	case RuntimeFailureReceiptPurpose:
		statusClause = "('failed','action_failed','action_unknown','cancelled')"
	default:
		return nil, Fail("invalid_input", "unsupported runtime stage acknowledgement")
	}
	rows, err := q.QueryContext(ctx, `SELECT t.id FROM runtime_tasks t
WHERE t.runtime_id=? AND t.status IN `+statusClause+`
AND EXISTS(SELECT 1 FROM outbox receipt WHERE receipt.job_id=t.id AND receipt.reason=?)
AND NOT EXISTS(SELECT 1 FROM outbox stage WHERE stage.job_id=t.id AND stage.reason=? AND stage.state<>'ready')
ORDER BY t.updated_at,t.id`, runtimeID, RuntimeReceiptPurpose, purpose)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func RuntimeFailureAcknowledgementTaskIDs(ctx context.Context, q Queryer, runtimeID string) ([]string, error) {
	return RuntimeStageAcknowledgementTaskIDs(ctx, q, runtimeID, RuntimeFailureReceiptPurpose)
}

func (tx *Tx) RecoverRuntime(ctx context.Context, value string) (RuntimeRecovery, error) {
	var out RuntimeRecovery
	c, err := ReadRuntime(ctx, tx.Conn, value)
	if err != nil {
		return out, err
	}
	now := Now()
	if _, err = tx.Conn.ExecContext(ctx, `UPDATE runtime_tasks SET memory_status='failed',memory_error_code='review_interrupted' WHERE runtime_id=? AND id IN (SELECT task_id FROM runtime_reviews WHERE status='running')`, c.ID); err != nil {
		return out, err
	}
	if _, err = tx.Conn.ExecContext(ctx, `UPDATE runtime_reviews SET status='failed',error_code='review_interrupted',finished_at=? WHERE runtime_id=? AND status='running'`, now, c.ID); err != nil {
		return out, err
	}
	if _, err = tx.Conn.ExecContext(ctx, `UPDATE runtime_message_actions SET state='unknown',detail='runtime_restarted',updated_at=? WHERE state='sending'
AND task_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)`, now, c.ID); err != nil {
		return out, err
	}
	batchRows, err := tx.Conn.QueryContext(ctx, "SELECT id FROM runtime_batches WHERE runtime_id=? AND status='analyzing'", c.ID)
	if err != nil {
		return out, err
	}
	ids := []string{}
	for batchRows.Next() {
		var id string
		if err = batchRows.Scan(&id); err != nil {
			batchRows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	batchRows.Close()
	for _, id := range ids {
		if err = tx.FailRuntimeBatch(ctx, id, "runtime_restarted"); err != nil {
			return out, err
		}
		if err = tx.ReleaseWorkLease(ctx, id); err != nil {
			return out, err
		}
		out.Batches++
	}
	taskRows, err := tx.Conn.QueryContext(ctx, "SELECT id FROM runtime_tasks WHERE runtime_id=? AND status='running' ORDER BY id", c.ID)
	if err != nil {
		return out, err
	}
	for taskRows.Next() {
		var id string
		if err = taskRows.Scan(&id); err != nil {
			taskRows.Close()
			return out, err
		}
		out.FailedTaskIDs = append(out.FailedTaskIDs, id)
	}
	if err = taskRows.Err(); err != nil {
		taskRows.Close()
		return out, err
	}
	taskRows.Close()
	if _, execErr := tx.Conn.ExecContext(ctx, `UPDATE runtime_attempts SET status='failed',error_code='runtime_restarted',finished_at=?
WHERE status='running' AND task_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)`, now, c.ID); execErr != nil {
		return out, execErr
	}
	out.Tasks = len(out.FailedTaskIDs)
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status='failed',error_code='runtime_restarted',updated_at=? WHERE runtime_id=? AND status='running'", now, c.ID); err != nil {
		return out, err
	}
	if res, execErr := tx.Conn.ExecContext(ctx, `UPDATE runtime_pending_actions SET status='unknown',updated_at=? WHERE status='executing'
AND task_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)`, now, c.ID); execErr != nil {
		return out, execErr
	} else {
		out.ActionsUnknown, _ = rowsAffected(res)
	}
	if _, err = tx.Conn.ExecContext(ctx, `UPDATE runtime_action_attempts SET status='unknown',error_code='runtime_restarted',finished_at=? WHERE status='running'
AND task_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)`, now, c.ID); err != nil {
		return out, err
	}
	if out.ActionsUnknown > 0 {
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status='action_unknown',error_code='runtime_restarted',updated_at=? WHERE runtime_id=? AND status='awaiting_confirmation'", now, c.ID); err != nil {
			return out, err
		}
	}
	if res, execErr := tx.Conn.ExecContext(ctx, `UPDATE outbox SET state='unknown',updated_at=? WHERE state='sending'
AND job_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)`, now, c.ID); execErr != nil {
		return out, execErr
	} else {
		out.DeliveriesUnknown, _ = rowsAffected(res)
	}
	if _, err = tx.Conn.ExecContext(ctx, `UPDATE delivery_attempts SET state='unknown',finished_at=? WHERE state='sending'
AND outbox_id IN (SELECT o.id FROM outbox o JOIN runtime_tasks t ON t.id=o.job_id WHERE t.runtime_id=?)`, now, c.ID); err != nil {
		return out, err
	}
	return out, nil
}

func rowsAffected(result sql.Result) (int, error) {
	n, err := result.RowsAffected()
	return int(n), err
}

type RuntimeStatus struct {
	Work                   RuntimeWorkStatus `json:"work"`
	Runtime                RuntimeConfig     `json:"runtime"`
	PendingMessages        int               `json:"pending_messages"`
	WaitingReceiptMessages int               `json:"waiting_receipt_messages"`
	AnalyzingBatches       int               `json:"analyzing_batches"`
	Tasks                  map[string]int    `json:"tasks"`
	PendingActions         int               `json:"pending_actions"`
}

// RuntimeStatusList reads every runtime and its counters as one snapshot. The
// grouped UNION keeps console refreshes bounded instead of issuing a separate
// set of count queries for every configured runtime.
func RuntimeStatusList(ctx context.Context, q Queryer) ([]RuntimeStatus, error) {
	configs, err := RuntimeList(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]RuntimeStatus, len(configs))
	byID := make(map[string]*RuntimeStatus, len(configs))
	for i, config := range configs {
		out[i] = RuntimeStatus{Runtime: config, Tasks: map[string]int{}}
		byID[config.ID] = &out[i]
	}
	if len(out) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `
SELECT runtime_id,'message',state,count(*) FROM runtime_message_states
 WHERE state IN ('pending','waiting_receipt') GROUP BY runtime_id,state
UNION ALL
SELECT runtime_id,'batch',status,count(*) FROM runtime_batches
 WHERE status='analyzing' GROUP BY runtime_id,status
UNION ALL
SELECT runtime_id,'task',status,count(*) FROM runtime_tasks GROUP BY runtime_id,status
UNION ALL
SELECT t.runtime_id,'action',a.status,count(*) FROM runtime_pending_actions a
 JOIN runtime_tasks t ON t.id=a.task_id WHERE a.status='pending' GROUP BY t.runtime_id,a.status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var runtimeID, kind, state string
		var count int
		if err = rows.Scan(&runtimeID, &kind, &state, &count); err != nil {
			return nil, err
		}
		status := byID[runtimeID]
		if status == nil {
			return nil, Fail("internal", "runtime status references an unknown runtime")
		}
		switch kind {
		case "message":
			if state == "pending" {
				status.PendingMessages = count
			} else if state == "waiting_receipt" {
				status.WaitingReceiptMessages = count
			}
		case "batch":
			status.AnalyzingBatches = count
		case "task":
			status.Tasks[state] = count
		case "action":
			status.PendingActions = count
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	health, err := readRuntimeWorkStatuses(ctx, q)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Work = health[out[i].Runtime.ID]
	}
	return out, nil
}

func RuntimeStatusFor(ctx context.Context, q Queryer, value string) (RuntimeStatus, error) {
	var out RuntimeStatus
	var err error
	out.Runtime, err = ReadRuntime(ctx, q, value)
	if err != nil {
		return out, err
	}
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM runtime_message_states WHERE runtime_id=? AND state='pending'", out.Runtime.ID).Scan(&out.PendingMessages); err != nil {
		return out, err
	}
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM runtime_message_states WHERE runtime_id=? AND state='waiting_receipt'", out.Runtime.ID).Scan(&out.WaitingReceiptMessages); err != nil {
		return out, err
	}
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM runtime_batches WHERE runtime_id=? AND status='analyzing'", out.Runtime.ID).Scan(&out.AnalyzingBatches); err != nil {
		return out, err
	}
	rows, err := q.QueryContext(ctx, "SELECT status,count(*) FROM runtime_tasks WHERE runtime_id=? GROUP BY status", out.Runtime.ID)
	if err != nil {
		return out, err
	}
	out.Tasks = map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err = rows.Scan(&s, &n); err != nil {
			rows.Close()
			return out, err
		}
		out.Tasks[s] = n
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM runtime_pending_actions a JOIN runtime_tasks t ON t.id=a.task_id WHERE t.runtime_id=? AND a.status='pending'", out.Runtime.ID).Scan(&out.PendingActions); err != nil {
		return out, err
	}
	out.Work, err = ReadRuntimeWorkStatus(ctx, q, out.Runtime)
	return out, err
}

type OutboxView struct {
	ID             string `json:"id"`
	ChannelID      string `json:"channel_id"`
	RouteID        string `json:"route_id"`
	ConversationID string `json:"conversation_id"`
	Transport      string `json:"transport"`
	Content        string `json:"content"`
	Format         string `json:"format"`
	State          string `json:"state"`
	InputDigest    string `json:"input_digest"`
	ReplyTo        string `json:"reply_to,omitempty"`
}

// Runtime acknowledgement labels are message reactions, never chat replies.
const (
	RuntimeAcknowledgement           = "已收到"
	RuntimeProcessingAcknowledgement = "处理中"
	RuntimeCompletionAcknowledgement = "已完成"
	RuntimeFailureAcknowledgement    = "打叉"
	RuntimeReceiptPurpose            = "runtime_receipt"
	RuntimeProcessingReceiptPurpose  = "runtime_processing_receipt"
	RuntimeCompletionReceiptPurpose  = "runtime_completion_receipt"
	RuntimeFailureReceiptPurpose     = "runtime_failure_receipt"
	RuntimeFailureNoticePurpose      = "runtime_failure_notice"
)

func RuntimeReactionPurpose(purpose string) bool {
	return contains([]string{RuntimeReceiptPurpose, RuntimeProcessingReceiptPurpose, RuntimeCompletionReceiptPurpose, RuntimeFailureReceiptPurpose}, purpose)
}

func (tx *Tx) PrepareTaskDelivery(ctx context.Context, taskID string) (OutboxView, error) {
	return tx.prepareTaskDelivery(ctx, taskID, "result")
}

func (tx *Tx) PrepareTaskAcknowledgement(ctx context.Context, taskID string) (OutboxView, error) {
	return tx.prepareTaskDelivery(ctx, taskID, RuntimeReceiptPurpose)
}

func (tx *Tx) PrepareTaskProcessingAcknowledgement(ctx context.Context, taskID string) (OutboxView, error) {
	return tx.prepareTaskDelivery(ctx, taskID, RuntimeProcessingReceiptPurpose)
}

func (tx *Tx) PrepareTaskCompletionAcknowledgement(ctx context.Context, taskID string) (OutboxView, error) {
	return tx.prepareTaskDelivery(ctx, taskID, RuntimeCompletionReceiptPurpose)
}

func (tx *Tx) PrepareTaskFailureAcknowledgement(ctx context.Context, taskID string) (OutboxView, error) {
	return tx.prepareTaskDelivery(ctx, taskID, RuntimeFailureReceiptPurpose)
}

func (tx *Tx) prepareTaskDelivery(ctx context.Context, taskID, purpose string) (OutboxView, error) {
	var out OutboxView
	t, err := ReadRuntimeTask(ctx, tx.Conn, taskID)
	if err != nil {
		return out, err
	}
	switch purpose {
	case RuntimeReceiptPurpose:
		if !contains([]string{"pending", "running"}, t.Status) {
			return out, Fail("conflict", "task is not awaiting an Agent response")
		}
	case RuntimeProcessingReceiptPurpose:
		if !contains([]string{"running", "awaiting_confirmation"}, t.Status) {
			return out, Fail("conflict", "task is not being processed")
		}
	case RuntimeCompletionReceiptPurpose:
		if t.Status != "completed" {
			return out, Fail("conflict", "task has not completed")
		}
	case RuntimeFailureReceiptPurpose:
		if !contains([]string{"failed", "action_failed", "action_unknown", "cancelled"}, t.Status) {
			return out, Fail("conflict", "task has not failed")
		}
	case RuntimeFailureNoticePurpose:
		if !contains([]string{"failed", "action_failed", "action_unknown", "cancelled"}, t.Status) {
			return out, Fail("conflict", "task has not failed")
		}
	default:
		if !contains([]string{"completed", "awaiting_confirmation", "clarification"}, t.Status) {
			return out, Fail("conflict", "task result is not ready for delivery")
		}
	}
	current, err := runtimeTaskMessagesCurrent(ctx, tx.Conn, taskID)
	if err != nil {
		return out, err
	}
	if !current {
		return out, Fail("conflict", "task source messages changed before delivery")
	}
	c, err := ReadRuntime(ctx, tx.Conn, t.RuntimeID)
	if err != nil {
		return out, err
	}
	if RuntimeCompletionPolicy(c) == "record_only" {
		return out, Fail("denied", "background consumption records results; only explicit Agent communication actions may send")
	}
	if purpose == RuntimeReceiptPurpose && (c.Status != "running" || !contains([]string{"direct", "group_mention"}, c.ApplicationMode)) {
		return out, Fail("denied", "only active interactive runtimes acknowledge requests")
	}
	if purpose != RuntimeReceiptPurpose && (RuntimeReactionPurpose(purpose) || purpose == RuntimeFailureNoticePurpose) && (c.Status != "running" || !contains([]string{"direct", "group_mention"}, c.ApplicationMode)) {
		return out, Fail("denied", "only active interactive Agents publish task stage reactions")
	}
	if purpose == RuntimeFailureNoticePurpose && c.ApplicationMode == "group_mention" && !runtimeTaskHasReportableResult(t) {
		return out, Fail("conflict", "group task has no result to report")
	}
	if RuntimeReactionPurpose(purpose) && c.ApplicationMode == "direct" && len(t.Messages) == 1 && DirectCommand(t.Messages[0].Body) != "" {
		return out, Fail("conflict", "system command does not need an Agent acknowledgement")
	}
	admitted, err := runtimeTaskTriggerAdmitted(ctx, tx.Conn, c, t)
	if err != nil {
		return out, err
	}
	if !admitted {
		return out, Fail("conflict", "task trigger route left the runtime processing scope")
	}
	if c.ApplicationMode == "direct" {
		if _, err = RuntimeOwnerDirectProcessingRoute(ctx, tx.Conn, c, t.RouteID); err != nil {
			return out, err
		}
	}
	if purpose == RuntimeReceiptPurpose && c.ApplicationMode == "direct" {
		for _, message := range t.Messages {
			verified, verifyErr := RuntimeOwnerDirectSenderCurrent(ctx, tx.Conn, c, message.ID)
			if verifyErr != nil {
				return out, verifyErr
			}
			if !verified {
				return out, Fail("denied", "private sender is no longer the verified owner")
			}
		}
	}
	transport := "bot_dm"
	var r Route
	if c.ApplicationMode == "group_mention" {
		// The task carries its own trigger route (t.RouteID), which may be a
		// non-default group when a runtime monitors several groups. Delivery
		// must resolve that same route, never the runtime-level default, or a
		// reply can be sent to the wrong group.
		r, err = ReadRoute(ctx, tx.Conn, t.RouteID)
		if err != nil {
			return out, err
		}
		if r.ChannelID != c.ChannelID || r.Status != "active" || r.SendPolicy != "reply_to_trigger" || r.ConversationType != "group" || r.Mode != "assistant" || !contains(r.Triggers, "mention") {
			return out, Fail("denied", "group Agent route no longer permits a reply to its trigger")
		}
		transport = "bot_group"
	} else {
		r, err = ReadRoute(ctx, tx.Conn, c.DeliveryRouteID)
		if err != nil {
			return out, err
		}
		if r.SendPolicy != "dispatch_only" || r.ConversationType != "direct" || (c.ApplicationMode == "direct" && (r.ChannelID != c.ChannelID || r.ConversationID != c.OwnerIDValue)) {
			return out, Fail("denied", "runtime delivery route no longer permits owner direct dispatch")
		}
	}
	content := t.Result
	if strings.TrimSpace(content) == "" {
		content = t.ResultSummary
	}
	if t.Status == "awaiting_confirmation" {
		app, readErr := ReadChannel(ctx, tx.Conn, c.ChannelID)
		if readErr != nil {
			return out, readErr
		}
		buttonCard := c.ApplicationMode == "group_mention" && app.Identity.ConfirmationCardTemplate != ""
		var b strings.Builder
		b.WriteString(strings.TrimSpace(content))
		b.WriteString("\n\n需要你确认后才能执行的操作：")
		for _, action := range t.Actions {
			if action.Status != "pending" {
				continue
			}
			fmt.Fprintf(&b, "\n\n- 类型：%s\n- 目标：%s\n- 具体内容：%s", action.Kind, action.Target, action.Payload)
			if !buttonCard {
				fmt.Fprintf(&b, "\n- 确认口令：`%s`", ConfirmationToken(action))
			}
		}
		content = b.String()
	}
	if purpose == RuntimeReceiptPurpose {
		content = RuntimeAcknowledgement
	} else if purpose == RuntimeProcessingReceiptPurpose {
		content = RuntimeProcessingAcknowledgement
	} else if purpose == RuntimeCompletionReceiptPurpose {
		content = RuntimeCompletionAcknowledgement
	} else if purpose == RuntimeFailureReceiptPurpose {
		content = RuntimeFailureAcknowledgement
	} else if purpose == RuntimeFailureNoticePurpose {
		content = formatRuntimeDelivery(t, c, runtimeFailureNotice(t))
	} else {
		if c.ApplicationMode == "group_mention" && purpose == "result" && t.Status == "awaiting_confirmation" {
			app, readErr := ReadChannel(ctx, tx.Conn, c.ChannelID)
			if readErr != nil {
				return out, readErr
			}
			if app.Identity.ConfirmationCardTemplate != "" {
				content += "\n\n本次操作尚未执行，仅 DWS 所有者可以在卡片中选择“同意”或“拒绝”。"
			} else {
				content += "\n\n确认按钮模板尚未配置。本次操作尚未执行，暂由所有者在本群 @ 机器人发送完整确认口令。"
			}
		}
		content = formatRuntimeDelivery(t, c, content)
	}
	var replyTo string
	if transport == "bot_group" || RuntimeReactionPurpose(purpose) {
		for i := len(t.Messages) - 1; i >= 0; i-- {
			if t.Messages[i].ProviderMessageID != "" {
				replyTo = t.Messages[i].ProviderMessageID
				break
			}
		}
	}
	format := "markdown"
	if (purpose == "result" || purpose == RuntimeFailureNoticePurpose) && c.ApplicationMode == "group_mention" {
		app, readErr := ReadChannel(ctx, tx.Conn, c.ChannelID)
		if readErr != nil {
			return out, readErr
		}
		card := RuntimeCard{Text: content, TaskVersion: t.Version}
		format = "group_markdown"
		if t.Status == "awaiting_confirmation" && app.Identity.ConfirmationCardTemplate != "" {
			format = "confirmation_card"
			card.TemplateID = app.Identity.ConfirmationCardTemplate
			card.Title, card.Summary, card.Details = runtimeConfirmationCardPresentation(t)
			for _, a := range t.Actions {
				if a.Status == "pending" {
					card.Actions = append(card.Actions, RuntimeCardAction{ID: a.ID, Digest: a.PayloadDigest, Kind: a.Kind, Target: a.Target})
				}
			}
		}
		card.Mentions, err = runtimeCardMentions(ctx, tx.Conn, c, t, format == "confirmation_card")
		if err != nil {
			return out, err
		}
		card.Text = formatRuntimeMentionsAtEnd(card.Text, card.Mentions)
		content = JSON(card)
	}
	if RuntimeReactionPurpose(purpose) {
		format = "reaction"
		if replyTo == "" {
			return out, Fail("denied", "acknowledgement requires the original platform message")
		}
	}
	digest := Digest(map[string]any{"task": t.ID, "version": t.Version, "result": content})
	err = tx.Conn.QueryRowContext(ctx, "SELECT id,channel_id,route_id,conversation_id,transport,content,format,state,input_digest FROM outbox WHERE route_id=? AND input_digest=?", r.ID, digest).Scan(&out.ID, &out.ChannelID, &out.RouteID, &out.ConversationID, &out.Transport, &out.Content, &out.Format, &out.State, &out.InputDigest)
	if err == nil {
		out.ReplyTo = replyTo
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out = OutboxView{ID: NewID(), ChannelID: c.ChannelID, RouteID: r.ID, ConversationID: r.ConversationID, Transport: transport, Content: content, Format: format, State: "ready", InputDigest: digest}
	out.ReplyTo = replyTo
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,sender_identity,transport,content,format,input_digest,send_policy,state,reason,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", out.ID, out.ChannelID, out.RouteID, r.Version, t.ID, out.ConversationID, r.AudienceKey, "bot", out.Transport, out.Content, out.Format, out.InputDigest, r.SendPolicy, out.State, purpose, Now(), Now())
	return out, err
}

// A delivery route may remain an authorized owner private inbox after the
// group that triggered the task left the current processing scope. Check the
// original trigger rather than the outbound route at every task boundary.
func runtimeTaskTriggerAdmitted(ctx context.Context, q Queryer, c RuntimeConfig, t RuntimeTask) (bool, error) {
	if t.RuntimeID != c.ID || !contains(c.RouteIDs, t.RouteID) {
		return false, nil
	}
	r, err := ReadRoute(ctx, q, t.RouteID)
	if ErrorCode(err) == "not_found" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.ChannelID == c.ChannelID && r.Status == "active" && r.Mode != "ignore", nil
}

func runtimeOwnerDeliveryRoute(ctx context.Context, q Queryer, c RuntimeConfig) (Route, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+routeColumns+" FROM channel_routes WHERE channel_id=? AND conversation_type='direct' AND send_policy='dispatch_only' AND status='active' ORDER BY created_at", c.ChannelID)
	if err != nil {
		return Route{}, err
	}
	defer rows.Close()
	routes := []Route{}
	for rows.Next() {
		r, scanErr := scanRoute(rows)
		if scanErr != nil {
			return Route{}, scanErr
		}
		routes = append(routes, r)
	}
	if err = rows.Err(); err != nil {
		return Route{}, err
	}
	if len(routes) != 1 {
		return Route{}, Fail("denied", "group Agent confirmation requires exactly one active owner direct route")
	}
	return routes[0], nil
}

func formatRuntimeDelivery(task RuntimeTask, config RuntimeConfig, body string) string {
	body = strings.TrimSpace(body)
	var question string
	for i := len(task.Messages) - 1; i >= 0; i-- {
		if !task.Messages[i].SelfAuthored && strings.TrimSpace(task.Messages[i].Body) != "" {
			question = runtimeQuoteSnippet(task.Messages[i].Body, 80)
			break
		}
	}
	parts := make([]string, 0, 3)
	if question != "" {
		parts = append(parts, "> **你问：** "+question)
	}
	if body != "" {
		parts = append(parts, body)
	}
	model := ""
	var executionElapsed time.Duration
	for i := len(task.Attempts) - 1; i >= 0; i-- {
		a := task.Attempts[i]
		if a.TaskVersion != task.Version || a.Status != "completed" {
			continue
		}
		if value, ok := a.Usage["model"].(string); ok {
			model = strings.TrimSpace(value)
		}
		if model == "" {
			model = strings.TrimSpace(a.Model)
		}
		started, startErr := time.Parse(time.RFC3339Nano, a.StartedAt)
		finished, finishErr := time.Parse(time.RFC3339Nano, a.FinishedAt)
		if startErr == nil && finishErr == nil && finished.After(started) {
			executionElapsed = finished.Sub(started)
		}
		break
	}
	if model == "" && task.Status == "clarification" {
		model = config.AnalysisModel
	}
	if model == "profile" && config.ClaudeProfile != "" {
		model = config.ClaudeProfile + " profile"
	}
	analysisElapsed := time.Duration(task.AnalysisDurationMS) * time.Millisecond
	footer := runtimeDeliveryFooter(analysisElapsed, executionElapsed, model, task.AnalysisModel == "direct_local" || task.AnalysisModel == "direct_agent" || task.AnalysisModel == "local-mention-routing")
	if footer != "" {
		parts = append(parts, "---\n"+footer)
	}
	return strings.Join(parts, "\n\n")
}

// formatRuntimeMentionsAtEnd keeps the answer easy to scan and leaves the
// requester notification at the end. The delivery adapter still sends typed
// IDs to DingTalk; the display name is presentation only.
func formatRuntimeMentionsAtEnd(text string, mentions []RuntimeCardMention) string {
	names := make([]string, 0, len(mentions))
	seen := map[string]bool{}
	for _, mention := range mentions {
		name := strings.Join(strings.Fields(mention.Name), " ")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, "@"+name)
	}
	if len(names) == 0 {
		return text
	}
	// The Agent may have addressed the requester itself. Remove only standalone
	// copies of the same verified display mentions before appending the frozen
	// recipient list once at the end.
	visible := map[string]bool{}
	for _, name := range names {
		visible[name] = true
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if visible[strings.TrimSpace(line)] {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n")) + "\n\n" + strings.Join(names, " ")
}

func runtimeQuoteSnippet(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.TrimSpace(strings.TrimLeft(value, "#>-*`+"))
	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		value = string(runes[:limit]) + "…"
	}
	return value
}

func runtimeDeliveryFooter(analysisElapsed, executionElapsed time.Duration, model string, directLocal bool) string {
	items := []string{}
	if directLocal {
		items = append(items, "⚡ 接入 "+formatRuntimeElapsed(analysisElapsed))
	} else if analysisElapsed > 0 {
		items = append(items, "🔎 分析 "+formatRuntimeElapsed(analysisElapsed))
	}
	if executionElapsed > 0 {
		items = append(items, "⏱ 执行 "+formatRuntimeElapsed(executionElapsed))
	}
	if model = strings.TrimSpace(model); model != "" {
		items = append(items, "🤖 "+model)
	}
	return strings.Join(items, " · ")
}

func formatRuntimeElapsed(value time.Duration) string {
	if value < time.Minute {
		return fmt.Sprintf("%.1fs", value.Seconds())
	}
	total := int(value.Round(time.Second).Seconds())
	if total >= 3600 {
		return fmt.Sprintf("%dh%dm%ds", total/3600, total%3600/60, total%60)
	}
	return fmt.Sprintf("%dm%ds", total/60, total%60)
}

func (tx *Tx) BeginDelivery(ctx context.Context, id string) (int, error) {
	var taskID, preparedDigest, preparedContent, outboxChannel, outboxRoute, outboxConversation, purpose, preparedFormat string
	if err := tx.Conn.QueryRowContext(ctx, "SELECT job_id,input_digest,content,channel_id,route_id,conversation_id,reason,format FROM outbox WHERE id=?", id).Scan(&taskID, &preparedDigest, &preparedContent, &outboxChannel, &outboxRoute, &outboxConversation, &purpose, &preparedFormat); err != nil {
		return 0, err
	}
	if taskID != "" {
		task, err := ReadRuntimeTask(ctx, tx.Conn, taskID)
		if err != nil {
			return 0, err
		}
		config, err := ReadRuntime(ctx, tx.Conn, task.RuntimeID)
		if err != nil {
			return 0, err
		}
		if RuntimeCompletionPolicy(config) == "record_only" {
			return 0, Fail("denied", "background automatic delivery is disabled")
		}
		if config.ApplicationMode == "direct" {
			if _, err = RuntimeOwnerDirectProcessingRoute(ctx, tx.Conn, config, task.RouteID); err != nil {
				return 0, err
			}
			if outboxChannel != config.ChannelID || outboxRoute != config.DeliveryRouteID || outboxConversation != config.OwnerIDValue {
				return 0, Fail("denied", "owner-private outbox address changed before delivery")
			}
		}
		if config.ApplicationMode == "group_mention" {
			expected, err := ReadRoute(ctx, tx.Conn, task.RouteID)
			if err == nil && (expected.Status != "active" || expected.SendPolicy != "reply_to_trigger" || expected.ConversationType != "group" || expected.Mode != "assistant" || !contains(expected.Triggers, "mention")) {
				return 0, Fail("denied", "group route no longer permits a reply")
			}
			if err != nil {
				return 0, err
			}
			if outboxChannel != config.ChannelID || outboxRoute != expected.ID || outboxConversation != expected.ConversationID {
				return 0, Fail("denied", "group task outbox address changed before delivery")
			}
			if preparedFormat == "confirmation_card" || preparedFormat == "group_card" || preparedFormat == "group_markdown" {
				var card RuntimeCard
				app, readErr := ReadChannel(ctx, tx.Conn, config.ChannelID)
				if readErr != nil {
					return 0, readErr
				}
				if json.Unmarshal([]byte(preparedContent), &card) != nil || (preparedFormat == "confirmation_card" && (card.TemplateID == "" || card.TemplateID != app.Identity.ConfirmationCardTemplate)) {
					return 0, Fail("conflict", "confirmation card template changed")
				}
				mentions, mentionErr := runtimeCardMentions(ctx, tx.Conn, config, task, preparedFormat == "confirmation_card")
				if mentionErr != nil {
					return 0, mentionErr
				}
				if !runtimeCardMentionsEqual(card.Mentions, mentions) {
					return 0, Fail("conflict", "card mention identity changed before delivery")
				}
			}
		}
		admitted, err := runtimeTaskTriggerAdmitted(ctx, tx.Conn, config, task)
		if err != nil {
			return 0, err
		}
		current, err := runtimeTaskMessagesCurrent(ctx, tx.Conn, task.ID)
		if err != nil {
			return 0, err
		}
		versionDigest := Digest(map[string]any{"task": task.ID, "version": task.Version, "result": preparedContent})
		validStatus := contains([]string{"completed", "awaiting_confirmation", "clarification"}, task.Status)
		if RuntimeReactionPurpose(purpose) && config.ApplicationMode == "direct" {
			for _, message := range task.Messages {
				verified, verifyErr := RuntimeOwnerDirectSenderCurrent(ctx, tx.Conn, config, message.ID)
				if verifyErr != nil {
					return 0, verifyErr
				}
				if !verified {
					return 0, Fail("denied", "private sender changed before acknowledgement")
				}
			}
		}
		if purpose == RuntimeReceiptPurpose {
			validStatus = config.Status == "running" && contains([]string{"direct", "group_mention"}, config.ApplicationMode) && contains([]string{"pending", "running"}, task.Status) && preparedContent == RuntimeAcknowledgement && preparedFormat == "reaction"
		} else if purpose == RuntimeProcessingReceiptPurpose {
			validStatus = config.Status == "running" && contains([]string{"direct", "group_mention"}, config.ApplicationMode) && contains([]string{"running", "awaiting_confirmation"}, task.Status) && preparedContent == RuntimeProcessingAcknowledgement && preparedFormat == "reaction"
		} else if purpose == RuntimeCompletionReceiptPurpose {
			validStatus = config.Status == "running" && contains([]string{"direct", "group_mention"}, config.ApplicationMode) && task.Status == "completed" && preparedContent == RuntimeCompletionAcknowledgement && preparedFormat == "reaction"
		} else if purpose == RuntimeFailureReceiptPurpose {
			validStatus = config.Status == "running" && contains([]string{"direct", "group_mention"}, config.ApplicationMode) && contains([]string{"failed", "action_failed", "action_unknown", "cancelled"}, task.Status) && preparedContent == RuntimeFailureAcknowledgement && preparedFormat == "reaction"
		} else if purpose == RuntimeFailureNoticePurpose {
			expectedContent := formatRuntimeDelivery(task, config, runtimeFailureNotice(task))
			expectedFormat := "markdown"
			if config.ApplicationMode == "group_mention" {
				card := RuntimeCard{Text: expectedContent, TaskVersion: task.Version}
				mentions, mentionErr := runtimeCardMentions(ctx, tx.Conn, config, task, false)
				if mentionErr != nil {
					return 0, mentionErr
				}
				card.Mentions = mentions
				card.Text = formatRuntimeMentionsAtEnd(card.Text, card.Mentions)
				expectedContent = JSON(card)
				expectedFormat = "group_markdown"
			}
			validStatus = config.Status == "running" && contains([]string{"direct", "group_mention"}, config.ApplicationMode) && contains([]string{"failed", "action_failed", "action_unknown", "cancelled"}, task.Status) && preparedContent == expectedContent && preparedFormat == expectedFormat
		}
		if !admitted || !current || preparedDigest != versionDigest || !validStatus {
			return 0, Fail("conflict", "runtime task or trigger changed before delivery")
		}
	}
	var attempt int
	if err := tx.Conn.QueryRowContext(ctx, "SELECT coalesce(max(attempt),0)+1 FROM delivery_attempts WHERE outbox_id=?", id).Scan(&attempt); err != nil {
		return 0, err
	}
	res, err := tx.Conn.ExecContext(ctx, "UPDATE outbox SET state='sending',updated_at=? WHERE id=? AND state='ready'", Now(), id)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return 0, Fail("conflict", "outbox is not ready; an unknown prior send is never retried automatically")
	}
	var transport string
	if err = tx.Conn.QueryRowContext(ctx, "SELECT transport FROM outbox WHERE id=?", id).Scan(&transport); err != nil {
		return 0, err
	}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO delivery_attempts(id,outbox_id,attempt,transport,state,started_at) VALUES(?,?,?,?,?,?)", NewID(), id, attempt, transport, "sending", Now())
	return attempt, err
}
func (tx *Tx) FinishDelivery(ctx context.Context, id string, attempt int, state, receipt string) error {
	if !contains([]string{"accepted", "failed", "unknown"}, state) {
		return Fail("invalid_input", "invalid delivery state")
	}
	now := Now()
	_, err := tx.Conn.ExecContext(ctx, "UPDATE delivery_attempts SET state=?,receipt=?,finished_at=? WHERE outbox_id=? AND attempt=? AND state='sending'", state, receipt, now, id, attempt)
	if err != nil {
		return err
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE outbox SET state=?,updated_at=? WHERE id=? AND state='sending'", state, now, id)
	return err
}

func (t RuntimeTask) QueryText() string { return strings.TrimSpace(t.Title + "\n" + t.Instructions) }
func RuntimeTaskWorkspace(ctx context.Context, q Queryer, t RuntimeTask) (Workspace, error) {
	r, err := ReadRoute(ctx, q, t.RouteID)
	if err != nil {
		return Workspace{}, err
	}
	var w Workspace
	err = q.QueryRowContext(ctx, "SELECT id,name,coalesce(path,'') FROM workspaces WHERE id=?", r.WorkspaceID).Scan(&w.ID, &w.Name, &w.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return w, Fail("not_found", "task workspace not found")
	}
	return w, err
}

// RuntimeTaskConversationContext reads only the exact trusted conversation
// route: DWS context for a group Agent, or a few previous owner/bot turns in
// the same private application-bot route. Proactive observation has no chat
// history here and a model cannot select a channel by matching message text.
func RuntimeTaskConversationContext(ctx context.Context, q Queryer, c RuntimeConfig, t RuntimeTask, limit int) ([]RuntimeMessage, error) {
	if c.ApplicationMode == "direct" {
		var sessionID string
		if err := q.QueryRowContext(ctx, "SELECT session_id FROM runtime_direct_turns WHERE task_id=?", t.ID).Scan(&sessionID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return []RuntimeMessage{}, nil
			}
			return nil, err
		}
		return RuntimeDirectHistory(ctx, q, c, t, sessionID)
	}
	if c.ApplicationMode != "group_mention" {
		return []RuntimeMessage{}, nil
	}
	if limit < 1 || limit > 100 {
		return nil, Fail("invalid_input", "conversation context limit must be 1..100")
	}
	route, err := ReadRoute(ctx, q, t.RouteID)
	if err != nil {
		return nil, err
	}
	contextChannel := c.ContextChannelID
	if contextChannel == "" {
		contextChannel = c.ChannelID
	}
	rows, err := q.QueryContext(ctx, `SELECT id FROM messages WHERE channel_id=? AND conversation_id=? AND availability='available'
ORDER BY sent_at DESC,id DESC LIMIT ?`, contextChannel, route.ConversationID, limit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
	return runtimeMessages(ctx, q, ids)
}

// RuntimeOwnerDirectProcessingRoute binds an authenticated application-bot
// callback conversation to this runtime's exact owner dispatch address.
// DingTalk uses a callback CID and an owner user ID in different namespaces;
// they must not be compared as strings or guessed from message text. The
// callback route is explicitly in route_ids, while the outbound address must
// equal the configured verified owner user ID on the same app channel.
func RuntimeOwnerDirectProcessingRoute(ctx context.Context, q Queryer, c RuntimeConfig, routeID string) (Route, error) {
	if c.ApplicationMode != "direct" || c.OwnerPrincipalID == "" || c.OwnerIDValue == "" || !contains(c.RouteIDs, routeID) {
		return Route{}, Fail("denied", "owner-private processing route is not explicitly admitted")
	}
	app, err := ReadChannel(ctx, q, c.ChannelID)
	if err != nil {
		return Route{}, err
	}
	if app.Kind != ChannelDingTalkApp || app.Tenant == "" || app.Identity.RobotCode == "" || !contains([]string{"configured", "active"}, app.Status) {
		return Route{}, Fail("denied", "owner-private processing requires the configured application robot")
	}
	history, err := ReadChannel(ctx, q, app.Identity.HistoryChannel)
	if err != nil || history.Kind != ChannelDwsPersonal || history.Tenant != app.Tenant || !contains([]string{"configured", "active"}, history.Status) || c.OwnerIDType != "user_id" || c.OwnerIDValue != history.Identity.ExpectedUserID {
		return Route{}, Fail("denied", "owner-private processing requires the same-enterprise authenticated DWS owner")
	}
	var owner string
	err = q.QueryRowContext(ctx, `SELECT principal_id FROM identity_aliases
WHERE tenant=? AND id_type=? AND id_value=? AND verified=1 AND basis='authenticated_dws_profile'`, app.Tenant, c.OwnerIDType, c.OwnerIDValue).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != c.OwnerPrincipalID) {
		return Route{}, Fail("denied", "owner-private user ID no longer resolves to the verified owner")
	}
	if err != nil {
		return Route{}, err
	}
	delivery, err := ReadRoute(ctx, q, c.DeliveryRouteID)
	if err != nil {
		return Route{}, err
	}
	if delivery.ChannelID != app.ID || delivery.Status != "active" || delivery.Mode == "ignore" || delivery.ConversationType != "direct" || delivery.SendPolicy != "dispatch_only" || delivery.ConversationID != c.OwnerIDValue {
		return Route{}, Fail("denied", "owner-private dispatch address is not bound to the verified owner")
	}
	processing, err := ReadRoute(ctx, q, routeID)
	if err != nil {
		return Route{}, err
	}
	if processing.ChannelID != app.ID || processing.Status != "active" || processing.Mode == "ignore" || processing.ConversationType != "direct" || processing.ConversationID == "" ||
		(processing.ID == delivery.ID && processing.SendPolicy != "dispatch_only") || (processing.ID != delivery.ID && processing.SendPolicy != "draft_only") {
		return Route{}, Fail("denied", "owner-private callback route is not an admitted direct inbox")
	}
	return processing, nil
}

// A cached principal must still resolve from the authenticated platform
// sender alias when the task is admitted; revoked aliases do not keep access.
func RuntimeOwnerDirectSenderCurrent(ctx context.Context, q Queryer, c RuntimeConfig, messageID string) (bool, error) {
	return runtimeConfirmationSenderCurrent(ctx, q, c.ChannelID, messageID, c.OwnerPrincipalID)
}

func RuntimeBatchDescription(b RuntimeBatch) string {
	return fmt.Sprintf("batch %s contains %d new messages and %d context messages", b.ID, len(b.Messages), len(b.Context))
}
