package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

type SourceGroupDiscovery struct {
	Complete       bool              `json:"complete"`
	Valid          bool              `json:"valid"`
	SourceID       string            `json:"source_id"`
	SourceVersion  int               `json:"source_version"`
	ChannelVersion int               `json:"channel_version"`
	RobotCode      string            `json:"robot_code"`
	RobotName      string            `json:"robot_name"`
	Groups         []DataSourceGroup `json:"groups"`
	ObservedAt     string            `json:"observed_at"`
}

// RecordSourceGroupDiscovery is called only after a complete successful provider
// discovery, with the frozen versions used for that request. Configured route IDs
// and successful history pulls alone cannot create this authorization evidence.
func (tx *Tx) RecordSourceGroupDiscovery(ctx context.Context, sourceID string, sourceVersion, channelVersion int, groups []DataSourceGroup) error {
	return tx.RecordSourceGroupObservation(ctx, sourceID, sourceVersion, channelVersion, groups, true)
}

// Partial receipts contain only independently verified positives from this call.
func (tx *Tx) RecordSourceGroupObservation(ctx context.Context, sourceID string, sourceVersion, channelVersion int, groups []DataSourceGroup, complete bool) error {
	if !complete && len(groups) == 0 {
		return tx.InvalidateSourceGroupDiscovery(ctx, sourceID)
	}
	source, err := ReadDataSource(ctx, tx.Conn, sourceID)
	if err != nil {
		return err
	}
	channel, err := ReadChannel(ctx, tx.Conn, source.ChannelID)
	if err != nil {
		return err
	}
	if source.Version != sourceVersion || channel.ConfigVersion != channelVersion {
		return Fail("conflict", "group discovery configuration changed during provider request")
	}
	if source.MemberRobotCode != "" && (source.MemberRobotCode != channel.Identity.DeliveryRobotCode || channel.Identity.DeliveryRobotName == "") {
		return nil
	}
	if !channel.Capabilities.Verified["receive"] || !channel.Capabilities.Verified["history"] {
		return Fail("denied", "group discovery requires a verified DWS channel")
	}
	unique := map[string]DataSourceGroup{}
	for _, group := range groups {
		if !identifier.MatchString(group.ID) {
			return Fail("invalid_input", "invalid discovered conversation identifier")
		}
		if prior, ok := unique[group.ID]; ok && prior.Name != group.Name {
			return Fail("invalid_input", "ambiguous discovered conversation names")
		}
		unique[group.ID] = group
	}
	normalized := make([]DataSourceGroup, 0, len(unique))
	for _, group := range unique {
		normalized = append(normalized, group)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].ID < normalized[j].ID })
	_, err = tx.Conn.ExecContext(ctx, `INSERT INTO source_group_discoveries(source_id,source_version,channel_version,robot_code,robot_name,groups_json,observed_at,valid,complete) VALUES(?,?,?,?,?,?,?,1,?) ON CONFLICT(source_id) DO UPDATE SET source_version=excluded.source_version,channel_version=excluded.channel_version,robot_code=excluded.robot_code,robot_name=excluded.robot_name,groups_json=excluded.groups_json,observed_at=excluded.observed_at,valid=1,complete=excluded.complete`, source.ID, source.Version, channel.ConfigVersion, source.MemberRobotCode, sourceDiscoveryRobotName(source, channel), JSON(normalized), Now(), boolInt(complete))
	return err
}
func ReadSourceGroupDiscovery(ctx context.Context, q Queryer, sourceID string) (SourceGroupDiscovery, error) {
	var receipt SourceGroupDiscovery
	var groups string
	err := q.QueryRowContext(ctx, "SELECT source_id,source_version,channel_version,robot_code,robot_name,groups_json,observed_at,valid,complete FROM source_group_discoveries WHERE source_id=?", sourceID).Scan(&receipt.SourceID, &receipt.SourceVersion, &receipt.ChannelVersion, &receipt.RobotCode, &receipt.RobotName, &groups, &receipt.ObservedAt, &receipt.Valid, &receipt.Complete)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, Fail("not_found", "source group discovery has not completed")
	}
	if err != nil {
		return receipt, err
	}
	err = json.Unmarshal([]byte(groups), &receipt.Groups)
	return receipt, err
}

// ReadSourcePositiveProcessingRoutes limits an AI consumer to source routes
// backed by a recent, version- and robot-bound positive group observation.
// Partial observations can leave legacy routes in the source for collection;
// those routes alone do not authorize AI processing. A running source with a
// transient invalid observation can continue only within its bounded core
// fallback scope. Missing or unusable evidence yields an empty scope.
func ReadSourcePositiveProcessingRoutes(ctx context.Context, q Queryer, source DataSource, now time.Time) ([]string, error) {
	receipt, err := ReadSourceGroupDiscovery(ctx, q, source.ID)
	if ErrorCode(err) == "not_found" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	channel, err := ReadChannel(ctx, q, source.ChannelID)
	if err != nil {
		return nil, err
	}
	if !source.Enabled || source.Status == "paused" || channel.Kind != ChannelDwsPersonal || !channel.Capabilities.Verified["receive"] || !channel.Capabilities.Verified["history"] || source.Version != receipt.SourceVersion || channel.ConfigVersion != receipt.ChannelVersion || !sourceDiscoveryFilterCurrent(source, channel, receipt) {
		return nil, nil
	}
	observed, err := time.Parse(time.RFC3339Nano, receipt.ObservedAt)
	if err != nil || observed.After(now) {
		return nil, nil
	}
	if !receipt.Valid {
		fallback, fallbackErr := ReadSourceFallbackScope(ctx, q, source.ID, now)
		if fallbackErr != nil {
			if ErrorCode(fallbackErr) == "denied" {
				return nil, nil
			}
			return nil, fallbackErr
		}
		source = fallback
	} else {
		lifetime := time.Duration(source.ReconcileSeconds*2) * time.Second
		if lifetime < 10*time.Minute {
			lifetime = 10 * time.Minute
		}
		if lifetime > time.Hour {
			lifetime = time.Hour
		}
		if now.Sub(observed) > lifetime {
			return nil, nil
		}
	}
	proved := map[string]DataSourceGroup{}
	for _, group := range receipt.Groups {
		proved[group.ID] = group
	}
	ignored := map[string]bool{}
	for _, rule := range source.Ignore {
		ignored[rule] = true
	}
	ids := make([]string, 0, len(source.RouteIDs))
	seen := map[string]bool{}
	for _, id := range source.RouteIDs {
		if seen[id] {
			return nil, Fail("invalid_input", "data source scope contains duplicate routes")
		}
		seen[id] = true
		route, readErr := ReadRoute(ctx, q, id)
		if readErr != nil || route.ChannelID != source.ChannelID {
			return nil, Fail("denied", "data source scope contains a route outside its group channel")
		}
		if route.ConversationType == "direct" {
			continue // private collection never grants a group Agent evidence scope
		}
		if route.ConversationType != "group" {
			return nil, Fail("denied", "data source scope contains an unsupported conversation type")
		}
		group, hasProof := proved[route.ConversationID]
		if !hasProof || ignored[group.ID] || ignored[group.Name] {
			continue
		}
		updated, parseErr := time.Parse(time.RFC3339Nano, route.UpdatedAt)
		if parseErr != nil || updated.After(observed) {
			continue // route changed after the positive observation
		}
		if route.Status == "active" && route.Mode != "ignore" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ReadOwnerSourceProcessingRoutes extends the positively proved group scope
// with direct routes admitted after the explicit direct-enable boundary. It is
// for the owner proactive runtime only; group agents continue to call the group
// proof function above and cannot receive private routes.
func ReadOwnerSourceProcessingRoutes(ctx context.Context, q Queryer, source DataSource, now time.Time) ([]string, error) {
	groups, err := ReadSourcePositiveProcessingRoutes(ctx, q, source, now)
	if err != nil {
		return nil, err
	}
	if !source.Enabled || !source.DirectEnabled || source.Status == "paused" {
		return groups, nil
	}
	channel, err := ReadChannel(ctx, q, source.ChannelID)
	if err != nil {
		return nil, err
	}
	if channel.Kind != ChannelDwsPersonal || !channel.Capabilities.Verified["receive"] || !channel.Capabilities.Verified["history"] {
		return groups, nil
	}
	enabledAt, err := time.Parse(time.RFC3339, source.DirectEnabledAt)
	if err != nil || enabledAt.After(now) {
		return groups, nil
	}
	seen := map[string]bool{}
	for _, id := range groups {
		seen[id] = true
	}
	for _, id := range source.RouteIDs {
		if seen[id] {
			continue
		}
		route, readErr := ReadRoute(ctx, q, id)
		if readErr != nil || route.ChannelID != source.ChannelID {
			return nil, Fail("denied", "data source scope contains a route outside its channel")
		}
		if route.ConversationType != "direct" || route.Status != "active" || route.Mode == "ignore" {
			continue
		}
		var observed string
		if e := q.QueryRowContext(ctx, "SELECT observed_at FROM direct_conversation_contacts WHERE channel_id=? AND conversation_id=?", source.ChannelID, route.ConversationID).Scan(&observed); e != nil {
			continue
		}
		stamp, parseErr := time.Parse(time.RFC3339, observed)
		if parseErr == nil && !stamp.Before(enabledAt) {
			groups = append(groups, id)
			seen[id] = true
		}
	}
	sort.Strings(groups)
	return groups, nil
}

// Failed or ambiguous discovery cannot authorize new mounts using an older
// successful observation. Keep the old evidence for inspection and mark it invalid.
func (tx *Tx) InvalidateSourceGroupDiscovery(ctx context.Context, sourceID string) error {
	_, err := tx.Conn.ExecContext(ctx, "UPDATE source_group_discoveries SET valid=0 WHERE source_id=?", sourceID)
	return err
}

// ApplySourceGroupObservation preserves old collection routes on partial reads.
// The separately written receipt contains only fresh positives, never this union.
func (tx *Tx) ApplySourceGroupObservation(ctx context.Context, id string, groups []DataSourceGroup, complete bool, excluded []string) (DataSource, error) {
	if complete {
		return tx.SyncDataSourceGroupDetails(ctx, id, groups)
	}
	source, err := ReadDataSource(ctx, tx.Conn, id)
	if err != nil {
		return source, err
	}
	excludedSet := map[string]bool{}
	for _, id := range excluded {
		excludedSet[id] = true
	}
	byID := map[string]DataSourceGroup{}
	for _, group := range groups {
		if !excludedSet[group.ID] {
			byID[group.ID] = group
		}
	}
	for _, routeID := range source.RouteIDs {
		route, e := ReadRoute(ctx, tx.Conn, routeID)
		if e != nil {
			return source, e
		}
		if route.ConversationType == "group" && !excludedSet[route.ConversationID] {
			if _, ok := byID[route.ConversationID]; !ok {
				byID[route.ConversationID] = DataSourceGroup{ID: route.ConversationID}
			}
		}
	}
	merged := make([]DataSourceGroup, 0, len(byID))
	for _, group := range byID {
		merged = append(merged, group)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return tx.SyncDataSourceGroupDetails(ctx, id, merged)
}

// ReadSourceFallbackScope permits bounded collection continuity, never a new
// mount or fresh discovery receipt. Invalid only means the latest lookup failed;
// revoked evidence is cleared by RevokeSourceGroupDiscovery below.
func ReadSourceFallbackScope(ctx context.Context, q Queryer, sourceID string, now time.Time) (DataSource, error) {
	source, err := ReadDataSource(ctx, q, sourceID)
	if err != nil {
		return source, err
	}
	receipt, err := ReadSourceGroupDiscovery(ctx, q, sourceID)
	if err != nil {
		return source, err
	}
	channel, err := ReadChannel(ctx, q, source.ChannelID)
	if err != nil {
		return source, err
	}
	if !source.Enabled || source.Status != "running" || (channel.Status != "configured" && channel.Status != "active") || channel.Kind != ChannelDwsPersonal || !channel.Capabilities.Verified["receive"] || !channel.Capabilities.Verified["history"] || source.Version != receipt.SourceVersion || channel.ConfigVersion != receipt.ChannelVersion || !sourceDiscoveryFilterCurrent(source, channel, receipt) {
		return source, Fail("denied", "previous discovery no longer matches collection authority")
	}
	observed, err := time.Parse(time.RFC3339Nano, receipt.ObservedAt)
	if err != nil || observed.After(now) || now.Sub(observed) > 24*time.Hour {
		return source, Fail("denied", "previous positive discovery is too old for collection fallback")
	}
	groups := map[string]DataSourceGroup{}
	for _, group := range receipt.Groups {
		groups[group.ID] = group
	}
	ignored := map[string]bool{}
	for _, value := range source.Ignore {
		ignored[value] = true
	}
	allowed := []string{}
	for _, id := range source.RouteIDs {
		route, readErr := ReadRoute(ctx, q, id)
		if readErr != nil {
			return source, readErr
		}
		// A source adopts preconfigured routes without moving their workspace.
		// source.WorkspaceID is only the default for newly discovered routes;
		// the unchanged route remains the authority for collection placement.
		group, proved := groups[route.ConversationID]
		updated, parseErr := time.Parse(time.RFC3339Nano, route.UpdatedAt)
		if !proved || route.ChannelID != source.ChannelID || route.ConversationType != "group" || route.Status != "active" || route.Mode == "ignore" || ignored[group.ID] || ignored[group.Name] || parseErr != nil || updated.After(observed) {
			continue
		}
		allowed = append(allowed, id)
	}
	source.RouteIDs = allowed
	if len(allowed) == 0 {
		return source, Fail("denied", "no existing routes retain usable positive discovery proof")
	}
	return source, nil
}

// A permanent rejection must not become fallback permission after a later
// timeout. This removes only the latest reusable membership set, not sources,
// route audit history or the original observation timestamp.
func (tx *Tx) RevokeSourceGroupDiscovery(ctx context.Context, sourceID string) error {
	_, err := tx.Conn.ExecContext(ctx, "UPDATE source_group_discoveries SET valid=0,groups_json='[]' WHERE source_id=?", sourceID)
	return err
}

// An optional robot membership filter is part of the frozen discovery scope.
// Removing it in configuration invalidates old receipts rather than widening them.
func sourceDiscoveryRobotName(source DataSource, channel Channel) string {
	if source.MemberRobotCode == "" {
		return ""
	}
	return channel.Identity.DeliveryRobotName
}
func sourceDiscoveryFilterCurrent(source DataSource, channel Channel, receipt SourceGroupDiscovery) bool {
	if source.MemberRobotCode == "" {
		return receipt.RobotCode == "" && receipt.RobotName == ""
	}
	return source.MemberRobotCode == channel.Identity.DeliveryRobotCode && source.MemberRobotCode == receipt.RobotCode && channel.Identity.DeliveryRobotName != "" && channel.Identity.DeliveryRobotName == receipt.RobotName
}
