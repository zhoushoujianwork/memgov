package knowledge

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Credentials belong to memgov. They are never included in configuration
// plans, command arguments, tool descriptions, or status output.
type Credentials struct {
	Dokki struct {
		APIKey string `json:"api_key"`
	} `json:"dokki"`
	Confluence struct {
		DockToken string `json:"dock_token"`
		PAT       string `json:"pat"`
	} `json:"confluence"`
}

func file(home string) string { return filepath.Join(home, "knowledge-sources.json") }

func validSecret(value string) bool {
	if value == "" || len(value) > 8192 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func Load(home string) (Credentials, error) {
	var value Credentials
	path := file(home)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	if err != nil {
		return value, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 32768 {
		return value, fmt.Errorf("knowledge credential file must be a private regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	if err = json.Unmarshal(raw, &value); err != nil {
		return Credentials{}, fmt.Errorf("knowledge credential file is invalid")
	}
	if value.Dokki.APIKey != "" && !validSecret(value.Dokki.APIKey) ||
		value.Confluence.DockToken != "" && !validSecret(value.Confluence.DockToken) ||
		value.Confluence.PAT != "" && !validSecret(value.Confluence.PAT) {
		return Credentials{}, fmt.Errorf("knowledge credential file has invalid values")
	}
	return value, nil
}

func Status(home string) (map[string]bool, error) {
	c, err := Load(home)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"dokki": c.Dokki.APIKey != "", "confluence": c.Confluence.DockToken != "" && c.Confluence.PAT != ""}, nil
}

// Configure replaces both source credentials from a private JSON input or
// stdin. The caller must keep the input itself private; no secret is emitted.
func Configure(home string, raw []byte) (map[string]bool, error) {
	var empty map[string]bool
	if !filepath.IsAbs(home) || len(raw) > 32768 {
		return empty, fmt.Errorf("invalid knowledge credential input")
	}
	var value Credentials
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil {
		return empty, fmt.Errorf("invalid knowledge credential input")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return empty, fmt.Errorf("invalid knowledge credential input")
	}
	if !validSecret(value.Dokki.APIKey) || !validSecret(value.Confluence.DockToken) || !validSecret(value.Confluence.PAT) {
		return empty, fmt.Errorf("both knowledge sources require valid credentials")
	}
	if err := save(home, value); err != nil {
		return empty, err
	}
	return Status(home)
}

func save(home string, value Credentials) error {
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	dest := file(home)
	if info, err := os.Lstat(dest); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("knowledge credential destination is not private")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(home, ".knowledge-sources-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp.Name(), dest)
}

// ImportRelayer copies configured credentials once from a legacy read-only
// SQLite file into memgov's private store. It never deletes or updates source.
func ImportRelayer(home, source string) (map[string]bool, error) {
	var empty map[string]bool
	if !filepath.IsAbs(home) || !filepath.IsAbs(source) {
		return empty, fmt.Errorf("absolute home and source paths are required")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return empty, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return empty, fmt.Errorf("legacy credential database must be a private regular file")
	}
	u := url.URL{Scheme: "file", Path: source, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return empty, err
	}
	defer db.Close()
	var value Credentials
	for _, provider := range []string{"dokki", "confluence"} {
		var body string
		if err = db.QueryRow("SELECT body FROM connections WHERE provider=?", provider).Scan(&body); err != nil {
			return empty, fmt.Errorf("legacy %s connection unavailable: %w", provider, err)
		}
		var record struct {
			Credentials struct {
				APIKey    string `json:"apiKey"`
				DockToken string `json:"dockToken"`
				PAT       string `json:"pat"`
			} `json:"credentials"`
		}
		if err = json.Unmarshal([]byte(body), &record); err != nil {
			return empty, fmt.Errorf("legacy %s connection is invalid", provider)
		}
		if provider == "dokki" {
			value.Dokki.APIKey = record.Credentials.APIKey
		} else {
			value.Confluence.DockToken = record.Credentials.DockToken
			value.Confluence.PAT = record.Credentials.PAT
		}
	}
	if !validSecret(value.Dokki.APIKey) || !validSecret(value.Confluence.DockToken) || !validSecret(value.Confluence.PAT) {
		return empty, fmt.Errorf("both legacy sources must have valid credentials")
	}
	if err = os.MkdirAll(home, 0700); err != nil {
		return empty, err
	}
	dest := file(home)
	if _, statErr := os.Lstat(dest); statErr == nil {
		current, loadErr := Load(home)
		if loadErr != nil {
			return empty, loadErr
		}
		if current != value {
			return empty, fmt.Errorf("memgov credentials already exist and differ; import did not overwrite them")
		}
		return Status(home)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return empty, statErr
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return empty, err
	}
	tmp, err := os.CreateTemp(home, ".knowledge-sources-*")
	if err != nil {
		return empty, err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return empty, err
	}
	if closeErr != nil {
		return empty, closeErr
	}
	// Hard-link creation is exclusive: a concurrent writer cannot be replaced.
	if err = os.Link(tmp.Name(), dest); err != nil {
		return empty, err
	}
	return Status(home)
}
