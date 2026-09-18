package dingtalkapp

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// resolveSecret turns a credential reference into the application secret. The
// reference is all the database holds; the value is read at the moment it is
// needed and never stored, logged or returned in an envelope.
//
// Only the three reference forms the channel schema already accepts are
// resolved. An unreadable reference is a clear failure rather than a fallback to
// another credential, because a wrong secret would connect as another identity.
func resolveSecret(ctx context.Context, ref string) (string, error) {
	switch {
	case ref == "":
		return "", core.Fail("invalid_input", "the channel has no credential_ref; a Stream connection needs the application secret")
	case strings.HasPrefix(ref, "env://"):
		name := strings.TrimPrefix(ref, "env://")
		value := os.Getenv(name)
		if value == "" {
			return "", core.Fail("denied", "environment variable %s is empty; the application secret is unavailable", name)
		}
		return value, nil
	case strings.HasPrefix(ref, "file://"):
		raw, err := os.ReadFile(strings.TrimPrefix(ref, "file://"))
		if err != nil {
			return "", core.Fail("denied", "credential file could not be read: %v", err)
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			return "", core.Fail("denied", "credential file is empty")
		}
		return value, nil
	case strings.HasPrefix(ref, "keychain://"):
		// keychain://service/account. The value is fetched through the OS keychain
		// tool with an argv array, so the reference never reaches a shell.
		service, account, ok := strings.Cut(strings.TrimPrefix(ref, "keychain://"), "/")
		if !ok || service == "" || account == "" {
			return "", core.Fail("invalid_input", "keychain reference must be keychain://service/account")
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
		if err != nil {
			return "", core.Fail("denied", "the keychain did not return %s: %v", ref, err)
		}
		value := strings.TrimSpace(string(out))
		if value == "" {
			return "", core.Fail("denied", "the keychain entry %s is empty", ref)
		}
		return value, nil
	default:
		return "", core.Fail("invalid_input", "unsupported credential_ref scheme in %q", ref)
	}
}
