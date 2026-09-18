package dws

import (
	"context"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestAttestOwnerRequiresProfileAndCurrentUserAgreement(t *testing.T) {
	c := core.Channel{Kind: core.ChannelDwsPersonal, Tenant: "corp", Identity: core.ChannelIdentity{Profile: "corp:owner", ExpectedUserID: "owner"}}
	for _, tc := range []struct {
		name, profileUser, currentUser string
		want                           string
	}{
		{"agrees", "owner", "owner", ""}, {"profile mismatch", "other", "owner", "denied"},
		{"old profile omits user", "", "owner", ""},
		{"current mismatch", "owner", "other", "denied"}, {"missing current", "owner", "", "denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{Run: func(_ context.Context, args ...string) ([]byte, error) {
				if strings.Join(args, " ") == "profile list --format json" {
					return []byte(`{"success":true,"result":{"profiles":[{"profile":"corp:owner","corpId":"corp","userId":"` + tc.profileUser + `"}]}}`), nil
				}
				if strings.Contains(strings.Join(args, " "), "contact +me") {
					return []byte(`{"success":true,"result":{"data":{"userId":"` + tc.currentUser + `"}}}`), nil
				}
				return nil, core.Fail("invalid_input", "unexpected fake call")
			}}
			err := a.AttestOwner(context.Background(), c)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && core.ErrorCode(err) != tc.want {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}
