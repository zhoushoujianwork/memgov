package core

import (
	"context"
	"time"
)

// RuntimeMessagesNeedSync reports whether SyncRuntimeMessages can change
// durable state. It deliberately runs outside a write transaction; the write
// path repeats every check before applying a change.
func RuntimeMessagesNeedSync(ctx context.Context, q Queryer, value string) (bool, error) {
	c, err := ReadRuntime(ctx, q, value)
	if err != nil {
		return false, err
	}
	for _, routeID := range runtimeProcessingRouteIDs(c) {
		r, readErr := ReadRoute(ctx, q, routeID)
		if readErr != nil {
			return false, readErr
		}
		if r.Mode == "ignore" {
			var pending int
			if err = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM runtime_message_states WHERE runtime_id=? AND route_id=? AND state='pending')", c.ID, routeID).Scan(&pending); err != nil {
				return false, err
			}
			if pending != 0 {
				return true, nil
			}
			continue
		}

		retained := retainedMessagePredicate("m")
		rows, queryErr := q.QueryContext(ctx, `SELECT m.id,m.current_revision,m.sent_at,
CASE WHEN `+retained+` THEN m.availability ELSE 'expired' END,
m.self_authored,m.addressed,m.context_only,m.provider_message_id,
coalesce(rms.revision,-1),coalesce(rms.state,'')
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
ORDER BY m.sent_at,m.id`, c.ID, c.ChannelID, r.ConversationID)
		if queryErr != nil {
			return false, queryErr
		}
		for rows.Next() {
			var id, sentAt, availability, providerMessageID, priorState string
			var revision, self, addressed, contextOnly, priorRevision int
			if queryErr = rows.Scan(&id, &revision, &sentAt, &availability, &self, &addressed, &contextOnly, &providerMessageID, &priorRevision, &priorState); queryErr != nil {
				rows.Close()
				return false, queryErr
			}
			if priorState == "" {
				rows.Close()
				return true, nil
			}

			waitingReceipt := false
			if c.ApplicationMode == "proactive" {
				echo, echoErr := RuntimeAgentMessageEcho(ctx, q, c.ChannelID, r.ConversationID, providerMessageID)
				if echoErr != nil {
					rows.Close()
					return false, echoErr
				}
				if echo {
					contextOnly = 1
				} else if self == 1 {
					waitingReceipt, echoErr = RuntimeAgentMessageAwaitingReceipt(ctx, q, c.ChannelID, r.ConversationID, sentAt)
					if echoErr != nil {
						rows.Close()
						return false, echoErr
					}
				}
			}
			if r.ConversationType == "direct" || c.ApplicationMode == "group_mention" {
				control, controlErr := runtimeMessageIsConfirmation(ctx, q, id, c.ApplicationMode == "group_mention")
				if controlErr != nil {
					rows.Close()
					return false, controlErr
				}
				if control {
					contextOnly = 1
				}
			}

			nextState := priorState
			changed := priorRevision != revision
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
			if changed || nextState != priorState {
				rows.Close()
				return true, nil
			}
		}
		queryErr = rows.Err()
		rows.Close()
		if queryErr != nil {
			return false, queryErr
		}
	}
	return false, nil
}

// RuntimeConfirmationsReady performs the exact token and verified-owner match
// used by ProcessRuntimeConfirmations without acquiring SQLite's writer lock.
func RuntimeConfirmationsReady(ctx context.Context, q Queryer, value string) (bool, error) {
	c, err := ReadRuntime(ctx, q, value)
	if err != nil {
		return false, err
	}
	routes, err := runtimeConfirmationRoutes(ctx, q, c)
	if err != nil || len(routes) == 0 {
		return false, err
	}
	rows, err := q.QueryContext(ctx, `SELECT a.id,a.payload_digest,a.created_at,t.route_id FROM runtime_pending_actions a
JOIN runtime_tasks t ON t.id=a.task_id WHERE t.runtime_id=? AND t.status='awaiting_confirmation' AND t.version=a.task_version AND a.status='pending'
AND t.route_id IN (SELECT value FROM json_each(?))
AND EXISTS (SELECT 1 FROM channel_routes r WHERE r.id=t.route_id AND r.channel_id=? AND r.status='active' AND r.mode<>'ignore')
ORDER BY a.created_at`, c.ID, JSON(c.RouteIDs), c.ChannelID)
	if err != nil {
		return false, err
	}
	type candidate struct{ id, digest, created, routeID string }
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.id, &item.digest, &item.created, &item.routeID); err != nil {
			rows.Close()
			return false, err
		}
		candidates = append(candidates, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, item := range candidates {
		digest := item.digest
		if len(digest) > 12 {
			digest = digest[:12]
		}
		token := "确认操作 " + item.id + " " + digest
		for _, route := range routes {
			if c.ApplicationMode == "group_mention" && route.ID != item.routeID {
				continue
			}
			predicate := "trim(mr.body)=?"
			args := []any{route.ChannelID, route.ConversationID, c.OwnerPrincipalID, item.created, token}
			if c.ApplicationMode == "proactive" {
				predicate += " AND (SELECT origin FROM inbox_events e WHERE e.message_id=m.id ORDER BY received_at,e.rowid LIMIT 1)='stream' AND m.context_only=0 AND julianday(m.created_at)>=julianday(?)"
				args = append(args, item.created)
			}
			if c.ApplicationMode == "group_mention" {
				predicate = "(" + predicate + " OR (m.addressed=1 AND substr(trim(mr.body),1,1)='@' AND instr(trim(mr.body),' ')>1 AND trim(substr(trim(mr.body),instr(trim(mr.body),' ')+1))=?))"
				args = append(args, token)
			}
			var found int
			err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m
JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision
JOIN channels ch ON ch.id=m.channel_id
JOIN identity_aliases ia ON ia.tenant=ch.tenant AND ia.id_type=m.sender_id_type AND ia.id_value=m.sender_id_value
WHERE m.channel_id=? AND m.conversation_id=? AND m.sender_principal=? AND ia.principal_id=m.sender_principal AND ia.verified=1
AND m.availability='available' AND julianday(m.sent_at)>=julianday(?) AND `+predicate+`)`, args...).Scan(&found)
			if err != nil {
				return false, err
			}
			if found != 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// RuntimeBatchReady reports either duplicate pending work that needs one-time
// cleanup or a pending batch that has reached its count/time threshold.
func RuntimeBatchReady(ctx context.Context, q Queryer, value string, at time.Time) (bool, error) {
	c, err := ReadRuntime(ctx, q, value)
	if err != nil {
		return false, err
	}
	if c.Status != "running" {
		return false, nil
	}
	if available, e := PoolAvailable(ctx, q, c, "analysis"); e != nil || !available {
		return false, e
	}
	routes, e := eligibleAnalysisRoutes(ctx, q, c, at)
	if e != nil {
		return false, e
	}
	cutoff := at.UTC().Add(-time.Duration(c.MaxWaitSeconds) * time.Second).Format(time.RFC3339Nano)
	for _, routeID := range routes {
		route, routeErr := ReadRoute(ctx, q, routeID)
		if routeErr != nil {
			return false, routeErr
		}
		if route.Mode == "ignore" {
			continue
		}
		var duplicate int
		if c.ApplicationMode == "proactive" {
			err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runtime_message_states s WHERE s.runtime_id=? AND s.route_id=? AND s.state='pending' AND EXISTS (
 SELECT 1 FROM verified_message_associations a
 JOIN messages bot ON bot.id=CASE WHEN a.first_message_id=s.message_id THEN a.second_message_id ELSE a.first_message_id END
 JOIN runtime_configs g ON g.channel_id=bot.channel_id AND g.application_mode='group_mention' AND g.status='running'
 JOIN channel_routes gr ON gr.channel_id=g.channel_id AND gr.conversation_id=bot.conversation_id
 WHERE (a.first_message_id=s.message_id OR a.second_message_id=s.message_id)
 AND bot.addressed=1 AND bot.context_only=0 AND bot.availability='available'
 AND gr.status='active' AND gr.mode='assistant' AND gr.send_policy='reply_to_trigger'
 AND (gr.id=g.delivery_route_id OR gr.id IN (SELECT value FROM json_each(g.route_ids)))))`, c.ID, routeID).Scan(&duplicate)
			if err != nil {
				return false, err
			}
			if duplicate != 0 {
				return true, nil
			}
		}
		err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runtime_message_states s WHERE s.runtime_id=? AND s.route_id=? AND s.state='pending' AND EXISTS (
 SELECT 1 FROM verified_message_associations a WHERE
 (a.first_message_id=s.message_id OR a.second_message_id=s.message_id)
 AND a.claimed_message_id<>'' AND (a.claimed_message_id<>s.message_id OR a.claimed_runtime_id<>s.runtime_id)))`, c.ID, routeID).Scan(&duplicate)
		if err != nil {
			return false, err
		}
		if duplicate != 0 {
			return true, nil
		}
		var count int
		var earliest string
		if err = q.QueryRowContext(ctx, "SELECT count(*),coalesce(min(first_seen_at),'') FROM runtime_message_states WHERE runtime_id=? AND route_id=? AND state='pending'", c.ID, routeID).Scan(&count, &earliest); err != nil {
			return false, err
		}
		threshold := c.ItemThreshold
		if c.ApplicationMode == "direct" || c.ApplicationMode == "group_mention" || routeID == c.DeliveryRouteID {
			threshold = 1
		}
		if count > 0 && (count >= threshold || earliest <= cutoff) {
			return true, nil
		}
	}
	return false, nil
}

func runtimeHasActiveExecution(ctx context.Context, q Queryer, runtimeID string) (bool, error) {
	var active int
	err := q.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM runtime_attempts ra JOIN runtime_tasks rt ON rt.id=ra.task_id WHERE rt.runtime_id=? AND ra.status='running')+
(SELECT count(*) FROM runtime_action_attempts aa JOIN runtime_tasks rt ON rt.id=aa.task_id WHERE rt.runtime_id=? AND aa.status='running')`, runtimeID, runtimeID).Scan(&active)
	return active > 0, err
}

func RuntimeActionReady(ctx context.Context, q Queryer, value string) (bool, error) {
	c, err := ReadRuntime(ctx, q, value)
	if err != nil {
		return false, err
	}
	if c.Status != "running" {
		return false, nil
	}
	if available, e := PoolAvailable(ctx, q, c, "execution"); e != nil || !available {
		return false, e
	}
	if active, activeErr := runtimeHasActiveExecution(ctx, q, c.ID); activeErr != nil || (active && c.ApplicationMode != "proactive") {
		return false, activeErr
	}
	var found int
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runtime_pending_actions a JOIN runtime_tasks t ON t.id=a.task_id
WHERE t.runtime_id=? AND t.status='awaiting_confirmation' AND t.version=a.task_version AND a.status='confirmed'
AND NOT EXISTS(SELECT 1 FROM runtime_work_leases l WHERE l.task_id=t.id AND l.released=0)
AND t.route_id IN (SELECT value FROM json_each(?))
AND EXISTS (SELECT 1 FROM channel_routes r WHERE r.id=t.route_id AND r.channel_id=? AND r.status='active' AND r.mode<>'ignore'))`, c.ID, JSON(c.RouteIDs), c.ChannelID).Scan(&found)
	return found != 0, err
}

func RuntimeTaskReady(ctx context.Context, q Queryer, value string) (bool, error) {
	c, err := ReadRuntime(ctx, q, value)
	if err != nil {
		return false, err
	}
	if c.Status != "running" {
		return false, nil
	}
	if c.ApplicationMode == "proactive" {
		if available, e := PoolAvailable(ctx, q, c, "execution"); e != nil || !available {
			return false, e
		}
	} else if active, activeErr := runtimeHasActiveExecution(ctx, q, c.ID); activeErr != nil || active {
		return false, activeErr
	}
	var found int
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runtime_tasks WHERE runtime_id=? AND status='pending'
AND NOT EXISTS(SELECT 1 FROM runtime_work_leases l WHERE l.task_id=runtime_tasks.id AND l.released=0)
AND (kind<>'memory' OR (SELECT count(*) FROM runtime_work_leases l JOIN runtime_tasks mt ON mt.id=l.task_id WHERE l.released=0 AND mt.kind='memory')<2)
AND route_id IN (SELECT value FROM json_each(?))
AND EXISTS (SELECT 1 FROM channel_routes r WHERE r.id=runtime_tasks.route_id AND r.channel_id=? AND r.status='active' AND r.mode<>'ignore'))`, c.ID, JSON(c.RouteIDs), c.ChannelID).Scan(&found)
	return found != 0, err
}
