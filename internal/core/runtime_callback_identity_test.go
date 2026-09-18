package core

import (
	"context"
	"testing"
	"time"
)

// Parser v2 corrects the callback's senderStaffId to user_id. Neither redelivery
// nor a matching value may silently change the identity of an existing message.
func TestAppParserUpgradePreservesLegacyIdentityAndConfirmationAudit(t *testing.T) {
	for _, eventID := range []string{"", "stable-event-id"} {
		t.Run("provider-event-"+eventID, func(t *testing.T) {
			f, app, route, action := crossConfirmationFixture(t)
			ctx := context.Background()
			sent := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
			legacy := NormalizedEvent{Kind: EventMessage, Adapter: "dingtalk_app", ParseVersion: "dingtalk_app/1", Origin: "stream",
				ProviderEventID: eventID, ProviderMessageID: "legacy-approval", ConversationID: route.ConversationID,
				ConversationType: "direct", Tenant: app.Tenant, Sender: Sender{IDType: "staff_id", IDValue: f.owner.IDValue},
				Body: ConfirmationToken(action), SentAt: sent, EventAt: sent}
			old := intake(t, f.s, app.ID, legacy, "legacy")
			before, err := ReadMessage(ctx, f.s.DB, old.MessageID)
			if err != nil {
				t.Fatal(err)
			}
			upgraded := legacy
			upgraded.ParseVersion, upgraded.Sender.IDType = "dingtalk_app/2", "user_id"
			replayed := intake(t, f.s, app.ID, upgraded, "replayed")
			after, err := ReadMessage(ctx, f.s.DB, old.MessageID)
			if err != nil {
				t.Fatal(err)
			}
			if replayed.MessageID != old.MessageID || after.SenderIDType != "staff_id" || after.Sender != before.Sender || after.Sender == f.config.OwnerPrincipalID || after.Revision != before.Revision {
				t.Fatalf("parser upgrade relabeled history: before=%+v after=%+v replay=%+v", before, after, replayed)
			}
			if len(after.SourceIDs) != len(before.SourceIDs) || len(after.SourceIDs) != 1 || after.SourceIDs[0] != before.SourceIDs[0] {
				t.Fatalf("historical evidence changed: before=%v after=%v", before.SourceIDs, after.SourceIDs)
			}
			var parseVersion string
			if err = f.s.DB.QueryRowContext(ctx, "SELECT parse_version FROM inbox_events WHERE id=?", old.InboxEventID).Scan(&parseVersion); err != nil || parseVersion != "dingtalk_app/1" {
				t.Fatalf("old parser audit lost: %s %v", parseVersion, err)
			}
			_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "test.legacy.confirm"}, func(tx *Tx) (any, error) {
				return tx.ConfirmRuntimeActionFromMessage(ctx, action.ID, replayed.MessageID)
			})
			if ErrorCode(err) != "denied" {
				t.Fatalf("legacy same-value staff_id authorized: %v", err)
			}
			// A genuinely new v2 callback resolves to the already attested exact
			// user_id without creating or trusting a cross-type identity link.
			upgraded.ProviderEventID, upgraded.ProviderMessageID = "new-event", "new-approval"
			fresh := intake(t, f.s, app.ID, upgraded, "fresh")
			current, err := ReadMessage(ctx, f.s.DB, fresh.MessageID)
			if err != nil || current.SenderIDType != "user_id" || current.Sender != f.config.OwnerPrincipalID {
				t.Fatalf("new callback did not resolve attested owner: %+v %v", current, err)
			}
			var basis, staffPrincipal string
			if err = f.s.DB.QueryRowContext(ctx, "SELECT basis FROM identity_aliases WHERE tenant=? AND id_type='user_id' AND id_value=?", app.Tenant, f.owner.IDValue).Scan(&basis); err != nil || basis != "authenticated_dws_profile" {
				t.Fatalf("DWS attestation overwritten: %s %v", basis, err)
			}
			if err = f.s.DB.QueryRowContext(ctx, "SELECT principal_id FROM identity_aliases WHERE tenant=? AND id_type='staff_id' AND id_value=?", app.Tenant, f.owner.IDValue).Scan(&staffPrincipal); err != nil || staffPrincipal != before.Sender {
				t.Fatalf("historical alias relinked: %s %v", staffPrincipal, err)
			}
			_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "test.new.callback.confirm"}, func(tx *Tx) (any, error) {
				return tx.ConfirmRuntimeActionFromMessage(ctx, action.ID, fresh.MessageID)
			})
			if ErrorCode(err) != "denied" {
				t.Fatalf("parser change restored background confirmation: %v", err)
			}
		})
	}
}
