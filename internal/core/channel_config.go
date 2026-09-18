package core

import "context"

// ApplyChannelConfig keeps YAML an input format: the committed configuration,
// policy versions, capabilities and audit facts remain authoritative in SQLite.
func (tx *Tx) ApplyChannelConfig(ctx context.Context, in ChannelInput, expected, expectedRoute int, reason string) (Channel, error) {
	next, err := normalizeChannel(in)
	if err != nil {
		return Channel{}, err
	}
	current, err := ReadChannel(ctx, tx.Conn, in.Name)
	if ErrorCode(err) == "not_found" {
		if expected != 0 || expectedRoute != 0 {
			return Channel{}, Fail("conflict", "channel does not exist; omit expected versions when creating it")
		}
		return tx.AddChannel(ctx, in)
	}
	if err != nil {
		return Channel{}, err
	}
	if expected < 1 || current.ConfigVersion != expected {
		return Channel{}, Fail("conflict", "expected channel version %d, current %d; use --expected-version", expected, current.ConfigVersion)
	}
	if reason == "" {
		return Channel{}, Fail("invalid_input", "updating a channel requires --reason")
	}
	if current.Kind != next.Kind || current.AuthNamespace != next.AuthNamespace || current.IDNamespace != next.IDNamespace ||
		current.Identity.ExpectedUserID != next.Identity.ExpectedUserID {
		return Channel{}, Fail("conflict", "channel identity cannot be repointed; register a new channel name")
	}
	if in.Subscription != nil {
		return Channel{}, Fail("invalid_input", "subscription is only accepted when creating a channel; manage existing conversations through channel route")
	}
	lease, err := ReadLease(ctx, tx.Conn, current.ID)
	if err != nil {
		return Channel{}, err
	}
	var live int
	if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM runtime_configs WHERE channel_id=? AND status NOT IN ('configured','stopped')", current.ID).Scan(&live); err != nil {
		return Channel{}, err
	}
	if lease.Held || live > 0 {
		return Channel{}, Fail("conflict", "stop the channel receiver and its runtimes before changing configuration")
	}
	// A sending attempt may already have crossed the platform boundary.
	if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM outbox WHERE channel_id=? AND state='sending'", current.ID).Scan(&live); err != nil {
		return Channel{}, err
	}
	if live > 0 {
		return Channel{}, Fail("conflict", "channel has an in-flight delivery; reconcile it before changing configuration")
	}
	if in.Route != nil {
		var existing *Route
		for i := range current.Routes {
			if current.Routes[i].ConversationID == in.Route.ConversationID {
				existing = &current.Routes[i]
				break
			}
		}
		if existing == nil {
			if expectedRoute != 0 {
				return Channel{}, Fail("conflict", "route does not exist; omit --expected-route-version when adding it")
			}
			if _, err = tx.AddRoute(ctx, current.ID, *in.Route); err != nil {
				return Channel{}, err
			}
		} else {
			if in.Route.ConversationType != "" && in.Route.ConversationType != existing.ConversationType {
				return Channel{}, Fail("conflict", "existing route conversation type cannot change")
			}
			if in.Route.Workspace != "" {
				var scope string
				err = tx.Conn.QueryRowContext(ctx, "SELECT id FROM workspaces WHERE id=? OR name=?", in.Route.Workspace, in.Route.Workspace).Scan(&scope)
				if err != nil || scope != existing.WorkspaceID {
					return Channel{}, Fail("conflict", "existing route workspace cannot change")
				}
			}
			if _, err = tx.UpdateRoute(ctx, existing.ID, expectedRoute, *in.Route, reason); err != nil {
				return Channel{}, err
			}
		}
	} else if expectedRoute != 0 {
		return Channel{}, Fail("invalid_input", "--expected-route-version requires a route in the selected configuration")
	}
	// New credentials or robot identity require a fresh, explicit platform probe.
	_, err = tx.Conn.ExecContext(ctx, "UPDATE channels SET identity=?,credential_ref=?,capabilities=?,tool_version='',status='configured',config_version=config_version+1,updated_at=? WHERE id=? AND config_version=?",
		JSON(next.Identity), next.CredentialRef, JSON(next.Capabilities), Now(), current.ID, expected)
	if err != nil {
		return Channel{}, err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE outbox SET state='stale',reason='channel configuration changed',updated_at=? WHERE channel_id=? AND state IN ('draft','ready')", Now(), current.ID); err != nil {
		return Channel{}, err
	}
	if _, err = tx.Audit(ctx, "channel.configure", reason, []Change{{ObjectType: "channel", ObjectID: current.ID, Before: expected, After: expected + 1}}); err != nil {
		return Channel{}, err
	}
	return ReadChannel(ctx, tx.Conn, current.ID)
}
