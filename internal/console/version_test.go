package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type versionTransport func(*http.Request) (*http.Response, error)

func (f versionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVersionComparison(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		want  int
		valid bool
	}{
		{"2.0.0-rc1", "v2.0.0", -1, true}, {"v2.0.0", "2.0.0+build.7", 0, true},
		{"2.10.0", "2.9.9", 1, true}, {"2.0.0-rc.10", "2.0.0-rc.2", 1, true},
		{"2.0.0-alpha", "2.0.0-alpha.1", -1, true}, {"2.0.0-1", "2.0.0-alpha", -1, true},
		{"2.0.0-beta", "2.0.0-alpha", 1, true}, {"dev", "2.0.0", 0, false},
		{"2.0.0-01", "2.0.0", 0, false}, {"02.0.0", "2.0.0", 0, false},
		{"2.0.0", "garbage", 0, false}, {"999999999999999999999.0.0", "3.0.0", 1, true},
	} {
		got, valid := compareVersions(tc.a, tc.b)
		if got != tc.want || valid != tc.valid {
			t.Errorf("%q vs %q: %d,%v", tc.a, tc.b, got, valid)
		}
	}
}

func TestTagCheckFailureStatesAndCache(t *testing.T) {
	for _, tc := range []struct {
		name, version, body, want string
		status                    int
		fail                      bool
	}{
		{"update", "2.0.0-rc1", `[{"name":"v2.0.0","html_url":"https://evil.test"}]`, "update", 200, false},
		{"same", "2.0.0", `[{"name":"v2.0.0"}]`, "current", 200, false},
		{"ahead", "2.1.0", `[{"name":"v2.0.0"}]`, "ahead", 200, false},
		{"dev", "dev", `[{"name":"v2.0.0"}]`, "unknown", 200, false},
		{"no tags", "2.0.0", "[]", "not_found", 200, false},
		{"private inaccessible", "2.0.0", "{}", "unavailable", 404, false},
		{"limited", "2.0.0", "{}", "unavailable", 429, false},
		{"network", "2.0.0", "", "unavailable", 0, true},
		{"bad json", "2.0.0", "bad", "unavailable", 200, false},
		{"numeric order", "2.10.0", `[{"name":"v2.9.0"},{"name":"v2.10.0"},{"name":"v2.8.0"}]`, "current", 200, false},
		{"preview", "2.0.0", `[{"name":"v3.0.0-rc1"}]`, "update", 200, false},
		{"invalid tag", "2.0.0", `[{"name":"<script>"}]`, "not_found", 200, false},
		{"oversize", "2.0.0", strings.Repeat("x", (1<<20)+1), "unavailable", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s := &Server{opts: Options{Version: tc.version}, tagClient: &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.URL.String() != tagAPI+"&page=1" || r.Header.Get("Authorization") != "" || r.Body != nil {
					t.Error("unexpected data or destination")
				}
				if tc.fail {
					return nil, errors.New("offline")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}}
			var wg sync.WaitGroup
			for i := 0; i < 5; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					result := s.checkTag(context.Background())
					if result.State != tc.want || result.CheckedAt.IsZero() {
						t.Errorf("result: %+v", result)
					}
				}()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatal("concurrent checks bypassed cache", calls.Load())
			}
			if strings.Contains(s.tagResult.TagURL, "evil") {
				t.Fatal("trusted remote tag URL")
			}
			s.tagExpires = time.Time{}
			s.checkTag(context.Background())
			if calls.Load() != 2 {
				t.Fatal("expired check was not refreshed")
			}
		})
	}
}

func TestTagEndpointIsIndependentAndChecksOrigin(t *testing.T) {
	s := &Server{opts: Options{Host: "127.0.0.1:8787", Version: "2.0.0"}, tagClient: &http.Client{Transport: versionTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}}
	w := request(t, s, "GET", "/api/v1/version/check", "", nil)
	var result struct {
		OK   bool
		Data TagStatus
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != 200 || !result.OK || result.Data.State != "unavailable" {
		t.Fatal(w.Body.String())
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/v1/version/check", nil)
	r.Header.Set("Origin", "https://external.test")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("cross-origin tag check accepted")
	}
}

func TestTagPaginationAndOutputBounds(t *testing.T) {
	calls := 0
	s := &Server{opts: Options{Version: "2.0.0"}, tagClient: &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `[{"name":"v2.0.0"}]`
		header := make(http.Header)
		if r.URL.Query().Get("page") == "1" {
			// Never follow arbitrary next URLs from a remote response.
			header.Set("Link", `<https://evil.test>; rel="next"`)
		} else {
			if !strings.HasPrefix(r.URL.String(), tagAPI) {
				t.Fatal("followed external pagination link")
			}
			body = `[{"name":"v3.0.0-rc.1"}]`
		}
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	result := s.checkTag(context.Background())
	if calls != 2 || result.State != "update" || result.LatestVersion != "v3.0.0-rc.1" {
		t.Fatal(result, calls)
	}
	var output tagOutput
	if _, err := io.Copy(&output, strings.NewReader(strings.Repeat("x", (1<<20)+1))); err == nil {
		t.Fatal("unbounded CLI output")
	}
}
