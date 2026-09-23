package core

// RuntimeManagedSnapshot describes settings owned by a config declaration.
func RuntimeManagedSnapshot(r RuntimeConfig) map[string]any {
	value := map[string]any{"channel_id": r.ChannelID, "delivery_route_id": r.DeliveryRouteID, "owner_principal_id": r.OwnerPrincipalID, "owner_id_type": r.OwnerIDType, "owner_id_value": r.OwnerIDValue, "application_mode": r.ApplicationMode, "context_channel_id": r.ContextChannelID, "agent_capabilities": r.AgentCapabilities, "claude_profile": r.ClaudeProfile, "analysis_model": r.AnalysisModel, "execution_model": r.ExecutionModel, "agent_preset": r.AgentPreset, "item_threshold": r.ItemThreshold, "max_wait_seconds": r.MaxWaitSeconds, "reconcile_seconds": r.ReconcileSeconds}
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
	return value
}

// RouteManagedSnapshot describes route settings owned by a config declaration.
func RouteManagedSnapshot(r Route) map[string]any {
	return map[string]any{"channel_id": r.ChannelID, "conversation_id": r.ConversationID, "workspace_id": r.WorkspaceID, "mode": r.Mode, "triggers": r.Triggers, "audience_policy": r.AudiencePolicy, "send_policy": r.SendPolicy, "status": r.Status, "version": r.Version}
}

// ChannelManagedSnapshot excludes transient capability-probe state.
func ChannelManagedSnapshot(c Channel, in ChannelInput) string {
	return Digest(channelManagedSnapshot(c, in, false))
}
func channelManagedSnapshot(c Channel, in ChannelInput, legacyMemory bool) map[string]any {
	var ownedRoute any
	if in.Route != nil {
		for _, r := range c.Routes {
			if r.ConversationID == in.Route.ConversationID {
				route := map[string]any{"id": r.ID, "version": r.Version, "workspace": r.WorkspaceID, "mode": r.Mode, "triggers": r.Triggers, "audience": r.AudiencePolicy, "send": r.SendPolicy, "status": r.Status}
				if legacyMemory {
					route["memory"] = r.MemoryPolicy
				}
				ownedRoute = route
				break
			}
		}
	}
	return map[string]any{"identity": c.Identity, "kind": c.Kind, "tenant": c.Tenant, "credential_ref_digest": Digest(c.CredentialRef), "declared_route": ownedRoute}
}
