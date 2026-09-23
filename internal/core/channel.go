package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Channel kinds are separate authorization namespaces. A personal dws account and
// an application robot never share credentials, visible scope or ID namespaces.
const (
	ChannelDwsPersonal = "dws_personal"
	ChannelDingTalkApp = "dingtalk_app"
)

type ChannelIdentity struct {
	Profile                  string `json:"profile,omitempty" yaml:"profile,omitempty"`
	ExpectedCorpID           string `json:"expected_corp_id" yaml:"expected_corp_id"`
	ExpectedUserID           string `json:"expected_user_id,omitempty" yaml:"expected_user_id,omitempty"`
	ClientID                 string `json:"client_id,omitempty" yaml:"client_id,omitempty"`
	RobotCode                string `json:"robot_code,omitempty" yaml:"robot_code,omitempty"`
	DeliveryRobotCode        string `json:"delivery_robot_code,omitempty" yaml:"delivery_robot_code,omitempty"`
	DeliveryRobotName        string `json:"delivery_robot_name,omitempty" yaml:"delivery_robot_name,omitempty"`
	HistoryChannel           string `json:"history_channel,omitempty" yaml:"history_channel,omitempty"`
	ConfirmationCardTemplate string `json:"confirmation_card_template,omitempty" yaml:"confirmation_card_template,omitempty"`
}
type Subscription struct {
	Kind           string   `json:"kind,omitempty" yaml:"kind,omitempty"`
	ConversationID string   `json:"conversation_id,omitempty" yaml:"conversation_id,omitempty"`
	UserID         string   `json:"user_id,omitempty" yaml:"user_id,omitempty"`
	Events         []string `json:"events,omitempty" yaml:"events,omitempty"`
}
type RouteInput struct {
	ConversationID   string   `json:"conversation_id" yaml:"conversation_id"`
	ConversationType string   `json:"conversation_type,omitempty" yaml:"conversation_type,omitempty"`
	Workspace        string   `json:"workspace_id,omitempty" yaml:"workspace_id,omitempty"`
	Mode             string   `json:"mode,omitempty" yaml:"mode,omitempty"`
	Triggers         []string `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	AudiencePolicy   string   `json:"audience_policy,omitempty" yaml:"audience_policy,omitempty"`
	SendPolicy       string   `json:"send_policy,omitempty" yaml:"send_policy,omitempty"`
	Retention        string   `json:"retention,omitempty" yaml:"retention,omitempty"`
}
type ChannelInput struct {
	SchemaVersion int             `json:"schema_version,omitempty" yaml:"schema_version,omitempty"`
	Name          string          `json:"name" yaml:"name"`
	Kind          string          `json:"kind" yaml:"kind"`
	Provider      string          `json:"provider,omitempty" yaml:"provider,omitempty"`
	Tenant        string          `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	Identity      ChannelIdentity `json:"identity" yaml:"identity"`
	CredentialRef string          `json:"credential_ref,omitempty" yaml:"credential_ref,omitempty"`
	Transport     string          `json:"transport,omitempty" yaml:"transport,omitempty"`
	Subscription  *Subscription   `json:"subscription,omitempty" yaml:"subscription,omitempty"`
	Route         *RouteInput     `json:"route,omitempty" yaml:"route,omitempty"`
}
type Channel struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Kind          string          `json:"kind"`
	Provider      string          `json:"provider"`
	Tenant        string          `json:"tenant"`
	AuthNamespace string          `json:"auth_namespace"`
	IDNamespace   string          `json:"id_namespace"`
	Identity      ChannelIdentity `json:"identity"`
	CredentialRef string          `json:"credential_ref,omitempty"`
	Capabilities  Capabilities    `json:"capabilities"`
	ToolVersion   string          `json:"tool_version,omitempty"`
	ConfigVersion int             `json:"config_version"`
	Status        string          `json:"status"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	Routes        []Route         `json:"routes,omitempty"`
}
type Route struct {
	ID               string   `json:"id"`
	ChannelID        string   `json:"channel_id"`
	ConversationID   string   `json:"conversation_id"`
	ConversationType string   `json:"conversation_type"`
	WorkspaceID      string   `json:"workspace_id"`
	Mode             string   `json:"mode"`
	Triggers         []string `json:"triggers"`
	AudiencePolicy   string   `json:"audience_policy"`
	AudienceKey      string   `json:"audience_key"`
	MemoryPolicy     string   `json:"-" yaml:"-"`
	SendPolicy       string   `json:"send_policy"`
	ApprovalDisplay  string   `json:"approval_display"`
	Retention        string   `json:"retention,omitempty"`
	Version          int      `json:"version"`
	Status           string   `json:"status"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
}

// Capabilities record what was actually verified, never what a help text implies.
// An unverified capability is treated as absent.
type Capabilities struct {
	Verified   map[string]bool `json:"verified"`
	Unverified []string        `json:"unverified"`
	CheckedAt  string          `json:"checked_at,omitempty"`
	Tool       string          `json:"tool,omitempty"`
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9._:$+=/-]{1,200}$`)
var channelName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var channelKind = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,62}$`)
var providerName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,62}$`)

// A credential reference names a secret held by the OS keychain or an equally
// restricted store. An inline secret is refused so SQLite never holds one.
func validateCredentialRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !strings.HasPrefix(ref, "keychain://") && !strings.HasPrefix(ref, "env://") && !strings.HasPrefix(ref, "file://") {
		return Fail("invalid_input", "credential_ref must use keychain://, env:// or file://; inline secrets are refused")
	}
	if len(ref) > 300 {
		return Fail("invalid_input", "credential_ref is too long")
	}
	for _, marker := range []string{"secret=", "password=", "token="} {
		if strings.Contains(strings.ToLower(ref), marker) {
			return Fail("invalid_input", "credential_ref must not embed a secret value")
		}
	}
	return nil
}

// audienceKey names one disclosure boundary. Every key is scoped to exactly one
// channel and conversation, including the private default: a publication is a
// permission for one audience, and a shared key would silently turn it into a
// permission for every conversation on every channel.
//
// The policy stays in the key so widening a route from private to conversation
// still requires its own publication rather than inheriting the earlier one.
func audienceKey(policy, channel, conversation string) string {
	if policy == "" {
		policy = "local_private"
	}
	return policy + ":" + channel + ":" + conversation
}

func normalizeChannel(in ChannelInput) (Channel, error) {
	if in.SchemaVersion != 0 && in.SchemaVersion != 1 {
		return Channel{}, Fail("invalid_input", "unsupported channel schema_version %d", in.SchemaVersion)
	}
	if !channelName.MatchString(in.Name) {
		return Channel{}, Fail("invalid_input", "channel name must be lowercase letters, digits and dashes")
	}
	if !channelKind.MatchString(in.Kind) {
		return Channel{}, Fail("invalid_input", "channel kind is invalid")
	}
	if err := validateCredentialRef(in.CredentialRef); err != nil {
		return Channel{}, err
	}
	provider := strings.TrimSpace(in.Provider)
	tenant := strings.TrimSpace(in.Tenant)
	if contains([]string{ChannelDwsPersonal, ChannelDingTalkApp}, in.Kind) {
		provider = "dingtalk"
		if !identifier.MatchString(in.Identity.ExpectedCorpID) {
			return Channel{}, Fail("invalid_input", "identity.expected_corp_id is required")
		}
		tenant = in.Identity.ExpectedCorpID
	} else {
		if provider == "" {
			provider = in.Kind
		}
		if !providerName.MatchString(provider) {
			return Channel{}, Fail("invalid_input", "provider is invalid")
		}
		if tenant == "" {
			tenant = in.Identity.ExpectedCorpID
			if tenant == "" {
				tenant = in.Identity.Profile
			}
		}
		if !identifier.MatchString(tenant) {
			return Channel{}, Fail("invalid_input", "generic channel requires tenant or identity.profile")
		}
	}
	c := Channel{ID: NewID(), Name: in.Name, Kind: in.Kind, Provider: provider, Tenant: tenant,
		Identity: in.Identity, CredentialRef: in.CredentialRef, ConfigVersion: 1, Status: "configured",
		Capabilities: Capabilities{Verified: map[string]bool{}, Unverified: []string{}}, CreatedAt: Now(), UpdatedAt: Now()}
	switch in.Kind {
	case ChannelDwsPersonal:
		if !identifier.MatchString(in.Identity.ExpectedUserID) {
			return c, Fail("invalid_input", "dws_personal requires identity.expected_user_id")
		}
		if in.Identity.Profile == "" {
			in.Identity.Profile = in.Identity.ExpectedCorpID + ":" + in.Identity.ExpectedUserID
			c.Identity = in.Identity
		}
		if !identifier.MatchString(in.Identity.Profile) {
			return c, Fail("invalid_input", "identity.profile is invalid")
		}
		if in.Identity.RobotCode != "" || in.Identity.ClientID != "" || in.Identity.HistoryChannel != "" || in.Identity.ConfirmationCardTemplate != "" {
			return c, Fail("invalid_input", "dws_personal must not carry application robot identity")
		}
		if in.Identity.DeliveryRobotCode != "" && !identifier.MatchString(in.Identity.DeliveryRobotCode) {
			return c, Fail("invalid_input", "identity.delivery_robot_code is invalid")
		}
		if len(strings.TrimSpace(in.Identity.DeliveryRobotName)) > 200 {
			return c, Fail("invalid_input", "identity.delivery_robot_name is too long")
		}
		// A personal channel is bound to one logged-in account, so the auth
		// namespace includes the user; app channels are bound to the app.
		c.AuthNamespace = "dws:" + in.Identity.Profile
		c.IDNamespace = "dws:" + in.Identity.ExpectedCorpID
		if in.Transport != "" && in.Transport != "dws" {
			return c, Fail("invalid_input", "dws_personal transport must be dws")
		}
	case ChannelDingTalkApp:
		if !identifier.MatchString(in.Identity.ClientID) {
			return c, Fail("invalid_input", "dingtalk_app requires identity.client_id")
		}
		if in.Identity.Profile != "" || in.Identity.ExpectedUserID != "" || in.Identity.DeliveryRobotCode != "" {
			return c, Fail("invalid_input", "dingtalk_app must not carry a personal dws profile or user")
		}
		if in.Identity.HistoryChannel != "" && !identifier.MatchString(in.Identity.HistoryChannel) {
			return c, Fail("invalid_input", "identity.history_channel is invalid")
		}
		if in.Identity.ConfirmationCardTemplate != "" && (!identifier.MatchString(in.Identity.ConfirmationCardTemplate) || !strings.HasSuffix(in.Identity.ConfirmationCardTemplate, ".schema") || len(in.Identity.ConfirmationCardTemplate) > 200) {
			return c, Fail("invalid_input", "identity.confirmation_card_template must be an application-associated .schema template ID")
		}
		if in.Transport != "" && in.Transport != "stream" {
			return c, Fail("invalid_input", "dingtalk_app transport must be stream")
		}
		c.AuthNamespace = "app:" + in.Identity.ClientID
		c.IDNamespace = "app:" + in.Identity.ExpectedCorpID + ":" + in.Identity.ClientID
	default:
		if in.Transport != "" && !identifier.MatchString(in.Transport) {
			return c, Fail("invalid_input", "generic channel transport is invalid")
		}
		c.AuthNamespace = provider + ":" + tenant
		c.IDNamespace = provider + ":" + tenant
	}
	if in.Subscription != nil {
		if in.Kind != ChannelDwsPersonal {
			return c, Fail("invalid_input", "subscription applies to dws_personal only")
		}
		if err := validateSubscription(*in.Subscription); err != nil {
			return c, err
		}
	}
	return c, nil
}

// ValidateChannelInput checks channel identity and credential references offline.
func ValidateChannelInput(in ChannelInput) error {
	_, err := normalizeChannel(in)
	return err
}

func (tx *Tx) AddChannel(ctx context.Context, in ChannelInput) (Channel, error) {
	c, err := normalizeChannel(in)
	if err != nil {
		return c, err
	}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO channels(id,name,kind,provider,tenant,auth_namespace,id_namespace,identity,credential_ref,capabilities,tool_version,config_version,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		c.ID, c.Name, c.Kind, c.Provider, c.Tenant, c.AuthNamespace, c.IDNamespace, JSON(c.Identity), c.CredentialRef, JSON(c.Capabilities), c.ToolVersion, c.ConfigVersion, c.Status, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return c, Fail("conflict", "channel name already registered: %v", err)
	}
	if _, err = tx.Audit(ctx, "channel.add", in.Name, objectChange("channel", c.ID)); err != nil {
		return c, err
	}
	if in.Subscription != nil {
		if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO conversations(id,channel_id,provider_conversation_id,kind,created_at) VALUES(?,?,?,?,?)",
			NewID(), c.ID, in.Subscription.ConversationID, subscriptionConversationKind(*in.Subscription), Now()); err != nil {
			return c, err
		}
	}
	if in.Route != nil {
		if _, err = tx.AddRoute(ctx, c.ID, *in.Route); err != nil {
			return c, err
		}
	}
	return ReadChannel(ctx, tx.Conn, c.ID)
}

func subscriptionConversationKind(s Subscription) string {
	if s.Kind == "group" {
		return "group"
	}
	return "direct"
}

func validateSubscription(s Subscription) error {
	if !contains([]string{"at-me", "sender", "group", "all-direct", "all-group"}, s.Kind) {
		return Fail("invalid_input", "subscription kind must be at-me, sender, group, all-direct or all-group")
	}
	if s.Kind == "group" && !identifier.MatchString(s.ConversationID) {
		return Fail("invalid_input", "group subscription requires conversation_id")
	}
	if s.Kind == "sender" && !identifier.MatchString(s.UserID) {
		return Fail("invalid_input", "sender subscription requires user_id")
	}
	for _, e := range s.Events {
		if !contains([]string{"message", "reaction", "read", "recall"}, e) {
			return Fail("invalid_input", "unsupported subscription event %q", e)
		}
	}
	return nil
}

func (tx *Tx) AddRoute(ctx context.Context, channelID string, in RouteInput) (Route, error) {
	c, err := ReadChannel(ctx, tx.Conn, channelID)
	if err != nil {
		return Route{}, err
	}
	if !identifier.MatchString(in.ConversationID) {
		return Route{}, Fail("invalid_input", "route conversation_id is required and must be a stable platform ID")
	}
	r := Route{ID: NewID(), ChannelID: c.ID, ConversationID: in.ConversationID, ConversationType: in.ConversationType,
		Mode: in.Mode, Triggers: in.Triggers, AudiencePolicy: in.AudiencePolicy,
		SendPolicy: in.SendPolicy, ApprovalDisplay: "display_only", Retention: in.Retention, Version: 1,
		Status: "active", CreatedAt: Now(), UpdatedAt: Now()}
	if r.ConversationType == "" {
		r.ConversationType = "group"
	}
	if !contains([]string{"group", "direct"}, r.ConversationType) {
		return r, Fail("invalid_input", "conversation_type must be group or direct")
	}
	if r.Mode == "" {
		if c.Kind == ChannelDwsPersonal {
			r.Mode = "collect"
		} else {
			r.Mode = "assistant"
		}
	}
	if !contains([]string{"collect", "assistant", "notify", "ignore"}, r.Mode) {
		return r, Fail("invalid_input", "mode must be collect, assistant, notify or ignore")
	}
	if r.Triggers == nil {
		r.Triggers = []string{}
	}
	for _, t := range r.Triggers {
		if !contains([]string{"mention", "direct"}, t) {
			return r, Fail("invalid_input", "unsupported trigger %q", t)
		}
	}
	if r.AudiencePolicy == "" {
		r.AudiencePolicy = "local_private"
	}
	if !contains([]string{"local_private", "conversation"}, r.AudiencePolicy) {
		return r, Fail("invalid_input", "audience_policy must be local_private or conversation")
	}
	r.AudienceKey = audienceKey(r.AudiencePolicy, c.ID, r.ConversationID)
	// A new route never sends. Enabling delivery is a separate explicit change.
	if r.SendPolicy == "" {
		r.SendPolicy = "draft_only"
	}
	if !contains([]string{"draft_only", "dispatch_only", "reply_to_trigger"}, r.SendPolicy) {
		return r, Fail("invalid_input", "send_policy must be draft_only, dispatch_only or reply_to_trigger")
	}
	if r.Retention == "" {
		r.Retention = "{}"
	}
	if !json.Valid([]byte(r.Retention)) {
		return r, Fail("invalid_input", "retention must be a JSON object")
	}
	scope := in.Workspace
	if scope == "" {
		scope = scopeID(tx.Request.Scope)
	}
	var w Workspace
	err = tx.Conn.QueryRowContext(ctx, "SELECT id,name,coalesce(path,'') FROM workspaces WHERE id=? OR name=?", scope, scope).Scan(&w.ID, &w.Name, &w.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return r, Fail("not_found", "workspace %q not found", scope)
	}
	if err != nil {
		return r, err
	}
	r.WorkspaceID = w.ID
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO channel_routes(id,channel_id,conversation_id,conversation_type,workspace_id,mode,triggers,audience_policy,audience_key,memory_policy,send_policy,approval_display,retention,version,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		r.ID, r.ChannelID, r.ConversationID, r.ConversationType, r.WorkspaceID, r.Mode, JSON(r.Triggers), r.AudiencePolicy, r.AudienceKey, r.MemoryPolicy, r.SendPolicy, r.ApprovalDisplay, r.Retention, r.Version, r.Status, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return r, Fail("conflict", "route for this conversation already exists: %v", err)
	}
	if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO conversations(id,channel_id,provider_conversation_id,kind,created_at) VALUES(?,?,?,?,?)",
		NewID(), c.ID, r.ConversationID, r.ConversationType, Now()); err != nil {
		return r, err
	}
	_, err = tx.Audit(ctx, "channel.route.add", r.ConversationID, objectChange("route", r.ID))
	return r, err
}

// UpdateRoute requires the caller's expected version. Widening the audience or
// enabling delivery invalidates drafts produced under the previous policy.
func (tx *Tx) UpdateRoute(ctx context.Context, id string, expected int, in RouteInput, reason string) (any, error) {
	if reason == "" {
		return nil, Fail("invalid_input", "route change reason is required")
	}
	r, err := ReadRoute(ctx, tx.Conn, id)
	if err != nil {
		return nil, err
	}
	if expected < 1 || r.Version != expected {
		return nil, Fail("conflict", "expected route version %d, current %d", expected, r.Version)
	}
	next := r
	if in.Mode != "" {
		next.Mode = in.Mode
	}
	if in.AudiencePolicy != "" {
		next.AudiencePolicy = in.AudiencePolicy
	}
	if in.SendPolicy != "" {
		next.SendPolicy = in.SendPolicy
	}
	if in.Triggers != nil {
		next.Triggers = in.Triggers
	}
	if in.Retention != "" {
		next.Retention = in.Retention
	}
	if in.ConversationID != "" && in.ConversationID != r.ConversationID {
		return nil, Fail("invalid_input", "a route cannot be repointed to another conversation; add a new route")
	}
	for _, check := range [][2]string{{next.Mode, "collect assistant notify ignore"}, {next.AudiencePolicy, "local_private conversation"},
		{next.SendPolicy, "draft_only dispatch_only reply_to_trigger"}} {
		if !contains(strings.Fields(check[1]), check[0]) {
			return nil, Fail("invalid_input", "invalid route value %q", check[0])
		}
	}
	for _, t := range next.Triggers {
		if !contains([]string{"mention", "direct"}, t) {
			return nil, Fail("invalid_input", "unsupported trigger %q", t)
		}
	}
	if !json.Valid([]byte(next.Retention)) {
		return nil, Fail("invalid_input", "retention must be a JSON object")
	}
	next.AudienceKey = audienceKey(next.AudiencePolicy, next.ChannelID, next.ConversationID)
	next.Version = r.Version + 1
	next.UpdatedAt = Now()
	res, err := tx.Conn.ExecContext(ctx, "UPDATE channel_routes SET mode=?,triggers=?,audience_policy=?,audience_key=?,memory_policy=?,send_policy=?,retention=?,version=?,updated_at=? WHERE id=? AND version=?",
		next.Mode, JSON(next.Triggers), next.AudiencePolicy, next.AudienceKey, next.MemoryPolicy, next.SendPolicy, next.Retention, next.Version, next.UpdatedAt, id, expected)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, Fail("conflict", "route changed since version %d", expected)
	}
	// Drafts were produced against the old policy version; they must be
	// re-checked before any delivery rather than inheriting the new permission.
	stale, err := tx.Conn.ExecContext(ctx, "UPDATE outbox SET state='stale',reason=?,updated_at=? WHERE route_id=? AND state IN ('draft','ready')",
		"route policy changed to version "+strconv.Itoa(next.Version), Now(), id)
	if err != nil {
		return nil, err
	}
	affected, _ := stale.RowsAffected()
	if _, err = tx.Audit(ctx, "channel.route.update", reason, []Change{{ObjectType: "route", ObjectID: id, Before: expected, After: next.Version}}); err != nil {
		return nil, err
	}
	return map[string]any{"route": next, "stale_drafts": affected}, nil
}

func (tx *Tx) SetChannelCapabilities(ctx context.Context, id string, caps Capabilities, tool string) (Channel, error) {
	c, err := ReadChannel(ctx, tx.Conn, id)
	if err != nil {
		return c, err
	}
	if caps.Verified == nil {
		caps.Verified = map[string]bool{}
	}
	if caps.Unverified == nil {
		caps.Unverified = []string{}
	}
	caps.CheckedAt = Now()
	caps.Tool = tool
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE channels SET capabilities=?,tool_version=?,config_version=config_version+1,updated_at=? WHERE id=?",
		JSON(caps), tool, Now(), id); err != nil {
		return c, err
	}
	if _, err = tx.Audit(ctx, "channel.capabilities", tool, objectChange("channel", id)); err != nil {
		return c, err
	}
	return ReadChannel(ctx, tx.Conn, id)
}

const channelColumns = "id,name,kind,provider,tenant,auth_namespace,id_namespace,identity,credential_ref,capabilities,tool_version,config_version,status,created_at,updated_at"

func scanChannel(row scanner) (Channel, error) {
	var c Channel
	var identity, caps string
	err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.Provider, &c.Tenant, &c.AuthNamespace, &c.IDNamespace, &identity, &c.CredentialRef, &caps, &c.ToolVersion, &c.ConfigVersion, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal([]byte(identity), &c.Identity); err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(caps), &c.Capabilities)
}

func ReadChannel(ctx context.Context, q Queryer, value string) (Channel, error) {
	c, err := scanChannel(q.QueryRowContext(ctx, "SELECT "+channelColumns+" FROM channels WHERE id=? OR name=?", value, value))
	if errors.Is(err, sql.ErrNoRows) {
		return c, Fail("not_found", "channel %q not found", value)
	}
	if err != nil {
		return c, err
	}
	c.Routes, err = RouteList(ctx, q, c.ID)
	return c, err
}
func ChannelList(ctx context.Context, q Queryer) ([]Channel, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+channelColumns+" FROM channels ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Routes, err = RouteList(ctx, q, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

const routeColumns = "id,channel_id,conversation_id,conversation_type,workspace_id,mode,triggers,audience_policy,audience_key,memory_policy,send_policy,approval_display,retention,version,status,created_at,updated_at"

func scanRoute(row scanner) (Route, error) {
	var r Route
	var triggers string
	err := row.Scan(&r.ID, &r.ChannelID, &r.ConversationID, &r.ConversationType, &r.WorkspaceID, &r.Mode, &triggers, &r.AudiencePolicy, &r.AudienceKey, &r.MemoryPolicy, &r.SendPolicy, &r.ApprovalDisplay, &r.Retention, &r.Version, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal([]byte(triggers), &r.Triggers)
}
func ReadRoute(ctx context.Context, q Queryer, id string) (Route, error) {
	r, err := scanRoute(q.QueryRowContext(ctx, "SELECT "+routeColumns+" FROM channel_routes WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, Fail("not_found", "route %q not found", id)
	}
	return r, err
}

// RouteFor resolves the exact conversation binding. An unbound conversation is
// never collected or answered, so a missing route is a definite refusal.
func RouteFor(ctx context.Context, q Queryer, channelID, conversationID string) (Route, error) {
	r, err := scanRoute(q.QueryRowContext(ctx, "SELECT "+routeColumns+" FROM channel_routes WHERE channel_id=? AND conversation_id=? AND status='active'", channelID, conversationID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, Fail("denied", "conversation is not bound to a route on this channel")
	}
	return r, err
}
func RouteList(ctx context.Context, q Queryer, channelID string) ([]Route, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+routeColumns+" FROM channel_routes WHERE channel_id=? ORDER BY created_at,id", channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Route{}
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ChannelPlan states what a receiver would subscribe to, which identity it would
// use and which policies apply. It performs no platform call and creates nothing.
func ChannelPlan(ctx context.Context, q Queryer, value string) (any, error) {
	c, err := ReadChannel(ctx, q, value)
	if err != nil {
		return nil, err
	}
	plan := map[string]any{"channel": c.Name, "kind": c.Kind, "tenant": c.Tenant,
		"auth_namespace": c.AuthNamespace, "id_namespace": c.IDNamespace,
		"credential_ref": c.CredentialRef, "would_subscribe": []any{}, "creates_subscription": false,
		"sends_message": false, "capabilities": c.Capabilities}
	items := []any{}
	for _, r := range c.Routes {
		items = append(items, map[string]any{"conversation_id": r.ConversationID, "conversation_type": r.ConversationType,
			"workspace_id": r.WorkspaceID, "mode": r.Mode, "triggers": r.Triggers, "audience_policy": r.AudiencePolicy,
			"audience_key": r.AudienceKey, "send_policy": r.SendPolicy,
			"approval_display": r.ApprovalDisplay, "route_version": r.Version})
	}
	plan["would_subscribe"] = items
	plan["note"] = "plan is offline: no subscription, no platform call and no message is produced"
	return plan, nil
}

// ChannelDoctor is offline by default: it checks configuration, capability
// records and credential references without contacting any platform.
func ChannelDoctor(ctx context.Context, q Queryer, value string) (any, error) {
	c, err := ReadChannel(ctx, q, value)
	if err != nil {
		return nil, err
	}
	issues := []string{}
	if len(c.Routes) == 0 {
		issues = append(issues, "channel has no route; collection and replies are refused until one is bound")
	}
	if c.Kind == ChannelDingTalkApp && c.CredentialRef == "" {
		issues = append(issues, "dingtalk_app has no credential_ref; stream connect will be refused")
	}
	if c.Kind == ChannelDingTalkApp && c.Identity.RobotCode == "" {
		issues = append(issues, "robot_code is unverified; bot transport stays unavailable")
	}
	if len(c.Capabilities.Verified) == 0 {
		issues = append(issues, "no capability has been verified for this channel")
	}
	for _, r := range c.Routes {
		if r.SendPolicy != "draft_only" && c.Capabilities.Verified["send"] != true {
			issues = append(issues, "route "+r.ID+" enables delivery while send capability is unverified")
		}
	}
	lease, err := ReadLease(ctx, q, c.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"channel": c, "online_checked": false, "issues": issues, "healthy": len(issues) == 0,
		"lease": lease, "next_action": "capability and connectivity checks that contact the platform are a separate explicit step"}, nil
}

// Lease keeps a single live receiver per channel. The fence advances on every
// acquisition so a stale holder that wakes up cannot write.
type Lease struct {
	ChannelID string `json:"channel_id"`
	Holder    string `json:"holder"`
	Token     string `json:"token"`
	Fence     int64  `json:"fence"`
	Until     string `json:"until"`
	UpdatedAt string `json:"updated_at"`
	Held      bool   `json:"held"`
}

func ReadLease(ctx context.Context, q Queryer, channelID string) (Lease, error) {
	var l Lease
	err := q.QueryRowContext(ctx, "SELECT channel_id,holder,token,fence,until,updated_at FROM channel_leases WHERE channel_id=?", channelID).
		Scan(&l.ChannelID, &l.Holder, &l.Token, &l.Fence, &l.Until, &l.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{ChannelID: channelID}, nil
	}
	if err != nil {
		return l, err
	}
	l.Held = l.Until > Now()
	return l, nil
}

// AcquireLease takes over only when the current lease has expired. The returned
// fence must accompany every later write from that receiver.
func (tx *Tx) AcquireLease(ctx context.Context, channelID, holder string, ttl time.Duration) (Lease, error) {
	if _, err := ReadChannel(ctx, tx.Conn, channelID); err != nil {
		return Lease{}, err
	}
	if holder == "" {
		return Lease{}, Fail("invalid_input", "lease holder is required")
	}
	if ttl < time.Second || ttl > 24*time.Hour {
		return Lease{}, Fail("invalid_input", "lease ttl must be 1s..24h")
	}
	current, err := ReadLease(ctx, tx.Conn, channelID)
	if err != nil {
		return current, err
	}
	if current.Held && current.Holder != holder {
		return current, Fail("conflict", "channel %s already has a live receiver until %s", channelID, current.Until)
	}
	next := Lease{ChannelID: channelID, Holder: holder, Token: NewID(), Fence: current.Fence + 1,
		Until: time.Now().UTC().Add(ttl).Format(time.RFC3339Nano), UpdatedAt: Now(), Held: true}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO channel_leases(channel_id,holder,token,fence,until,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(channel_id) DO UPDATE SET holder=excluded.holder,token=excluded.token,fence=excluded.fence,until=excluded.until,updated_at=excluded.updated_at",
		next.ChannelID, next.Holder, next.Token, next.Fence, next.Until, next.UpdatedAt)
	return next, err
}

// CheckLease rejects a writer whose fence is behind the current lease, which is
// how a receiver that lost its lease stops being able to record events.
func CheckLease(ctx context.Context, q Queryer, channelID, token string, fence int64) error {
	l, err := ReadLease(ctx, q, channelID)
	if err != nil {
		return err
	}
	if l.Token == "" {
		return Fail("conflict", "channel %s has no lease; acquire one before writing", channelID)
	}
	if l.Token != token || l.Fence != fence {
		return Fail("conflict", "lease fence %d is stale; current fence is %d", fence, l.Fence)
	}
	return nil
}

func (tx *Tx) RenewLease(ctx context.Context, channelID, token string, fence int64, ttl time.Duration) (Lease, error) {
	if ttl < time.Second || ttl > 24*time.Hour {
		return Lease{}, Fail("invalid_input", "lease ttl must be 1s..24h")
	}
	now := Now()
	until := time.Now().UTC().Add(ttl).Format(time.RFC3339Nano)
	res, err := tx.Conn.ExecContext(ctx, "UPDATE channel_leases SET until=?,updated_at=? WHERE channel_id=? AND token=? AND fence=?", until, now, channelID, token, fence)
	if err != nil {
		return Lease{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Lease{}, Fail("conflict", "channel lease was replaced before renewal")
	}
	return Lease{ChannelID: channelID, Token: token, Fence: fence, Until: until, UpdatedAt: now, Held: true}, nil
}

// ReleaseLease lets a clean shutdown hand the channel over without waiting for
// the TTL. The fence is kept so a stale holder still cannot write.
func (tx *Tx) ReleaseLease(ctx context.Context, channelID, token string) error {
	res, err := tx.Conn.ExecContext(ctx, "UPDATE channel_leases SET until=?,updated_at=? WHERE channel_id=? AND token=?",
		Now(), Now(), channelID, token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Fail("conflict", "lease token does not hold channel %s", channelID)
	}
	return nil
}
