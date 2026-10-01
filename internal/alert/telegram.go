package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Telegram sends to one chat through a bot.
//
// The developer makes the bot with @BotFather, messages it once, and reads
// the chat id from getUpdates; both go in the config on the VPS. The token is
// part of the URL Telegram defines, which is why it also goes in the redaction
// filter: a logged request line would otherwise carry it.
type Telegram struct {
	Token  string
	ChatID string
	// BaseURL overrides the API host, for a test.
	BaseURL string
	HTTP    *http.Client
}

func (t Telegram) Name() string { return "telegram" }

// Send posts the event's text as a message.
func (t Telegram) Send(ctx context.Context, ev Event) error {
	if t.Token == "" || t.ChatID == "" {
		return errors.New("telegram: token and chat id are required")
	}
	base := t.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	body, err := json.Marshal(map[string]any{
		"chat_id": t.ChatID,
		"text":    Text(ev),
		// Plain text: a ticket title with an underscore is not markup.
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(base, "/")+"/bot"+t.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := t.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// The error would carry the URL, and the URL carries the token.
		return errors.New("telegram: request failed: " + redactErr(err, t.Token))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// redactErr strips the token out of a transport error's text.
func redactErr(err error, token string) string {
	return strings.ReplaceAll(err.Error(), token, "***")
}
