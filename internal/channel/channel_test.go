package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/roostlabs/protocol"
)

// cloud is a stub Cloud API.
//
// handle runs once per connection and receives that connection's ordinal, so a
// test can behave differently on a reconnect. Returning true holds the
// connection open until the test ends; returning false drops it, which is how a
// test forces a reconnect.
type cloud struct {
	url   string
	conns atomic.Int64
	done  chan struct{}
}

func newCloud(t *testing.T, handle func(ctx context.Context, ws *websocket.Conn, n int64) bool) *cloud {
	t.Helper()
	c := &cloud{done: make(chan struct{})}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()

		if !handle(r.Context(), ws, c.conns.Add(1)) {
			return
		}
		// Hold the connection until the test finishes. Waiting on the request
		// context alone is not enough: it is not cancelled when a hijacked
		// client goes away, which would leak this goroutine and stall Close.
		select {
		case <-c.done:
		case <-r.Context().Done():
		}
	}))

	// Cleanup runs last-registered-first: release the handlers, then stop the
	// server.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(c.done) })

	c.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return c
}

func readEnvelope(ctx context.Context, ws *websocket.Conn) (protocol.Envelope, error) {
	_, raw, err := ws.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	err = json.Unmarshal(raw, &env)
	return env, err
}

func writeEnvelope(ctx context.Context, ws *websocket.Conn, t protocol.Type, data any) error {
	env, err := protocol.New("srv-msg", 1, t, data)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, raw)
}

// acceptHello reads the handshake and accepts it.
func acceptHello(ctx context.Context, ws *websocket.Conn, lastSeq map[string]uint64) (protocol.Hello, error) {
	env, err := readEnvelope(ctx, ws)
	if err != nil {
		return protocol.Hello{}, err
	}
	var hello protocol.Hello
	if err := env.Decode(&hello); err != nil {
		return protocol.Hello{}, err
	}
	return hello, writeEnvelope(ctx, ws, protocol.TypeHelloOK, protocol.HelloOK{
		Proto:    protocol.Version,
		ServerTS: 2,
		RunnerID: "r-test",
		LastSeq:  lastSeq,
	})
}

// rejectHello reads the handshake and refuses it.
func rejectHello(ctx context.Context, ws *websocket.Conn, code protocol.ErrorCode, msg string) {
	if _, err := readEnvelope(ctx, ws); err != nil {
		return
	}
	writeEnvelope(ctx, ws, protocol.TypeHelloErr, protocol.HelloErr{Code: code, Msg: msg})
}

func testOptions(url string) Options {
	return Options{
		URL:           url,
		Token:         "rt_token",
		RunnerVersion: "0.0.1-test",
		Host:          protocol.HostInfo{OS: "linux", Arch: "amd64", Docker: "27.0"},
		MinBackoff:    10 * time.Millisecond,
		MaxBackoff:    40 * time.Millisecond,
		PingInterval:  time.Hour, // keepalive must not interfere
	}
}

func noopHandler(context.Context, *Conn, protocol.Envelope) error { return nil }

func TestDialCompletesHandshake(t *testing.T) {
	hellos := make(chan protocol.Hello, 1)
	c := newCloud(t, func(ctx context.Context, ws *websocket.Conn, _ int64) bool {
		hello, err := acceptHello(ctx, ws, map[string]uint64{"T-1": 9})
		if err != nil {
			return false
		}
		hellos <- hello
		return true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := Dial(ctx, testOptions(c.url))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.CloseNow()

	select {
	case hello := <-hellos:
		if hello.Token != "rt_token" {
			t.Errorf("hello token = %q", hello.Token)
		}
		if hello.RunnerVersion != "0.0.1-test" {
			t.Errorf("hello runnerVersion = %q", hello.RunnerVersion)
		}
		if len(hello.ProtoVersions) == 0 || hello.ProtoVersions[0] != protocol.Version {
			t.Errorf("hello protoVersions = %v", hello.ProtoVersions)
		}
		if hello.Host.Docker != "27.0" {
			t.Errorf("hello host = %+v", hello.Host)
		}
	case <-ctx.Done():
		t.Fatal("server never received a hello")
	}

	if conn.RunnerID() != "r-test" {
		t.Errorf("RunnerID() = %q, want r-test", conn.RunnerID())
	}
	if conn.Proto() != protocol.Version {
		t.Errorf("Proto() = %d, want %d", conn.Proto(), protocol.Version)
	}
	if conn.LastSeq()["T-1"] != 9 {
		t.Errorf("LastSeq() = %v, want T-1 at 9", conn.LastSeq())
	}
}

func TestDialSurfacesRejection(t *testing.T) {
	c := newCloud(t, func(ctx context.Context, ws *websocket.Conn, _ int64) bool {
		rejectHello(ctx, ws, protocol.ErrAuthFailed, "unknown token")
		return true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Dial(ctx, testOptions(c.url))
	if err == nil {
		t.Fatal("Dial succeeded against a rejecting server")
	}
	var rejected protocol.Error
	if !errors.As(err, &rejected) {
		t.Fatalf("Dial error = %v, want a protocol.Error", err)
	}
	if rejected.Code != protocol.ErrAuthFailed {
		t.Errorf("code = %q, want %q", rejected.Code, protocol.ErrAuthFailed)
	}
}

// A rejected handshake must not cost seconds: closing politely would wait for a
// close frame the rejecting peer never sends, and that delay lands on the
// caller's context.
func TestDialReturnsPromptlyOnRejection(t *testing.T) {
	c := newCloud(t, func(ctx context.Context, ws *websocket.Conn, _ int64) bool {
		rejectHello(ctx, ws, protocol.ErrAuthFailed, "")
		return true // never answer the closing handshake
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	if _, err := Dial(ctx, testOptions(c.url)); err == nil {
		t.Fatal("Dial succeeded against a rejecting server")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Dial took %v to report a rejection, want well under 2s", elapsed)
	}
}

// Cloud must not settle on a version the Runner never offered.
func TestDialRejectsUnofferedVersion(t *testing.T) {
	c := newCloud(t, func(ctx context.Context, ws *websocket.Conn, _ int64) bool {
		if _, err := readEnvelope(ctx, ws); err != nil {
			return false
		}
		writeEnvelope(ctx, ws, protocol.TypeHelloOK, protocol.HelloOK{
			Proto:    protocol.Version + 99,
			RunnerID: "r-test",
		})
		return true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Dial(ctx, testOptions(c.url))
	var rejected protocol.Error
	if !errors.As(err, &rejected) || rejected.Code != protocol.ErrUnsupportedVersion {
		t.Fatalf("Dial error = %v, want %s", err, protocol.ErrUnsupportedVersion)
	}
}

// Retrying a refused token would hammer Cloud and hide a problem only a human
// can fix, so Run has to give up.
func TestRunGivesUpOnRejection(t *testing.T) {
	c := newCloud(t, func(ctx context.Context, ws *websocket.Conn, _ int64) bool {
		rejectHello(ctx, ws, protocol.ErrAuthFailed, "")
		return true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Run(ctx, testOptions(c.url), noopHandler)
	if err == nil {
		t.Fatal("Run returned nil, want a rejection")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Run kept retrying a rejected token instead of giving up")
	}
	var rejected protocol.Error
	if !errors.As(err, &rejected) || rejected.Code != protocol.ErrAuthFailed {
		t.Fatalf("Run error = %v, want %s", err, protocol.ErrAuthFailed)
	}
	if n := c.conns.Load(); n != 1 {
		t.Errorf("server saw %d connections, want 1", n)
	}
}

func TestRunReconnectsAfterDrop(t *testing.T) {
	reconnected := make(chan struct{})
	c := newCloud(t, func(ctx context.Context, ws *websocket.Conn, n int64) bool {
		if _, err := acceptHello(ctx, ws, nil); err != nil {
			return false
		}
		if n == 1 {
			return false // drop the first connection right after the handshake
		}
		if n == 2 {
			close(reconnected)
		}
		return true
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- Run(ctx, testOptions(c.url), noopHandler) }()

	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("runner never reconnected")
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run error = %v, want context.Canceled", err)
	}
}

// OnConnect carries the state Cloud needs immediately, so it has to run on
// reconnects too, not just the first connection.
func TestOnConnectRunsOnEveryConnection(t *testing.T) {
	secondReady := make(chan struct{})
	firstMessages := make(chan protocol.Type, 4)

	c := newCloud(t, func(ctx context.Context, ws *websocket.Conn, n int64) bool {
		if _, err := acceptHello(ctx, ws, nil); err != nil {
			return false
		}
		// Whatever OnConnect sent must be the first thing after the handshake.
		env, err := readEnvelope(ctx, ws)
		if err != nil {
			return false
		}
		firstMessages <- env.Type

		if n == 1 {
			return false // drop, forcing a reconnect
		}
		if n == 2 {
			close(secondReady)
		}
		return true
	})

	var calls atomic.Int64
	opts := testOptions(c.url)
	opts.OnConnect = func(ctx context.Context, conn *Conn) error {
		calls.Add(1)
		return conn.SendMessage(ctx, protocol.TypeCredStatus,
			protocol.CredStatus{Git: true, LLM: true})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts, noopHandler) }()

	select {
	case <-secondReady:
	case <-time.After(10 * time.Second):
		t.Fatal("second connection never arrived")
	}

	// Both sends happen before secondReady closes, so a fixed drain is safe.
	// Closing the channel instead would race the server goroutine.
	for i := range 2 {
		select {
		case typ := <-firstMessages:
			if typ != protocol.TypeCredStatus {
				t.Errorf("first post-handshake message on connection %d = %s, want %s",
					i+1, typ, protocol.TypeCredStatus)
			}
		default:
			t.Fatalf("server saw only %d post-handshake messages, want 2", i)
		}
	}

	cancel()
	<-done

	if got := calls.Load(); got != 2 {
		t.Errorf("OnConnect ran %d times, want 2", got)
	}
}

func TestHandlerReceivesMessages(t *testing.T) {
	c := newCloud(t, func(ctx context.Context, ws *websocket.Conn, _ int64) bool {
		if _, err := acceptHello(ctx, ws, nil); err != nil {
			return false
		}
		writeEnvelope(ctx, ws, protocol.TypeTaskCancel,
			protocol.TaskCancel{TaskID: "T-7", Reason: "user asked"})
		return true
	})

	got := make(chan protocol.Envelope, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, testOptions(c.url), func(_ context.Context, _ *Conn, env protocol.Envelope) error {
			got <- env
			return nil
		})
	}()

	select {
	case env := <-got:
		if env.Type != protocol.TypeTaskCancel {
			t.Errorf("handler got %s, want %s", env.Type, protocol.TypeTaskCancel)
		}
		var cancelMsg protocol.TaskCancel
		if err := env.Decode(&cancelMsg); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if cancelMsg.TaskID != "T-7" {
			t.Errorf("taskId = %q, want T-7", cancelMsg.TaskID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler never saw the message")
	}

	cancel()
	<-done
}

// One malformed frame is a bad message, not a dead connection: dropping it must
// not cost the messages behind it.
func TestReadLoopSkipsMalformedEnvelope(t *testing.T) {
	c := newCloud(t, func(ctx context.Context, ws *websocket.Conn, _ int64) bool {
		if _, err := acceptHello(ctx, ws, nil); err != nil {
			return false
		}
		// Parseable JSON, but no id and no type: not a usable envelope.
		ws.Write(ctx, websocket.MessageText, []byte(`{"v":1,"ts":3,"data":{}}`))
		writeEnvelope(ctx, ws, protocol.TypeChat, protocol.Chat{TaskID: "T-1", Text: "still here"})
		return true
	})

	got := make(chan protocol.Envelope, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, testOptions(c.url), func(_ context.Context, _ *Conn, env protocol.Envelope) error {
			got <- env
			return nil
		})
	}()

	select {
	case env := <-got:
		if env.Type != protocol.TypeChat {
			t.Errorf("handler got %s, want the malformed frame skipped and %s delivered",
				env.Type, protocol.TypeChat)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler never saw the message after the malformed one")
	}

	cancel()
	<-done
}

func TestJitterStaysInTheTopHalf(t *testing.T) {
	const d = 800 * time.Millisecond
	for range 200 {
		got := jitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%v) = %v, want between %v and %v", d, got, d/2, d)
		}
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		id := newID()
		if seen[id] {
			t.Fatalf("newID repeated %q", id)
		}
		seen[id] = true
	}
}
