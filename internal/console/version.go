package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const tagEndpoint = "repos/zhoushoujianwork/memgov/tags?per_page=100"
const tagAPI = "https://api.github.com/" + tagEndpoint
const tagPage = "https://github.com/zhoushoujianwork/memgov/tree/"

type TagStatus struct {
	State         string    `json:"state"`
	LatestVersion string    `json:"latest_version,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
	TagURL        string    `json:"tag_url,omitempty"`
}

// The check never holds a database connection or the local fingerprint mutex.
func (s *Server) checkTag(ctx context.Context) TagStatus {
	s.tagMu.Lock()
	defer s.tagMu.Unlock()
	if time.Now().Before(s.tagExpires) {
		return s.tagResult
	}
	result := TagStatus{State: "unavailable", CheckedAt: time.Now().UTC()}
	defer func() {
		s.tagResult = result
		ttl := 15 * time.Minute
		if result.State == "unavailable" || result.State == "not_found" {
			ttl = time.Minute
		}
		s.tagExpires = time.Now().Add(ttl)
	}()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	tags, err := s.loadTags(ctx)
	if err != nil {
		return result
	}
	latest := ""
	for _, tag := range tags {
		if len(tag.Name) > 128 {
			continue
		}
		if _, ok := parseVersion(tag.Name); !ok {
			continue
		}
		if latest == "" {
			latest = tag.Name
			continue
		}
		if comparison, _ := compareVersions(tag.Name, latest); comparison > 0 {
			latest = tag.Name
		}
	}
	if latest == "" {
		result.State = "not_found"
		return result
	}
	result.LatestVersion = latest
	result.TagURL = tagPage + url.PathEscape(latest)
	result.State = "unknown"
	if comparison, ok := compareVersions(s.opts.Version, latest); ok {
		switch {
		case comparison < 0:
			result.State = "update"
		case comparison > 0:
			result.State = "ahead"
		default:
			result.State = "current"
		}
	}
	return result
}

type repositoryTag struct {
	Name string `json:"name"`
}

type tagOutput struct{ buffer bytes.Buffer }

func (b *tagOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 1<<20 {
		return 0, fmt.Errorf("tag response exceeds limit")
	}
	return b.buffer.Write(p)
}

func (s *Server) loadTags(ctx context.Context) ([]repositoryTag, error) {
	// Use an existing gh login for private repositories; never copy its token.
	if s.tagClient == nil {
		if path, err := exec.LookPath("gh"); err == nil {
			cmd := exec.CommandContext(ctx, path, "api", "--hostname", "github.com", tagEndpoint, "--paginate", "--slurp")
			cmd.WaitDelay = 500 * time.Millisecond
			var output tagOutput
			cmd.Stdout = &output
			if err := cmd.Run(); err == nil {
				var pages [][]repositoryTag
				if err := json.Unmarshal(output.buffer.Bytes(), &pages); err != nil {
					return nil, err
				}
				var tags []repositoryTag
				for _, page := range pages {
					tags = append(tags, page...)
				}
				return tags, nil
			}
		}
	}
	client := s.tagClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	var tags []repositoryTag
	for page := 1; page <= 10; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s&page=%d", tagAPI, page), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("User-Agent", "memgov-version-check")
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		response.Body.Close()
		if readErr != nil || len(body) > 1<<20 || response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("cannot read repository tags")
		}
		var batch []repositoryTag
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, err
		}
		tags = append(tags, batch...)
		if !strings.Contains(response.Header.Get("Link"), `rel="next"`) {
			return tags, nil
		}
	}
	return nil, fmt.Errorf("tag pagination exceeds limit")
}

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

func numeric(value string) bool {
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return value != ""
}

func parseVersion(value string) ([]string, bool) {
	parts := versionPattern.FindStringSubmatch(value)
	if parts == nil {
		return nil, false
	}
	for _, id := range strings.Split(parts[4], ".") {
		if numeric(id) && len(id) > 1 && id[0] == '0' {
			return nil, false
		}
	}
	return parts[1:5], true
}

func compareNumber(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

func compareVersions(a, b string) (int, bool) {
	left, ok := parseVersion(a)
	if !ok {
		return 0, false
	}
	right, ok := parseVersion(b)
	if !ok {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if n := compareNumber(left[i], right[i]); n != 0 {
			return n, true
		}
	}
	if left[3] == right[3] {
		return 0, true
	}
	if left[3] == "" {
		return 1, true
	}
	if right[3] == "" {
		return -1, true
	}
	l, r := strings.Split(left[3], "."), strings.Split(right[3], ".")
	for i := 0; i < len(l) && i < len(r); i++ {
		if l[i] == r[i] {
			continue
		}
		ln, rn := numeric(l[i]), numeric(r[i])
		if ln && rn {
			return compareNumber(l[i], r[i]), true
		}
		if ln {
			return -1, true
		}
		if rn {
			return 1, true
		}
		return strings.Compare(l[i], r[i]), true
	}
	if len(l) < len(r) {
		return -1, true
	}
	return 1, true
}
