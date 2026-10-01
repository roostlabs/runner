package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SignatureHeader carries the HMAC of the body when a Webhook has a secret, as
// "sha256=<hex>", so the receiver can tell the Runner's posts from anyone
// else's.
const SignatureHeader = "X-Roost-Signature"

// Webhook posts the event as JSON to one URL.
//
// It is the sink for everything that is not Telegram: Slack, Discord, n8n, a
// script of the developer's own. The body is the Event; the text a chat would
// show rides along as "text", so a receiver that only wants a line has one.
type Webhook struct {
	URL    string
	Secret string
	HTTP   *http.Client
}

func (w Webhook) Name() string { return "webhook" }

// Payload is the JSON body a Webhook posts.
type Payload struct {
	Event
	Text string `json:"text"`
}

// Send posts the event.
func (w Webhook) Send(ctx context.Context, ev Event) error {
	if w.URL == "" {
		return errors.New("webhook: url is required")
	}
	body, err := json.Marshal(Payload{Event: ev, Text: Text(ev)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "roost-runner")
	if w.Secret != "" {
		req.Header.Set(SignatureHeader, Sign(w.Secret, body))
	}

	client := w.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Sign computes the signature header value for a body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether a signature header matches the body, for a receiver
// written in Go and for the test.
func Verify(secret string, body []byte, header string) bool {
	return hmac.Equal([]byte(Sign(secret, body)), []byte(header))
}
