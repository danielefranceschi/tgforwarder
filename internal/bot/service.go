package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type ForwardPayload struct {
	UserID      int64        `json:"user_id"`
	UserName    string       `json:"user_name,omitempty"`
	ChatID      int64        `json:"chat_id"`
	ChatType    string       `json:"chat_type,omitempty"`
	MessageID   int          `json:"message_id"`
	Timestamp   time.Time    `json:"timestamp"`
	Text        string       `json:"message_text"`
	Command     string       `json:"command,omitempty"`
	Attachments []Attachment `json:"attachments"`
}

type Attachment struct {
	Type     string                 `json:"type"`
	FileID   string                 `json:"file_id"`
	FileName string                 `json:"file_name,omitempty"`
	MimeType string                 `json:"mime_type,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
	Data     []byte                 `json:"data,omitempty"`
}

type Sender interface {
	Send(ctx context.Context, webhook WebhookConfig, payload ForwardPayload) error
}

type Forwarder struct {
	HTTPClient  *http.Client
	MaxRetries  int
	BaseBackoff time.Duration
	Jitter      func(base time.Duration, attempt int) time.Duration
	Sleep       func(d time.Duration)
}

func (f *Forwarder) Send(ctx context.Context, webhook WebhookConfig, payload ForwardPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	maxRetries := max(f.MaxRetries, 0)

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		lastErr = f.sendOnce(ctx, webhook, body)
		if lastErr == nil {
			return nil
		}

		if attempt == maxRetries {
			break
		}
		wait := f.backoffDuration(attempt + 1)
		if err := f.sleepContext(ctx, wait); err != nil {
			return fmt.Errorf("retry wait canceled: %w", err)
		}
	}

	return fmt.Errorf("forward failed after %d retries: %w", maxRetries, lastErr)
}

func (f *Forwarder) sendOnce(ctx context.Context, webhook WebhookConfig, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if webhook.Header != "" {
		req.Header.Set(webhook.Header, webhook.HeaderValue)
	}

	resp, err := f.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("perform request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("non-success status: %d body=%q", resp.StatusCode, string(respBody))
}

func (f *Forwarder) backoffDuration(attempt int) time.Duration {
	base := f.BaseBackoff
	if base <= 0 {
		base = 200 * time.Millisecond
	}
	// Exponential backoff: base * 2^(attempt-1)
	wait := base << (attempt - 1)
	if f.Jitter != nil {
		return f.Jitter(wait, attempt)
	}
	return defaultJitter(wait, attempt)
}

func (f *Forwarder) sleepContext(ctx context.Context, d time.Duration) error {
	if f.Sleep != nil {
		f.Sleep(d)
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func defaultJitter(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	// Deterministic per-attempt jitter to avoid synchronized retries.
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "attempt-%d", attempt)
	seed := int64(h.Sum64())
	r := rand.New(rand.NewSource(seed))
	extra := time.Duration(r.Int63n(int64(base/2 + 1)))
	return base + extra
}

type Service struct {
	Config      Config
	Forwarder   Sender
	SendMessage func(c tgbotapi.Chattable) (tgbotapi.Message, error)
	Logger      *slog.Logger
	Now         func() time.Time
}

var telegramAPIBaseURL = "https://api.telegram.org"

func (s *Service) processMessage(ctx context.Context, msg *tgbotapi.Message) {
	if msg == nil || msg.From == nil || msg.Chat == nil {
		return
	}
	if !s.isEnabledUser(msg.From.ID) {
		return
	}
	s.Logger.Info("received message", "user_id", msg.From.ID, "user_name", msg.From.UserName, "message_id", msg.MessageID)

	effectiveText := strings.TrimSpace(msg.Text)
	if effectiveText == "" {
		effectiveText = strings.TrimSpace(msg.Caption)
	}

	webhook, ok := matchWebhookByCommand(effectiveText, s.Config.Webhooks)
	if !ok {
		return
	}

	payload := buildForwardPayload(s, msg)

	if err := s.Forwarder.Send(ctx, webhook, payload); err != nil {
		s.Logger.Warn("forwarding failed", "endpoint", webhook.Name, "error", err.Error())
		reply := tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("forwarding to the %s endpoint failed", webhook.Name))
		reply.ReplyToMessageID = msg.MessageID
		if _, sendErr := s.SendMessage(reply); sendErr != nil {
			s.Logger.Warn("failed to send error reply", "error", sendErr.Error())
		}
		return
	}

	s.Logger.Info("message forwarded", "endpoint", webhook.Name, "user_id", msg.From.ID, "message_id", msg.MessageID)
}

func (s *Service) isEnabledUser(userID int64) bool {
	for _, allowed := range s.Config.EnabledUserIDs {
		if allowed == userID {
			return true
		}
	}
	s.Logger.Warn("received message from disabled user", "user_id", userID)
	return false
}

func matchWebhookByCommand(text string, webhooks []WebhookConfig) (WebhookConfig, bool) {
	command, ok := extractCommand(text)
	if !ok {
		return WebhookConfig{}, false
	}

	for _, wh := range webhooks {
		if strings.EqualFold(command, strings.TrimSpace(wh.MatchingString)) {
			return wh, true
		}
	}
	slog.Warn("no matching webhook found for command", "command", command)
	return WebhookConfig{}, false
}

func extractCommand(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", false
	}

	parts := strings.Fields(trimmed)
	if len(parts) == 0 {
		return "", false
	}
	cmd := strings.TrimPrefix(parts[0], "/")
	cmd = strings.SplitN(cmd, "@", 2)[0]
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", false
	}
	return cmd, true
}

func (s *Service) downloadFile(fileID string) ([]byte, error) {

	s.Logger.Debug("resolving file", "file_id", fileID)
	baseURL := strings.TrimRight(telegramAPIBaseURL, "/")
	client := &http.Client{Timeout: 20 * time.Second}

	getFileURL := fmt.Sprintf("%s/bot%s/getFile?file_id=%s", baseURL, s.Config.BotToken, url.QueryEscape(fileID))
	getFileResp, err := client.Get(getFileURL)
	if err != nil {
		return nil, fmt.Errorf("getFile request failed: %w", err)
	}
	defer func() {
		_ = getFileResp.Body.Close()
	}()
	if getFileResp.StatusCode < 200 || getFileResp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(getFileResp.Body, 2048))
		return nil, fmt.Errorf("getFile non-success status: %d body=%q", getFileResp.StatusCode, string(respBody))
	}

	var getFileResult struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.NewDecoder(getFileResp.Body).Decode(&getFileResult); err != nil {
		return nil, fmt.Errorf("decode getFile response: %w", err)
	}
	if !getFileResult.OK {
		return nil, fmt.Errorf("telegram getFile failed: %s", strings.TrimSpace(getFileResult.Description))
	}
	if strings.TrimSpace(getFileResult.Result.FilePath) == "" {
		return nil, fmt.Errorf("telegram getFile response missing file_path")
	}

	s.Logger.Debug("fetching file content", "file_path", getFileResult.Result.FilePath)
	fileURL := fmt.Sprintf("%s/file/bot%s/%s", baseURL, s.Config.BotToken, strings.TrimLeft(getFileResult.Result.FilePath, "/"))
	fileResp, err := client.Get(fileURL)
	if err != nil {
		return nil, fmt.Errorf("file download request failed: %w", err)
	}
	defer func() {
		_ = fileResp.Body.Close()
	}()
	if fileResp.StatusCode < 200 || fileResp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(fileResp.Body, 2048))
		return nil, fmt.Errorf("file download non-success status: %d body=%q", fileResp.StatusCode, string(respBody))
	}

	fileData, err := io.ReadAll(fileResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read downloaded file: %w", err)
	}
	return fileData, nil
}

func buildForwardPayload(s *Service, msg *tgbotapi.Message) ForwardPayload {

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		text = strings.TrimSpace(msg.Caption)
	}
	command, _ := extractCommand(text)
	text = strings.TrimPrefix(text, "/"+command+" ")
	payload := ForwardPayload{
		UserID:      msg.From.ID,
		UserName:    msg.From.UserName,
		ChatID:      msg.Chat.ID,
		ChatType:    msg.Chat.Type,
		MessageID:   msg.MessageID,
		Timestamp:   time.Unix(int64(msg.Date), 0).UTC(),
		Text:        text,
		Command:     command,
		Attachments: make([]Attachment, 0),
	}

	if msg.Document != nil {
		payload.Attachments = append(payload.Attachments, Attachment{
			Type:     "document",
			FileID:   msg.Document.FileID,
			FileName: msg.Document.FileName,
			MimeType: msg.Document.MimeType,
			Metadata: map[string]any{
				"file_size": msg.Document.FileSize,
			},
		})
	}

	if len(msg.Photo) > 0 {
		photo := msg.Photo[0]
		for _, p := range msg.Photo {
			if p.Width*p.Height > photo.Width*photo.Height {
				photo = p
			}
		}
		// download photo
		photoData, err := s.downloadFile(photo.FileID)
		if err != nil {
			s.Logger.Warn("failed to download photo", "error", err.Error())
			return payload
		}
		payload.Attachments = append(payload.Attachments, Attachment{
			Type:   "photo",
			FileID: photo.FileID,
			Metadata: map[string]any{
				"width":  photo.Width,
				"height": photo.Height,
			},
			Data: photoData,
		})
	}

	if msg.Audio != nil {
		audioData, err := s.downloadFile(msg.Audio.FileID)
		if err != nil {
			s.Logger.Warn("failed to download audio", "error", err.Error())
			return payload
		}
		payload.Attachments = append(payload.Attachments, Attachment{
			Type:     "audio",
			FileID:   msg.Audio.FileID,
			FileName: msg.Audio.FileName,
			MimeType: msg.Audio.MimeType,
			Data:     audioData,
		})
	}
	if msg.Video != nil {
		videoData, err := s.downloadFile(msg.Video.FileID)
		if err != nil {
			s.Logger.Warn("failed to download video", "error", err.Error())
			return payload
		}
		payload.Attachments = append(payload.Attachments, Attachment{
			Type:     "video",
			FileID:   msg.Video.FileID,
			FileName: msg.Video.FileName,
			MimeType: msg.Video.MimeType,
			Data:     videoData,
		})
	}
	if msg.Voice != nil {
		voiceData, err := s.downloadFile(msg.Voice.FileID)
		if err != nil {
			s.Logger.Warn("failed to download voice", "error", err.Error())
			return payload
		}
		payload.Attachments = append(payload.Attachments, Attachment{
			Type:     "voice",
			FileID:   msg.Voice.FileID,
			MimeType: msg.Voice.MimeType,
			Data:     voiceData,
		})
	}
	if msg.Animation != nil {
		animationData, err := s.downloadFile(msg.Animation.FileID)
		if err != nil {
			s.Logger.Warn("failed to download animation", "error", err.Error())
			return payload
		}
		payload.Attachments = append(payload.Attachments, Attachment{
			Type:     "animation",
			FileID:   msg.Animation.FileID,
			FileName: msg.Animation.FileName,
			MimeType: msg.Animation.MimeType,
			Data:     animationData,
		})
	}
	if msg.VideoNote != nil {
		videoNoteData, err := s.downloadFile(msg.VideoNote.FileID)
		if err != nil {
			s.Logger.Warn("failed to download video note", "error", err.Error())
			return payload
		}
		payload.Attachments = append(payload.Attachments, Attachment{
			Type:   "video_note",
			FileID: msg.VideoNote.FileID,
			Metadata: map[string]any{
				"length": msg.VideoNote.Length,
			},
			Data: videoNoteData,
		})
	}

	return payload
}
