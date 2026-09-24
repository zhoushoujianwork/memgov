package dingtalkapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func TestReceiverDownloadsBoundGroupPictureWithoutPersistingCapability(t *testing.T) {
	image, err := base64.StdEncoding.DecodeString(tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	frame := richTextFrame([]any{
		map[string]any{"text": "请看这个错误："},
		map[string]any{"type": "picture", "downloadCode": "DOWNLOAD_SECRET"},
	})
	frame = strings.Replace(frame, `"conversationType":"1"`, `"conversationType":"2","isInAtList":true`, 1)
	stream := &fakeStream{frames: []string{frame}}
	a := adapter(stream)
	a.MediaRoot = filepath.Join(t.TempDir(), "runtime", "media")
	a.APIBase = "https://api.dingtalk.com"
	a.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case accessTokenPath:
			body = `{"accessToken":"TOKEN_SECRET","expireIn":7200}`
		case messageFileDownloadPath:
			var input map[string]string
			if err := json.NewDecoder(req.Body).Decode(&input); err != nil || input["downloadCode"] != "DOWNLOAD_SECRET" || input["robotCode"] != "bot-1" || req.Header.Get("x-acs-dingtalk-access-token") != "TOKEN_SECRET" {
				t.Fatalf("wrong picture download request: %+v %v", input, err)
			}
			body = `{"downloadUrl":"https://media.aliyuncs.com/picture"}`
		case "/picture":
			if req.URL.Host != "media.aliyuncs.com" {
				t.Fatalf("untrusted picture URL: %s", req.URL)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(image))), Header: make(http.Header)}, nil
		default:
			t.Fatalf("unexpected request: %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	var received core.NormalizedEvent
	if err := a.RunReceiver(context.Background(), config(), channel.ReceiverOptions{Handle: func(_ context.Context, event core.NormalizedEvent) error {
		received = event
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if len(received.Attachments) != 1 || received.Attachments[0].MediaType != "image/png" || received.Attachments[0].ResourceID == "" || strings.Contains(received.Body, "内容未读取") {
		t.Fatalf("picture not readable: %+v", received)
	}
	data, err := os.ReadFile(filepath.Join(a.MediaRoot, received.Attachments[0].ResourceID))
	if err != nil || string(data) != string(image) {
		t.Fatal("picture bytes were not stored")
	}
	for _, secret := range []string{"DOWNLOAD_SECRET", "TOKEN_SECRET", "media.aliyuncs.com"} {
		if strings.Contains(core.JSON(received), secret) {
			t.Fatalf("picture capability leaked: %s", secret)
		}
	}
}

func TestStandalonePictureIsParsedAndUntrustedDownloadHostIsRejected(t *testing.T) {
	raw := strings.Replace(frame(""), `"msgtype":"text","senderId"`, `"msgtype":"picture","content":{"downloadCode":"CAPABILITY"},"senderId"`, 1)
	event, err := ParseFrame(config(), []byte(raw))
	if err != nil || len(event.Attachments) != 1 || !strings.Contains(event.Body, "内容未读取") || strings.Contains(event.Payload, "CAPABILITY") {
		t.Fatalf("standalone picture parse: %+v %v", event, err)
	}
	for _, value := range []string{"https://127.0.0.1/file", "https://media.aliyuncs.com.evil.test/file", "http://media.aliyuncs.com/file", "https://user@media.aliyuncs.com/file"} {
		if trustedMediaURL(value) {
			t.Fatalf("accepted unsafe picture URL %s", value)
		}
	}
}

func TestFailedPictureDownloadKeepsExplicitUnreadMarker(t *testing.T) {
	raw := strings.Replace(frame(""), `"msgtype":"text","senderId"`, `"msgtype":"picture","content":{"downloadCode":"CAPABILITY"},"senderId"`, 1)
	stream := &fakeStream{frames: []string{raw}}
	a := adapter(stream)
	a.MediaRoot = filepath.Join(t.TempDir(), "runtime", "media")
	a.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"accessToken":"token","expireIn":7200}`
		if req.URL.Path == messageFileDownloadPath {
			body = `{"downloadUrl":"https://127.0.0.1/private"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	var event core.NormalizedEvent
	if err := a.RunReceiver(context.Background(), config(), channel.ReceiverOptions{Handle: func(_ context.Context, e core.NormalizedEvent) error {
		event = e
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if len(stream.results) != 1 || stream.results[0] != nil || !strings.Contains(event.Body, "内容未读取") || len(event.Attachments) != 1 || event.Attachments[0].ResourceID != "" {
		t.Fatalf("failed download was presented as readable or not stored: %+v %+v", event, stream.results)
	}
}
