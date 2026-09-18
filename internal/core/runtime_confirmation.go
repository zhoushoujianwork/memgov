package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// runtimeConfirmationRoutes retains the explicitly configured legacy inbox and
// admits a robot inbox only through an authenticated owner and explicit history
// binding. Similar identifiers in different DingTalk namespaces are not links.
func runtimeConfirmationRoutes(ctx context.Context, q Queryer, c RuntimeConfig) ([]Route, error) {
	var legacy Route
	var err error
	if c.ApplicationMode == "group_mention" {
		routes := []Route{}
		for _, id := range c.RouteIDs {
			r, readErr := ReadRoute(ctx, q, id)
			if readErr != nil {
				return nil, readErr
			}
			if r.ChannelID == c.ChannelID && r.Status == "active" && r.ConversationType == "group" && r.Mode == "assistant" && r.SendPolicy == "reply_to_trigger" && contains(r.Triggers, "mention") {
				routes = append(routes, r)
			}
		}
		return routes, nil
	} else if c.ApplicationMode != "proactive" {
		legacy, err = ReadRoute(ctx, q, c.DeliveryRouteID)
	}
	if err != nil {
		return nil, err
	}
	routes := []Route{}
	if legacy.ChannelID == c.ChannelID && legacy.Status == "active" && legacy.Mode != "ignore" && legacy.ConversationType == "direct" && legacy.SendPolicy == "dispatch_only" {
		routes = append(routes, legacy)
	}
	dws, err := ReadChannel(ctx, q, c.ChannelID)
	if err != nil {
		return nil, err
	}
	if c.ApplicationMode != "proactive" || dws.Kind != ChannelDwsPersonal || !contains([]string{"configured", "active"}, dws.Status) || c.OwnerIDType != "user_id" || c.OwnerIDValue != dws.Identity.ExpectedUserID {
		return routes, nil
	}
	var principal string
	err = q.QueryRowContext(ctx, `SELECT principal_id FROM identity_aliases
WHERE tenant=? AND id_type='user_id' AND id_value=? AND verified=1 AND basis='authenticated_dws_profile'`, dws.Tenant, c.OwnerIDValue).Scan(&principal)
	if errors.Is(err, sql.ErrNoRows) {
		return routes, nil
	}
	if err != nil {
		return nil, err
	}
	if principal != c.OwnerPrincipalID {
		return routes, nil
	}
	rows, err := q.QueryContext(ctx, "SELECT id FROM channels WHERE kind=? AND tenant=? AND status IN ('configured','active')", ChannelDingTalkApp, dws.Tenant)
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
	bound := []Channel{}
	for _, id := range ids {
		app, readErr := ReadChannel(ctx, q, id)
		if readErr != nil {
			return nil, readErr
		}
		if app.Identity.HistoryChannel == "" {
			continue
		}
		history, readErr := ReadChannel(ctx, q, app.Identity.HistoryChannel)
		if readErr != nil || history.ID != dws.ID {
			continue
		}
		if dws.Identity.DeliveryRobotCode != "" && app.Identity.RobotCode != dws.Identity.DeliveryRobotCode {
			continue
		}
		bound = append(bound, app)
	}
	// Ambiguity never broadens the inbox, even if one app currently has no route.
	if len(bound) != 1 {
		return routes, nil
	}
	appConfig := c
	appConfig.ChannelID = bound[0].ID
	inbox, err := runtimeOwnerDeliveryRoute(ctx, q, appConfig)
	if ErrorCode(err) == "denied" {
		return routes, nil
	}
	if err != nil {
		return nil, err
	}
	if inbox.Mode != "ignore" {
		routes = append(routes, inbox)
	}
	return routes, nil
}

// Cached sender_principal is insufficient when an alias has since been revoked
// or corrected. Resolve the exact identifier again inside the approval transaction.
func runtimeConfirmationSenderCurrent(ctx context.Context, q Queryer, channelID, messageID, owner string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM messages m
JOIN channels c ON c.id=m.channel_id
JOIN identity_aliases a ON a.tenant=c.tenant AND a.id_type=m.sender_id_type AND a.id_value=m.sender_id_value
WHERE m.id=? AND c.id=? AND c.status IN ('configured','active') AND m.sender_principal=? AND a.principal_id=? AND a.verified=1`, messageID, channelID, owner, owner).Scan(&n)
	return n == 1, err
}

// Only strip a single platform-addressed leading @ label. The remaining
// complete token must match; negation, quotes and arbitrary prose never approve.
func runtimeGroupConfirmationBody(body string, addressed bool) string {
	body = strings.TrimSpace(body)
	if addressed && strings.HasPrefix(body, "@") {
		if label, rest, ok := strings.Cut(body, " "); ok && len(label) > 1 {
			return strings.TrimSpace(rest)
		}
	}
	return body
}

// Recognizing a token only suppresses task creation; it grants no approval.
func runtimeMessageIsConfirmation(ctx context.Context, q Queryer, messageID string, group bool) (bool, error) {
	var body string
	var addressed int
	err := q.QueryRowContext(ctx, `SELECT mr.body,m.addressed FROM messages m
JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision WHERE m.id=?`, messageID).Scan(&body, &addressed)
	if err != nil {
		return false, err
	}
	if group {
		body = runtimeGroupConfirmationBody(body, addressed != 0)
	}
	var n int
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_pending_actions a
WHERE ?='确认操作 '||a.id||' '||substr(a.payload_digest,1,12)`, strings.TrimSpace(body)).Scan(&n)
	return n != 0, err
}
