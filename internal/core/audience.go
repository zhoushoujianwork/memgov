package core

import "context"

// Audience is the trusted route identity used to bind outbound delivery.
type Audience struct {
	ChannelID      string `json:"channel_id"`
	ChannelName    string `json:"channel_name"`
	Kind           string `json:"kind"`
	Tenant         string `json:"tenant"`
	ConversationID string `json:"conversation_id"`
	AudienceKey    string `json:"audience_key"`
	WorkspaceID    string `json:"workspace_id"`
	RouteID        string `json:"route_id"`
	RouteVersion   int    `json:"route_version"`
	SendPolicy     string `json:"send_policy"`
	Mode           string `json:"mode"`
}

func AudienceFor(ctx context.Context, q Queryer, channelValue, conversationID string) (Audience, error) {
	c, err := ReadChannel(ctx, q, channelValue)
	if err != nil {
		return Audience{}, err
	}
	r, err := RouteFor(ctx, q, c.ID, conversationID)
	if err != nil {
		return Audience{}, err
	}
	return Audience{ChannelID: c.ID, ChannelName: c.Name, Kind: c.Kind, Tenant: c.Tenant,
		ConversationID: r.ConversationID, AudienceKey: r.AudienceKey, WorkspaceID: r.WorkspaceID,
		RouteID: r.ID, RouteVersion: r.Version, SendPolicy: r.SendPolicy, Mode: r.Mode}, nil
}
