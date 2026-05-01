package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestMatchWebhookByCommand(t *testing.T) {
	cfg := Config{
		Webhooks: []WebhookConfig{
			{Name: "abc endpoint", MatchingString: "abc", URL: "http://localhost/abc"},
			{Name: "def endpoint", MatchingString: "def", URL: "http://localhost/def"},
		},
	}

	tests := []struct {
		name      string
		text      string
		wantName  string
		wantFound bool
	}{
		{name: "basic command", text: "/abc hello", wantName: "abc endpoint", wantFound: true},
		{name: "command with bot mention", text: "/abc@mybot payload", wantName: "abc endpoint", wantFound: true},
		{name: "unknown command", text: "/zzz payload", wantFound: false},
		{name: "not a command", text: "plain text", wantFound: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := matchWebhookByCommand(tt.text, cfg.Webhooks)
			if ok != tt.wantFound {
				t.Fatalf("match found = %v, want %v", ok, tt.wantFound)
			}
			if !ok {
				return
			}
			if got.Name != tt.wantName {
				t.Fatalf("matched webhook name = %q, want %q", got.Name, tt.wantName)
			}
		})
	}
}

func TestBuildForwardPayload(t *testing.T) {
	msg := &tgbotapi.Message{
		MessageID: 42,
		Date:      int(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix()),
		Text:      "/abc hello",
		From: &tgbotapi.User{
			ID:       101,
			UserName: "alice",
		},
		Chat: &tgbotapi.Chat{
			ID:   777,
			Type: "private",
		},
	}

	payload := buildForwardPayload(nil, msg)

	if payload.UserID != 101 {
		t.Fatalf("user id = %d, want 101", payload.UserID)
	}
	if payload.ChatID != 777 {
		t.Fatalf("chat id = %d, want 777", payload.ChatID)
	}
	if payload.Text != "hello" {
		t.Fatalf("text = %q, want hello", payload.Text)
	}
}

func TestForwarderSend_SetsOptionalHeader(t *testing.T) {
	var gotAuth string
	var gotPayload ForwardPayload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Webhook-Auth")
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	fwd := &Forwarder{
		HTTPClient: server.Client(),
	}
	webhook := WebhookConfig{
		Name:        "abc",
		URL:         server.URL,
		Header:      "X-Webhook-Auth",
		HeaderValue: "secret",
	}
	payload := ForwardPayload{
		UserID: 88,
		Text:   "/abc ping",
	}

	err := fwd.Send(context.Background(), webhook, payload)
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}
	if gotAuth != "secret" {
		t.Fatalf("header value = %q, want secret", gotAuth)
	}
	if gotPayload.UserID != 88 {
		t.Fatalf("payload user id = %d, want 88", gotPayload.UserID)
	}
}

func TestForwarderSend_RetriesWithExponentialBackoffAndJitter(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		current := calls
		mu.Unlock()

		if current <= 3 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("temporary failure"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var sleeps []time.Duration
	fwd := &Forwarder{
		HTTPClient:  server.Client(),
		MaxRetries:  3,
		BaseBackoff: 100 * time.Millisecond,
		Jitter: func(_ time.Duration, attempt int) time.Duration {
			// Deterministic jitter for test assertions.
			return (100 * time.Millisecond << (attempt - 1)) + (time.Duration(attempt) * 10 * time.Millisecond)
		},
		Sleep: func(d time.Duration) {
			sleeps = append(sleeps, d)
		},
	}

	err := fwd.Send(context.Background(), WebhookConfig{Name: "abc", URL: server.URL}, ForwardPayload{})
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 4 {
		t.Fatalf("calls = %d, want 4 (1 initial + 3 retries)", gotCalls)
	}

	wantSleeps := []time.Duration{
		110 * time.Millisecond,
		220 * time.Millisecond,
		430 * time.Millisecond,
	}
	if !reflect.DeepEqual(sleeps, wantSleeps) {
		t.Fatalf("sleep durations = %v, want %v", sleeps, wantSleeps)
	}
}

func TestForwarderSend_FailsAfterMaxRetries(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	fwd := &Forwarder{
		HTTPClient:  server.Client(),
		MaxRetries:  3,
		BaseBackoff: 100 * time.Millisecond,
		Jitter: func(base time.Duration, _ int) time.Duration {
			return base
		},
		Sleep: func(_ time.Duration) {},
	}

	err := fwd.Send(context.Background(), WebhookConfig{Name: "abc", URL: server.URL}, ForwardPayload{})
	if err == nil {
		t.Fatalf("Send() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "non-success status") {
		t.Fatalf("error = %q, want non-success status", err.Error())
	}

	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 4 {
		t.Fatalf("calls = %d, want 4 (1 initial + 3 retries)", gotCalls)
	}
}

func TestProcessMessage_ReplyOnForwardFailure(t *testing.T) {
	t.SkipNow()
	sendCalls := make([]tgbotapi.Chattable, 0, 1)

	svc := &Service{
		Config: Config{
			EnabledUserIDs: []int64{101},
			Webhooks: []WebhookConfig{
				{Name: "abc", URL: "http://localhost/abc", MatchingString: "abc"},
			},
		},
		Forwarder: &stubForwarder{
			err: errors.New("network failure"),
		},
		SendMessage: func(c tgbotapi.Chattable) (tgbotapi.Message, error) {
			sendCalls = append(sendCalls, c)
			return tgbotapi.Message{}, nil
		},
		Now: func() time.Time {
			return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		},
	}

	msg := &tgbotapi.Message{
		MessageID: 1,
		Date:      int(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix()),
		Text:      "/abc hello",
		From:      &tgbotapi.User{ID: 101},
		Chat:      &tgbotapi.Chat{ID: 999},
	}

	svc.processMessage(context.Background(), msg)

	if len(sendCalls) != 1 {
		t.Fatalf("send calls = %d, want 1", len(sendCalls))
	}

	replyCfg, ok := sendCalls[0].(tgbotapi.MessageConfig)
	if !ok {
		t.Fatalf("sent message type = %T, want MessageConfig", sendCalls[0])
	}
	if !strings.Contains(replyCfg.Text, "forwarding to the abc endpoint failed") {
		t.Fatalf("reply text = %q, missing expected error message", replyCfg.Text)
	}
}

type stubForwarder struct {
	err error
}

func (s *stubForwarder) Send(_ context.Context, _ WebhookConfig, _ ForwardPayload) error {
	return s.err
}

func TestServiceDownloadFile(t *testing.T) {
	t.SkipNow()
	const (
		token  = "test-token"
		fileID = "file-123"
	)

	const filePath = "photos/file_1.jpg"
	wantData := []byte("image-bytes")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fmt.Sprintf("/bot%s/getFile", token):
			if got := r.URL.Query().Get("file_id"); got != fileID {
				t.Fatalf("file_id query = %q, want %q", got, fileID)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"` + filePath + `"}}`))
		case fmt.Sprintf("/file/bot%s/%s", token, filePath):
			_, _ = w.Write(wantData)
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	origBaseURL := telegramAPIBaseURL
	telegramAPIBaseURL = server.URL
	defer func() { telegramAPIBaseURL = origBaseURL }()

	svc := &Service{
		Config: Config{
			BotToken: token,
		},
	}

	gotData, err := svc.downloadFile(fileID)
	if err != nil {
		t.Fatalf("downloadFile() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(gotData, wantData) {
		t.Fatalf("downloadFile() data = %q, want %q", string(gotData), string(wantData))
	}
}

func TestServiceDownloadFile_GetFileError(t *testing.T) {
	t.SkipNow()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"description":"file not found"}`))
	}))
	defer server.Close()

	origBaseURL := telegramAPIBaseURL
	telegramAPIBaseURL = server.URL
	defer func() { telegramAPIBaseURL = origBaseURL }()

	svc := &Service{
		Config: Config{
			BotToken: "test-token",
		},
	}

	_, err := svc.downloadFile("missing-file")
	if err == nil {
		t.Fatalf("downloadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "file not found") {
		t.Fatalf("downloadFile() error = %q, want to contain file not found", err.Error())
	}
}
