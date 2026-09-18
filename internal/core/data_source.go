package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
)

type DataSourceInput struct {
	Name                string   `json:"name"`
	Channel             string   `json:"channel"`
	Workspace           string   `json:"workspace_id"`
	ReconcileSeconds    int      `json:"reconcile_seconds,omitempty"`
	Ignore              []string `json:"ignore,omitempty"`
	MemberRobotCode     string   `json:"member_robot_code,omitempty"`
	HistoryEnabled      *bool    `json:"history_enabled,omitempty"`
	Enabled             *bool    `json:"enabled,omitempty"`
	HistoryDays         int      `json:"history_days,omitempty"`
	DirectEnabled       *bool    `json:"direct_enabled,omitempty"`
	BackfillAfterEnable *bool    `json:"backfill_after_enable,omitempty"`
	RetentionDays       int      `json:"retention_days,omitempty"`
}
type DataSource struct {
	ID                          string   `json:"id"`
	Name                        string   `json:"name"`
	ChannelID                   string   `json:"channel_id"`
	WorkspaceID                 string   `json:"workspace_id"`
	Status                      string   `json:"status"`
	ReconcileSeconds            int      `json:"reconcile_seconds"`
	RouteIDs                    []string `json:"route_ids"`
	Ignore                      []string `json:"ignore"`
	MemberRobotCode             string   `json:"member_robot_code,omitempty"`
	HistoryEnabled              bool     `json:"history_enabled"`
	Enabled                     bool     `json:"enabled"`
	HistoryDays                 int      `json:"history_days"`
	DirectEnabled               bool     `json:"direct_enabled"`
	DirectEnabledAt             string   `json:"direct_enabled_at,omitempty"`
	BackfillAfterEnable         bool     `json:"backfill_after_enable"`
	RetentionDays               int      `json:"retention_days"`
	LastDirectReceivedAt        string   `json:"last_direct_received_at,omitempty"`
	DirectDiscoveryCoveredUntil string   `json:"direct_discovery_covered_until,omitempty"`
	LastRetentionAt             string   `json:"last_retention_at,omitempty"`
	LastRetentionCount          int      `json:"last_retention_count"`
	RetentionErrorCode          string   `json:"retention_error_code,omitempty"`
	ReconcileCursor             int      `json:"reconcile_cursor"`
	LastReconciledAt            string   `json:"last_reconciled_at,omitempty"`
	LastErrorCode               string   `json:"last_error_code,omitempty"`
	Version                     int      `json:"version"`
	CreatedAt                   string   `json:"created_at"`
	UpdatedAt                   string   `json:"updated_at"`
}

var dataSourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

const dataSourceColumns = "id,name,channel_id,workspace_id,status,reconcile_seconds,route_ids,reconcile_cursor,last_reconciled_at,last_error_code,version,created_at,updated_at,ignore_rules,member_robot_code,history_enabled,history_days,enabled,direct_enabled,direct_enabled_at,backfill_after_enable,retention_days,last_direct_received_at,direct_discovery_covered_until,last_retention_at,last_retention_count,retention_error_code"

func scanDataSource(row scanner) (DataSource, error) {
	var d DataSource
	var routes, ignore string
	var historyEnabled, enabled, directEnabled, backfill int
	err := row.Scan(&d.ID, &d.Name, &d.ChannelID, &d.WorkspaceID, &d.Status, &d.ReconcileSeconds, &routes, &d.ReconcileCursor, &d.LastReconciledAt, &d.LastErrorCode, &d.Version, &d.CreatedAt, &d.UpdatedAt, &ignore, &d.MemberRobotCode, &historyEnabled, &d.HistoryDays, &enabled, &directEnabled, &d.DirectEnabledAt, &backfill, &d.RetentionDays, &d.LastDirectReceivedAt, &d.DirectDiscoveryCoveredUntil, &d.LastRetentionAt, &d.LastRetentionCount, &d.RetentionErrorCode)
	if err != nil {
		return d, err
	}
	if err = json.Unmarshal([]byte(routes), &d.RouteIDs); err != nil {
		return d, err
	}
	err = json.Unmarshal([]byte(ignore), &d.Ignore)
	d.HistoryEnabled = historyEnabled == 1
	d.Enabled = enabled == 1
	d.DirectEnabled = directEnabled == 1
	d.BackfillAfterEnable = backfill == 1
	return d, err
}
func ReadDataSource(ctx context.Context, q Queryer, value string) (DataSource, error) {
	d, err := scanDataSource(q.QueryRowContext(ctx, "SELECT "+dataSourceColumns+" FROM data_sources WHERE id=? OR name=?", value, value))
	if errors.Is(err, sql.ErrNoRows) {
		return d, Fail("not_found", "data source not found")
	}
	return d, err
}
func DataSourceList(ctx context.Context, q Queryer) ([]DataSource, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+dataSourceColumns+" FROM data_sources ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DataSource{}
	for rows.Next() {
		d, e := scanDataSource(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func ReadDataSourceByChannel(ctx context.Context, q Queryer, channelID string) (DataSource, error) {
	d, err := scanDataSource(q.QueryRowContext(ctx, "SELECT "+dataSourceColumns+" FROM data_sources WHERE channel_id=?", channelID))
	if errors.Is(err, sql.ErrNoRows) {
		return d, Fail("not_found", "data source not found")
	}
	return d, err
}
func (tx *Tx) ConfigureDataSource(ctx context.Context, in DataSourceInput) (DataSource, error) {
	if !dataSourceName.MatchString(in.Name) {
		return DataSource{}, Fail("invalid_input", "invalid data source name")
	}
	if in.ReconcileSeconds == 0 {
		in.ReconcileSeconds = 300
	}
	if in.ReconcileSeconds < 10 || in.ReconcileSeconds > 86400 {
		return DataSource{}, Fail("invalid_input", "reconciliation must be 10..86400 seconds")
	}
	if in.HistoryDays == 0 {
		in.HistoryDays = 30
	}
	if in.HistoryDays < 1 || in.HistoryDays > 30 {
		return DataSource{}, Fail("invalid_input", "history days must be 1..30")
	}
	if in.RetentionDays == 0 {
		in.RetentionDays = 7
	}
	if in.RetentionDays < 1 || in.RetentionDays > 30 {
		return DataSource{}, Fail("invalid_input", "retention days must be 1..30")
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	historyEnabled := true
	if in.HistoryEnabled != nil {
		historyEnabled = *in.HistoryEnabled
	}
	directEnabled := false
	if in.DirectEnabled != nil {
		directEnabled = *in.DirectEnabled
	}
	backfill := true
	if in.BackfillAfterEnable != nil {
		backfill = *in.BackfillAfterEnable
	}
	if directEnabled && historyEnabled {
		return DataSource{}, Fail("invalid_input", "direct incremental collection requires history_import.enabled=false")
	}
	seenIgnore := map[string]bool{}
	for _, v := range in.Ignore {
		if strings.TrimSpace(v) == "" || len(v) > 200 || strings.ContainsAny(v, "\r\n\x00") || seenIgnore[v] {
			return DataSource{}, Fail("invalid_input", "ignore rules must be unique group IDs or names")
		}
		seenIgnore[v] = true
	}
	sort.Strings(in.Ignore)
	c, err := ReadChannel(ctx, tx.Conn, in.Channel)
	if err != nil {
		return DataSource{}, err
	}
	if c.Kind != ChannelDwsPersonal {
		return DataSource{}, Fail("invalid_input", "data source requires a personal DWS channel")
	}
	if in.MemberRobotCode != "" && (c.Identity.DeliveryRobotCode == "" || c.Identity.DeliveryRobotCode != in.MemberRobotCode || c.Identity.DeliveryRobotName == "") {
		return DataSource{}, Fail("denied", "data source DWS identity must be bound to the selected delivery robot")
	}
	var workspace string
	err = tx.Conn.QueryRowContext(ctx, "SELECT id FROM workspaces WHERE id=? OR name=?", in.Workspace, in.Workspace).Scan(&workspace)
	if err != nil {
		return DataSource{}, Fail("invalid_input", "data source workspace must exist")
	}
	// A channel may retain broad legacy routes. A new source has no verified
	// acquisition scope until discovery explicitly adopts positive groups.
	now := Now()
	id := NewID()
	existing, err := ReadDataSource(ctx, tx.Conn, in.Name)
	if err == nil {
		if existing.Status != "stopped" {
			return DataSource{}, Fail("conflict", "stop data source before reconfiguration")
		}
		if existing.ChannelID != c.ID {
			return DataSource{}, Fail("denied", "data source channel identity cannot change")
		}
		id = existing.ID
		directEnabledAt := existing.DirectEnabledAt
		if directEnabled && !existing.DirectEnabled {
			directEnabledAt = now
		}
		directCovered := existing.DirectDiscoveryCoveredUntil
		if directEnabled && !existing.DirectEnabled {
			directCovered = directEnabledAt
		}
		_, err = tx.Conn.ExecContext(ctx, "UPDATE data_sources SET workspace_id=?,reconcile_seconds=?,ignore_rules=?,member_robot_code=?,history_enabled=?,history_days=?,enabled=?,direct_enabled=?,direct_enabled_at=?,direct_discovery_covered_until=?,backfill_after_enable=?,retention_days=?,version=version+1,updated_at=? WHERE id=?", workspace, in.ReconcileSeconds, JSON(in.Ignore), in.MemberRobotCode, boolInt(historyEnabled), in.HistoryDays, boolInt(enabled), boolInt(directEnabled), directEnabledAt, directCovered, boolInt(backfill), in.RetentionDays, now, id)
	} else if ErrorCode(err) == "not_found" {
		directEnabledAt := ""
		if directEnabled {
			directEnabledAt = now
		}
		_, err = tx.Conn.ExecContext(ctx, "INSERT INTO data_sources(id,name,channel_id,workspace_id,reconcile_seconds,route_ids,ignore_rules,member_robot_code,history_enabled,history_days,enabled,direct_enabled,direct_enabled_at,direct_discovery_covered_until,backfill_after_enable,retention_days,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", id, in.Name, c.ID, workspace, in.ReconcileSeconds, "[]", JSON(in.Ignore), in.MemberRobotCode, boolInt(historyEnabled), in.HistoryDays, boolInt(enabled), boolInt(directEnabled), directEnabledAt, directEnabledAt, boolInt(backfill), in.RetentionDays, now, now)
	}
	if err != nil {
		return DataSource{}, err
	}
	return ReadDataSource(ctx, tx.Conn, id)
}
func (tx *Tx) SetDataSourceStatus(ctx context.Context, value, status string) (DataSource, error) {
	if !contains([]string{"running", "paused", "stopped"}, status) {
		return DataSource{}, Fail("invalid_input", "invalid data source status")
	}
	d, err := ReadDataSource(ctx, tx.Conn, value)
	if err != nil {
		return d, err
	}
	if status == "running" && !d.Enabled {
		return d, Fail("denied", "disabled data source cannot be started")
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE data_sources SET status=?,updated_at=? WHERE id=?", status, Now(), d.ID)
	if err != nil {
		return d, err
	}
	return ReadDataSource(ctx, tx.Conn, d.ID)
}
func DataSourceForRuntime(ctx context.Context, q Queryer, runtimeID string) (DataSource, bool, error) {
	var id string
	err := q.QueryRowContext(ctx, "SELECT data_source_id FROM runtime_data_sources WHERE runtime_id=?", runtimeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return DataSource{}, false, nil
	}
	if err != nil {
		return DataSource{}, false, err
	}
	d, err := ReadDataSource(ctx, q, id)
	return d, true, err
}
func (tx *Tx) BindRuntimeDataSource(ctx context.Context, runtimeValue, sourceValue string) (DataSource, error) {
	r, err := ReadRuntime(ctx, tx.Conn, runtimeValue)
	if err != nil {
		return DataSource{}, err
	}
	d, err := ReadDataSource(ctx, tx.Conn, sourceValue)
	if err != nil {
		return d, err
	}
	if r.Status != "stopped" {
		return d, Fail("conflict", "stop runtime before moving collection to an independent source")
	}
	if r.ChannelID != d.ChannelID {
		return d, Fail("denied", "runtime and source must use the same DWS channel")
	}
	lease, err := ReadLease(ctx, tx.Conn, d.ChannelID)
	if err != nil {
		return d, err
	}
	if lease.Held {
		return d, Fail("conflict", "wait for the existing receiver to release its channel lease")
	}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_data_sources(runtime_id,data_source_id,created_at) VALUES(?,?,?) ON CONFLICT(runtime_id) DO UPDATE SET data_source_id=excluded.data_source_id", r.ID, d.ID, Now())
	return d, err
}

// SyncDataSourceGroups applies a complete discovery result. Ignore routes remain
// exclusions; historical routes remain available for audit but leave live scope.
type DataSourceGroup struct {
	ID   string
	Name string
}

func (tx *Tx) SyncDataSourceGroups(ctx context.Context, value string, conversations []string) (DataSource, error) {
	groups := make([]DataSourceGroup, 0, len(conversations))
	for _, id := range conversations {
		groups = append(groups, DataSourceGroup{ID: id})
	}
	return tx.SyncDataSourceGroupDetails(ctx, value, groups)
}
func (tx *Tx) SyncDataSourceGroupDetails(ctx context.Context, value string, conversations []DataSourceGroup) (DataSource, error) {
	d, err := ReadDataSource(ctx, tx.Conn, value)
	if err != nil {
		return d, err
	}
	known, err := RouteList(ctx, tx.Conn, d.ChannelID)
	if err != nil {
		return d, err
	}
	byConversation := map[string]Route{}
	for _, route := range known {
		byConversation[route.ConversationID] = route
	}
	routes := []string{}
	// Group discovery owns only group membership. Direct routes are created from
	// verified direct events/discovery and survive every group refresh.
	sourceRoutes := map[string]bool{}
	for _, id := range d.RouteIDs {
		sourceRoutes[id] = true
	}
	for _, existing := range known {
		if sourceRoutes[existing.ID] && existing.ConversationType == "direct" && existing.Status == "active" && existing.Mode != "ignore" {
			routes = append(routes, existing.ID)
		}
	}
	seen := map[string]bool{}
	ignored := map[string]bool{}
	for _, rule := range d.Ignore {
		ignored[rule] = true
	}
	for _, group := range conversations {
		cid := group.ID
		if cid == "" {
			return d, Fail("invalid_input", "discovered group lacks an ID")
		}
		if seen[cid] {
			continue
		}
		seen[cid] = true
		r, exists := byConversation[cid]
		var e error
		if !exists {
			mode := "collect"
			if ignored[cid] || ignored[group.Name] {
				mode = "ignore"
			}
			r, e = tx.AddRoute(ctx, d.ChannelID, RouteInput{ConversationID: cid, ConversationType: "group", Workspace: d.WorkspaceID, Mode: mode, MemoryPolicy: "explicit_only", SendPolicy: "draft_only"})
		} else if (ignored[cid] || ignored[group.Name]) && r.Mode != "ignore" {
			_, e = tx.UpdateRoute(ctx, r.ID, r.Version, RouteInput{Mode: "ignore"}, "source ignore rule")
			if e == nil {
				r, e = ReadRoute(ctx, tx.Conn, r.ID)
			}
		}
		if e != nil {
			return d, e
		}
		if r.Status == "active" && r.Mode != "ignore" {
			routes = append(routes, r.ID)
		}
	}
	// The source owns group acquisition. The bound direct route remains on the
	// channel for outbound owner delivery; application Stream receives owner
	// private confirmations on its separately verified direct route.
	raw, _ := json.Marshal(routes)
	_, err = tx.Conn.ExecContext(ctx, "UPDATE data_sources SET route_ids=?,updated_at=? WHERE id=?", string(raw), Now(), d.ID)
	if err != nil {
		return d, err
	}
	return ReadDataSource(ctx, tx.Conn, d.ID)
}
func (tx *Tx) CheckpointDataSource(ctx context.Context, id string, cursor int, errorCode string) error {
	_, err := tx.Conn.ExecContext(ctx, "UPDATE data_sources SET reconcile_cursor=?,last_reconciled_at=?,last_error_code=CASE WHEN last_error_code LIKE 'discovery_%' THEN last_error_code ELSE ? END,updated_at=? WHERE id=?", cursor, Now(), errorCode, Now(), id)
	return err
}
