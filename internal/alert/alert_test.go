package alert

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func event() Event {
	return Event{
		Kind:    KindFailed,
		TaskID:  "T-1",
		Ticket:  Ticket{Provider: "linear", ID: "ENG-7", Title: "Fix the paginator", URL: "https://linear.app/ENG-7"},
		Reason:  "agent: looping without progress",
		CostUSD: 0.42,
	}
}

func TestTextReadsAsOneMessage(t *testing.T) {
	got := Text(event())
	for _, want := range []string{"task T-1 failed", "ENG-7 Fix the paginator", "looping", "$0.42", "https://linear.app/ENG-7"} {
		if !strings.Contains(got, want) {
			t.Errorf("text lacks %q:\n%s", want, got)
		}
	}
	done := Text(Event{Kind: KindDone, TaskID: "T-2", PRURL: "https://github.com/x/y/pull/3"})
	if !strings.Contains(done, "opened a pull request") || !strings.Contains(done, "/pull/3") {
		t.Errorf("done text = %q", done)
	}
	if !strings.Contains(Text(Event{Kind: KindApproval, TaskID: "T-3"}), "waiting for your approval") {
		t.Error("approval text does not say what to do")
	}
}

func TestTelegramPostsToTheBot(t *testing.T) {
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := Telegram{Token: "123:abc", ChatID: "42", BaseURL: srv.URL}
	if err := tg.Send(context.Background(), event()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if path != "/bot123:abc/sendMessage" {
		t.Errorf("path = %q", path)
	}
	if body["chat_id"] != "42" || !strings.Contains(body["text"].(string), "task T-1 failed") {
		t.Errorf("body = %v", body)
	}
}

func TestTelegramErrorsNeverCarryTheToken(t *testing.T) {
	tg := Telegram{Token: "123:secret", ChatID: "42", BaseURL: "http://127.0.0.1:1"}
	err := tg.Send(context.Background(), event())
	if err == nil {
		t.Fatal("a closed port did not fail")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestWebhookPostsSignedJSON(t *testing.T) {
	var got Payload
	var sig string
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		sig = r.Header.Get(SignatureHeader)
		json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	hook := Webhook{URL: srv.URL, Secret: "s3cret"}
	if err := hook.Send(context.Background(), event()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.Kind != KindFailed || got.TaskID != "T-1" || got.Ticket.ID != "ENG-7" || got.Text == "" {
		t.Errorf("payload = %+v", got)
	}
	if !Verify("s3cret", raw, sig) {
		t.Errorf("signature %q does not verify", sig)
	}
	if Verify("other", raw, sig) {
		t.Error("the signature verifies with the wrong secret")
	}
}

func TestWebhookRefusesNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	if err := (Webhook{URL: srv.URL}).Send(context.Background(), event()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want a 403", err)
	}
}

// recording is a sink that remembers what it was given.
type recording struct {
	mu   sync.Mutex
	got  []Event
	fail error
}

func (r *recording) Name() string { return "recording" }
func (r *recording) Send(_ context.Context, ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, ev)
	return r.fail
}

func TestNotifierFansOutAndFilters(t *testing.T) {
	a, b := &recording{}, &recording{fail: errors.New("down")}
	n := New(slog.New(slog.NewTextHandler(io.Discard, nil)), []Kind{KindFailed, KindApproval}, a, b)

	n.Alert(context.Background(), Event{Kind: KindFailed, TaskID: "T-1"})
	n.Alert(context.Background(), Event{Kind: KindDone, TaskID: "T-2"})
	n.Alert(context.Background(), Event{Kind: KindApproval, TaskID: "T-3"})
	n.Wait()

	for _, r := range []*recording{a, b} {
		ids := map[string]bool{}
		for _, ev := range r.got {
			ids[ev.TaskID] = true
			if ev.TS == 0 {
				t.Error("the timestamp was not filled in")
			}
		}
		// Deliveries run concurrently, so only the set is checked.
		if len(r.got) != 2 || !ids["T-1"] || !ids["T-3"] {
			t.Errorf("%s got %+v, want T-1 and T-3 only", r.Name(), r.got)
		}
	}
}

func TestNilNotifierIsSilent(t *testing.T) {
	var n *Notifier
	n.Alert(context.Background(), event()) // must not panic
	n.Wait()
}

// A cancelled task is still reported: delivery is detached from its context.
func TestDeliveryOutlivesTheContext(t *testing.T) {
	r := &recording{}
	n := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.Alert(ctx, event())
	n.Wait()
	if len(r.got) != 1 {
		t.Errorf("got %d events, want 1", len(r.got))
	}
}
