package cli

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// Channel input is compared as a declaration; actual authorization state is
// independently fingerprinted in preconditions. Secrets remain references.
func planDualChannels(ctx context.Context, p *DualConfigPlan, s dualPlanState, old core.AppliedConfig, issue func(bool, string, string, string, string)) dualPlanState {
	var previous struct {
		Channels []core.ChannelInput `json:"channels"`
	}
	_ = json.Unmarshal(old.Declaration, &previous)
	oldInput := map[string]core.ChannelInput{}
	for _, c := range previous.Channels {
		oldInput[c.Name] = c
	}
	managed := map[string]core.ManagedConfigObject{}
	for _, o := range old.Objects {
		if o.Kind == "channel" {
			managed[o.Name] = o
		}
	}
	desired := map[string]bool{}
	for _, in := range p.DesiredChannels {
		desired[in.Name] = true
		change := PlanChange{Kind: "channel", Name: in.Name, Action: "create", AfterDigest: core.Digest(in)}
		if before, ok := oldInput[in.Name]; ok {
			change.BeforeDigest = core.Digest(before)
			change.Action = "update"
			if change.BeforeDigest == change.AfterDigest {
				change.Action = "unchanged"
			}
		}
		current, exists := s.Channels[in.Name]
		if exists {
			obj, owned := managed[in.Name]
			priorInput, hasPriorInput := oldInput[in.Name]
			if !hasPriorInput {
				priorInput = in
				if owned {
					change.Action = "update"
				}
			}
			if !owned || obj.ObjectID != current.ID {
				change.Action = "unmanaged_conflict"
				issue(true, "unmanaged_conflict", "channel", in.Name, "A same-named channel is unmanaged; keep using it by reference or explicitly adopt it outside this declaration.")
			} else if obj.Snapshot == "" || obj.Snapshot != dualChannelManagedSnapshot(current, priorInput) {
				change.Drift = true
				issue(true, "managed_object_drift", "channel", in.Name, "Managed channel changed after application; reconcile the drift before updating.")
			}
			identity := in.Identity
			if identity.Profile == "" && in.Kind == core.ChannelDwsPersonal {
				identity.Profile = identity.ExpectedCorpID + ":" + identity.ExpectedUserID
			}
			if in.Kind != current.Kind || identity.ExpectedCorpID != current.Tenant || identity.ClientID != current.Identity.ClientID || identity.ExpectedUserID != current.Identity.ExpectedUserID || (in.Kind == core.ChannelDwsPersonal && identity.Profile != current.Identity.Profile) {
				issue(true, "channel_identity_repoint", "channel", in.Name, "Channel account or enterprise cannot be repointed; use a new channel name.")
			}
			if change.Action == "update" {
				if in.Subscription != nil {
					issue(true, "channel_subscription_update", "channel", in.Name, "Subscriptions are creation-only; remove the subscription field when updating this channel.")
				}
				if core.Digest(in.Identity) != core.Digest(current.Identity) || in.CredentialRef != current.CredentialRef {
					change.BoundaryChange = true
				}
				var live int
				err := s.Q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM runtime_configs WHERE (channel_id=? OR context_channel_id=?) AND status NOT IN ('configured','stopped')) + (SELECT count(*) FROM data_sources WHERE channel_id=? AND status!='stopped') + (SELECT count(*) FROM channel_leases WHERE channel_id=? AND until>?) + (SELECT count(*) FROM outbox WHERE channel_id=? AND state='sending')`, current.ID, current.ID, current.ID, current.ID, core.Now(), current.ID).Scan(&live)
				if err != nil || live > 0 {
					issue(true, "channel_in_use", "channel", in.Name, "Stop channel receivers and dependent consumers, and reconcile in-flight delivery before updating.")
				}
			}
		} else if _, owned := managed[in.Name]; owned {
			change.Drift = true
			issue(true, "managed_object_drift", "channel", in.Name, "Managed channel is missing.")
		}
		projected := current
		if !exists {
			projected = core.Channel{ID: "planned:" + in.Name, Name: in.Name, Kind: in.Kind, Tenant: in.Identity.ExpectedCorpID, Routes: []core.Route{}}
		}
		if change.Action == "create" || change.Action == "update" {
			projected.Identity = in.Identity
			projected.CredentialRef = in.CredentialRef
			projected.Status = "configured"
			projected.Capabilities = core.Capabilities{Verified: map[string]bool{}}
			if projected.Kind == core.ChannelDwsPersonal && projected.Identity.Profile == "" {
				projected.Identity.Profile = projected.Tenant + ":" + projected.Identity.ExpectedUserID
			}
			issue(false, "channel_probe_required", "channel", in.Name, "Channel is installed without network access; verify capabilities before starting collection or AI consumers.")
			if in.Route != nil {
				route, ok := previewChannelRoute(*in.Route, projected, s, p.DefaultWorkspace)
				if !ok {
					issue(true, "channel_route_invalid", "channel", in.Name, "Route contains unsupported policy, an invalid identifier or a missing workspace.")
				} else {
					found := false
					for i, r := range projected.Routes {
						if r.ConversationID == route.ConversationID {
							found = true
							route.ID = r.ID
							route.Version = r.Version
							if route.WorkspaceID != r.WorkspaceID || route.ConversationType != r.ConversationType {
								issue(true, "channel_route_identity", "channel", in.Name, "Existing route workspace and conversation type cannot change.")
							}
							if route.AudiencePolicy != r.AudiencePolicy || route.SendPolicy != r.SendPolicy || route.MemoryPolicy != r.MemoryPolicy || route.Mode != r.Mode || core.Digest(route.Triggers) != core.Digest(r.Triggers) {
								change.BoundaryChange = true
							}
							projected.Routes[i] = route
						}
					}
					if !found {
						projected.Routes = append(projected.Routes, route)
						if route.SendPolicy != "draft_only" || route.AudiencePolicy == "conversation" {
							change.PermissionExpansion = true
						}
					}
				}
			}
		}
		if change.BoundaryChange {
			issue(true, "authorization_boundary_change", "channel", in.Name, "Robot, credential reference or route authorization changes require explicit reauthorization.")
		}
		if change.PermissionExpansion {
			issue(true, "permission_expansion", "channel", in.Name, "New route permits external delivery or conversation disclosure; explicitly authorize this scope.")
		}
		s.Channels[in.Name] = projected
		s.Routes[projected.ID] = map[string]core.Route{}
		for _, r := range projected.Routes {
			s.Routes[projected.ID][r.ConversationID] = r
		}
		p.Changes = append(p.Changes, change)
	}
	for name := range managed {
		if !desired[name] {
			p.Changes = append(p.Changes, PlanChange{Kind: "channel", Name: name, Action: "preserve"})
			issue(false, "channel_preserved", "channel", name, "Omitted shared channel is preserved; channel removal requires a separate explicit action.")
		}
	}
	return s
}

func previewChannelRoute(in core.RouteInput, c core.Channel, s dualPlanState, workspace string) (core.Route, bool) {
	if !stableConfigID.MatchString(in.ConversationID) {
		return core.Route{}, false
	}
	if in.ConversationType == "" {
		in.ConversationType = "group"
	}
	if in.Mode == "" {
		in.Mode = "assistant"
		if c.Kind == core.ChannelDwsPersonal {
			in.Mode = "collect"
		}
	}
	if in.AudiencePolicy == "" {
		in.AudiencePolicy = "local_private"
	}
	if in.MemoryPolicy == "" {
		in.MemoryPolicy = "explicit_only"
	}
	if in.SendPolicy == "" {
		in.SendPolicy = "draft_only"
	}
	allowed := func(v string, values ...string) bool {
		for _, x := range values {
			if v == x {
				return true
			}
		}
		return false
	}
	if !allowed(in.ConversationType, "group", "direct") || !allowed(in.Mode, "collect", "assistant", "notify", "ignore") || !allowed(in.AudiencePolicy, "local_private", "conversation") || !allowed(in.MemoryPolicy, "explicit_only", "curated") || !allowed(in.SendPolicy, "draft_only", "dispatch_only", "reply_to_trigger") {
		return core.Route{}, false
	}
	for _, trigger := range in.Triggers {
		if !allowed(trigger, "mention", "direct") {
			return core.Route{}, false
		}
	}
	if in.Retention != "" && !json.Valid([]byte(in.Retention)) {
		return core.Route{}, false
	}
	if in.Workspace != "" {
		workspace = in.Workspace
	}
	w := s.Workspaces[workspace]
	if w.ID == "" {
		for _, v := range s.Workspaces {
			if v.ID == workspace {
				w = v
			}
		}
	}
	if w.ID == "" {
		return core.Route{}, false
	}
	triggers := append([]string{}, in.Triggers...)
	sort.Strings(triggers)
	return core.Route{ID: "planned-route:" + c.Name, ChannelID: c.ID, ConversationID: in.ConversationID, ConversationType: in.ConversationType, WorkspaceID: w.ID, Mode: in.Mode, Triggers: triggers, AudiencePolicy: in.AudiencePolicy, MemoryPolicy: in.MemoryPolicy, SendPolicy: in.SendPolicy, Status: "active"}, true
}

func applyDualChannels(ctx context.Context, tx *core.Tx, plan DualConfigPlan) error {
	for _, in := range plan.DesiredChannels {
		change := dualChange(plan, "channel", in.Name)
		if change.Action == "unchanged" {
			continue
		}
		if change.Action != "create" && change.Action != "update" {
			return core.Fail("conflict", "channel plan is not applicable")
		}
		expected, routeVersion := 0, 0
		current, err := core.ReadChannel(ctx, tx.Conn, in.Name)
		if err == nil {
			expected = current.ConfigVersion
			if in.Route != nil {
				for _, r := range current.Routes {
					if r.ConversationID == in.Route.ConversationID {
						routeVersion = r.Version
					}
				}
			}
		} else if core.ErrorCode(err) != "not_found" {
			return err
		}
		if in.Route != nil && in.Route.Workspace == "" {
			copyRoute := *in.Route
			copyRoute.Workspace = plan.DefaultWorkspace
			in.Route = &copyRoute
		}
		c, err := tx.ApplyChannelConfig(ctx, in, expected, routeVersion, "Apply reviewed dual-mode channel plan")
		if err != nil {
			return core.Fail(core.ErrorCode(err), "channel configuration could not be applied")
		}
		if expected > 0 {
			rows, err := tx.Conn.QueryContext(ctx, "SELECT id FROM runtime_configs WHERE channel_id=? OR context_channel_id=?", c.ID, c.ID)
			if err != nil {
				return err
			}
			ids := []string{}
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				ids = append(ids, id)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err = tx.InvalidateRuntimeConfigWork(ctx, id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Capability probes change verification status, not the user's applied channel
// policy. Keep them in plan preconditions but outside managed-policy drift.
func dualChannelManagedSnapshot(c core.Channel, in core.ChannelInput) string {
	var ownedRoute any
	if in.Route != nil {
		for _, r := range c.Routes {
			if r.ConversationID == in.Route.ConversationID {
				ownedRoute = map[string]any{"id": r.ID, "version": r.Version, "workspace": r.WorkspaceID, "mode": r.Mode, "triggers": r.Triggers, "audience": r.AudiencePolicy, "memory": r.MemoryPolicy, "send": r.SendPolicy, "status": r.Status}
				break
			}
		}
	}
	return core.Digest(map[string]any{"identity": c.Identity, "kind": c.Kind, "tenant": c.Tenant, "credential_ref_digest": core.Digest(c.CredentialRef), "declared_route": ownedRoute})
}
