package scenario

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const identityCacheTTL = 15 * time.Minute

// identityCache persists only an identity and its dws profile fingerprint. It
// deliberately contains no token, profile secret, contact or message data.
type identityCache struct {
	dir string
	now func() time.Time
}

type profileContext struct {
	selector  string
	corpID    string
	clientID  string
	authEpoch string
}

func (p profileContext) key() string {
	return strings.Join([]string{"dws-identity-v1", p.selector, p.corpID, p.clientID, p.authEpoch}, "\n")
}

func hashString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

type cachedIdentity struct {
	Context    string `json:"context"`
	UserID     string `json:"user_id"`
	CapturedAt string `json:"captured_at"`
}

func defaultIdentityCache() identityCache {
	dir, err := os.UserCacheDir()
	if err != nil {
		return identityCache{}
	}
	return identityCache{dir: filepath.Join(dir, "memgov", "dws-identity-v1"), now: time.Now}
}

func (c identityCache) cachePath(p profileContext) string {
	return filepath.Join(c.dir, "identity-"+hashString(p.key())+".json")
}

func (c identityCache) load(p profileContext) (Person, bool) {
	if c.dir == "" {
		return Person{}, false
	}
	b, err := os.ReadFile(c.cachePath(p))
	if err != nil {
		return Person{}, false
	}
	var entry cachedIdentity
	if json.Unmarshal(b, &entry) != nil || entry.Context != p.key() || entry.UserID == "" {
		return Person{}, false
	}
	captured, err := time.Parse(time.RFC3339Nano, entry.CapturedAt)
	if err != nil {
		return Person{}, false
	}
	now := c.nowUTC()
	if captured.After(now.Add(time.Minute)) || now.Sub(captured) > identityCacheTTL {
		return Person{}, false
	}
	return Person{ID: entry.UserID}, true
}

func (c identityCache) store(p profileContext, person Person) {
	if c.dir == "" || person.ID == "" {
		return
	}
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return
	}
	_ = os.Chmod(c.dir, 0700)
	entry := cachedIdentity{Context: p.key(), UserID: person.ID, CapturedAt: c.nowUTC().Format(time.RFC3339Nano)}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(c.dir, ".identity-*.tmp")
	if err != nil {
		return
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		_ = os.Rename(name, c.cachePath(p))
		_ = os.Chmod(c.cachePath(p), 0600)
	}
}

func (c identityCache) nowUTC() time.Time {
	if c.now == nil {
		return time.Now().UTC()
	}
	return c.now().UTC()
}

func currentIdentity(ctx context.Context, run Runner, requested string, cache identityCache) (Person, string, string) {
	profile, ok := resolveProfile(ctx, run, requested)
	if !ok {
		return Person{}, "", "identity_context_unavailable"
	}
	if person, ok := cache.load(profile); ok {
		return person, profile.selector, ""
	}
	raw, status := scopedCall(ctx, run, profile.selector, "contact", "user", "get-self")
	if status != "" {
		return Person{}, "", status
	}
	person, ok := parseSelf(raw)
	if !ok {
		return Person{}, "", "invalid_response"
	}
	cache.store(profile, person)
	return person, profile.selector, ""
}

func parseSelf(raw json.RawMessage) (Person, bool) {
	var self Person
	if len(raw) > 0 && raw[0] == '[' {
		var entries []struct {
			Employee Person `json:"orgEmployeeModel"`
		}
		if json.Unmarshal(raw, &entries) != nil || len(entries) != 1 {
			return Person{}, false
		}
		self = entries[0].Employee
	} else if json.Unmarshal(raw, &self) != nil {
		return Person{}, false
	}
	return self, self.ID != ""
}

func resolveProfile(ctx context.Context, run Runner, requested string) (profileContext, bool) {
	b, err := run(ctx, "profile", "list", "--format", "json")
	if err != nil {
		return profileContext{}, false
	}
	var listing struct {
		Success        bool   `json:"success"`
		CurrentProfile string `json:"currentProfile"`
		Profiles       []struct {
			Profile     string `json:"profile"`
			CorpID      string `json:"corpId"`
			ClientID    string `json:"clientId"`
			LastLoginAt string `json:"lastLoginAt"`
			ExpiresAt   string `json:"expiresAt"`
			IsCurrent   bool   `json:"isCurrent"`
		} `json:"profiles"`
	}
	if json.Unmarshal(b, &listing) != nil || !listing.Success {
		return profileContext{}, false
	}
	var match *struct {
		Profile     string `json:"profile"`
		CorpID      string `json:"corpId"`
		ClientID    string `json:"clientId"`
		LastLoginAt string `json:"lastLoginAt"`
		ExpiresAt   string `json:"expiresAt"`
		IsCurrent   bool   `json:"isCurrent"`
	}
	for i := range listing.Profiles {
		candidate := &listing.Profiles[i]
		if requested != "" {
			if candidate.Profile != requested {
				continue
			}
		} else if candidate.Profile != listing.CurrentProfile && !candidate.IsCurrent {
			continue
		}
		if match != nil {
			return profileContext{}, false
		}
		match = candidate
	}
	if match == nil || match.Profile == "" || match.CorpID == "" || match.ClientID == "" || (match.LastLoginAt == "" && match.ExpiresAt == "") {
		return profileContext{}, false
	}
	selector := match.Profile
	return profileContext{selector: selector, corpID: match.CorpID, clientID: match.ClientID, authEpoch: match.LastLoginAt + "\n" + match.ExpiresAt}, true
}
