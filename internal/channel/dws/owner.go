package dws

import (
	"context"
	"encoding/json"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// AttestOwner checks two independent authenticated DWS reads. It returns no
// identity from caller input: the profile and current-user result must agree
// with the channel's enterprise and owner before the caller records proof.
func (a *Adapter) AttestOwner(ctx context.Context, c core.Channel) error {
	if c.Kind != core.ChannelDwsPersonal || c.Identity.Profile == "" || c.Tenant == "" || c.Identity.ExpectedUserID == "" {
		return core.Fail("invalid_input", "owner attestation requires a bound DWS channel")
	}
	profileRaw, err := a.run(ctx, "profile", "list")
	if err != nil {
		return err
	}
	var profiles struct {
		Profiles []struct {
			Profile string `json:"profile"`
			CorpID  string `json:"corpId"`
			UserID  string `json:"userId"`
		} `json:"profiles"`
	}
	if json.Unmarshal(profileRaw, &profiles) != nil {
		return core.Fail("unavailable", "DWS profile result is unreadable")
	}
	matched := 0
	for _, p := range profiles.Profiles {
		if p.Profile != c.Identity.Profile {
			continue
		}
		matched++
		// Older DWS profiles omit userId from the list. The explicit +me
		// request below supplies it for the selected authenticated profile.
		if p.CorpID != c.Tenant || (p.UserID != "" && p.UserID != c.Identity.ExpectedUserID) {
			return core.Fail("denied", "DWS profile identity does not match the configured owner")
		}
	}
	if matched != 1 {
		return core.Fail("denied", "DWS owner profile is unavailable or ambiguous")
	}
	meRaw, err := a.run(ctx, withProfile(channelConfig(c.Identity.Profile), []string{"contact", "+me"})...)
	if err != nil {
		return err
	}
	var me struct {
		Data struct {
			UserID string `json:"userId"`
		} `json:"data"`
		UserID string `json:"userId"`
	}
	if json.Unmarshal(meRaw, &me) != nil {
		return core.Fail("unavailable", "DWS current-user result is unreadable")
	}
	userID := me.UserID
	if me.Data.UserID != "" {
		userID = me.Data.UserID
	}
	if userID == "" || userID != c.Identity.ExpectedUserID {
		return core.Fail("denied", "DWS current user does not match the configured owner")
	}
	return nil
}
