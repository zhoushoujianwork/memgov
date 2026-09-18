package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type startupWriter struct {
	once   sync.Once
	output chan string
}

func (w *startupWriter) Write(p []byte) (int, error) {
	text := string(p)
	w.once.Do(func() { w.output <- text })
	return len(p), nil
}

func TestUIStopsWithoutControllingRuntimes(t *testing.T) {
	home := t.TempDir()
	if code, value := invoke(t, home, "", "init"); code != 0 {
		t.Fatal(value)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &startupWriter{output: make(chan string, 1)}
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"--home", home, "ui", "--port", "0"}, strings.NewReader(""), output, io.Discard)
	}()
	var startup string
	select {
	case startup = <-output.output:
	case <-time.After(10 * time.Second):
		t.Fatal("UI did not start")
	}
	var url string
	for _, line := range strings.Split(startup, "\n") {
		if strings.HasPrefix(line, "http://127.0.0.1:") {
			url = strings.Split(line, "#")[0]
		}
	}
	if url == "" {
		t.Fatal(startup)
	}
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !bytes.Contains(body, []byte(`id="root"`)) || !bytes.Contains(body, []byte(`/assets/`)) {
		t.Fatalf("no UI: %s", body)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal(code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UI did not shut down")
	}
	_, value := invoke(t, home, "", "runtime", "status")
	if values, ok := value["data"].([]any); !ok || len(values) != 0 {
		t.Fatal("UI created runtime state", value)
	}
}
func TestUIValidationDoesNotInitialize(t *testing.T) {
	home := t.TempDir() + "/missing"
	if code, value := invoke(t, home, "", "ui", "--port", "-1"); code != 2 {
		t.Fatal(code, value)
	}
	if code, value := invoke(t, home, "", "ui", "--port", "0"); code != 4 {
		t.Fatal(code, value)
	}
}

func TestUISkillDiscoveryDoesNotBlockOrDuplicateScans(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	resolve := backgroundSkillResolver(func(core.RuntimeSkillPolicy) ([]core.RuntimeSkill, error) {
		calls.Add(1)
		close(started)
		<-release
		return []core.RuntimeSkill{{Name: "example"}}, nil
	})
	policy := core.RuntimeSkillPolicy{Inherit: "executor"}
	if _, err := resolve(policy); core.ErrorCode(err) != "unavailable" {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scan not dispatched")
	}
	withDigest := policy
	withDigest.Resolved = []core.RuntimeSkill{{Name: "applied-result", Digest: "previous"}}
	if _, err := resolve(withDigest); core.ErrorCode(err) != "unavailable" {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate skill scans")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		skills, err := resolve(policy)
		if err == nil {
			if len(skills) != 1 || skills[0].Name != "example" {
				t.Fatal(skills)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("completed discovery not published")
}
