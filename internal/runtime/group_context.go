package runtime

// groupRequestContext names the addressed turn, rather than selecting a person
// from recent chat. Labels remain data; the principal is the identity key.
type groupRequestContext struct {
	MessageID         string `json:"message_id"`
	SenderPrincipal   string `json:"sender_principal"`
	SenderDisplayName string `json:"sender_display_name,omitempty"`
}

func currentGroupRequest(in ExecutionInput) *groupRequestContext {
	if in.ApplicationMode != "group_mention" || len(in.Task.Messages) != 1 {
		return nil
	}
	m := in.Task.Messages[0]
	return &groupRequestContext{MessageID: m.ID, SenderPrincipal: m.Sender, SenderDisplayName: m.SenderDisplayName}
}
