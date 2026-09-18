package core

import (
	"context"
	"database/sql"
	"errors"
)

// AttestDWSOwner records an exact user_id proven by authenticated DWS reads.
// It does not equate user_id with staff_id, open_id or any other namespace.
func (tx *Tx) AttestDWSOwner(ctx context.Context, channelID string, expectedVersion int) (any, error) {
	c, err := ReadChannel(ctx, tx.Conn, channelID)
	if err != nil {
		return nil, err
	}
	if c.Kind != ChannelDwsPersonal || c.ConfigVersion != expectedVersion || c.Tenant == "" || c.Identity.ExpectedUserID == "" {
		return nil, Fail("conflict", "DWS owner channel changed after identity verification")
	}
	var id string
	err = tx.Conn.QueryRowContext(ctx, "SELECT id FROM principals WHERE tenant=? AND id_type='user_id' AND id_value=?", c.Tenant, c.Identity.ExpectedUserID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = NewID()
		_, err = tx.Conn.ExecContext(ctx, "INSERT INTO principals(id,tenant,id_type,id_value,created_at) VALUES(?,?,'user_id',?,?)", id, c.Tenant, c.Identity.ExpectedUserID, Now())
	}
	if err != nil {
		return nil, err
	}
	var existing string
	err = tx.Conn.QueryRowContext(ctx, "SELECT principal_id FROM identity_aliases WHERE tenant=? AND id_type='user_id' AND id_value=?", c.Tenant, c.Identity.ExpectedUserID).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && existing != id {
		return nil, Fail("conflict", "owner identity is already linked to another principal")
	}
	if _, err = tx.Conn.ExecContext(ctx, `INSERT INTO identity_aliases(tenant,id_type,id_value,principal_id,basis,verified,created_at)
		VALUES(?,'user_id',?,?,'authenticated_dws_profile',1,?)
		ON CONFLICT(tenant,id_type,id_value) DO UPDATE SET basis=excluded.basis,verified=1`, c.Tenant, c.Identity.ExpectedUserID, id, Now()); err != nil {
		return nil, err
	}
	if _, err = tx.Audit(ctx, "identity.attest_dws_owner", "authenticated_dws_profile", objectChange("principal", id)); err != nil {
		return nil, err
	}
	return map[string]any{"principal_id": id, "verified": true, "basis": "authenticated_dws_profile"}, nil
}
