// Package channel maintains the Runner's outbound connection to Cloud.
//
// The Runner always dials and Cloud never connects inward, so the VPS needs no
// open ports. One connection multiplexes every stream.
//
// A dropped connection is expected, not exceptional. Tasks keep running while it
// is down, events keep landing in the event-store, and this package reconnects
// with exponential backoff and jitter. The channel is observation and control,
// not life support.
package channel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/roostlabs/protocol"
)

// backoffResetAfter is how long a connection must survive before its failure
// counts as fresh rather than as part of a reconnect loop. Without this, a
// channel that flaps every few minutes would keep escalating to the maximum
// delay forever.
const backoffResetAfter = 60 * time.Second

// dialTimeout bounds one connection attempt, handshake included.
const dialTimeout = 30 * time.Second

// Handler processes one inbound message.
//
// Returning an error tears the connection down and triggers a reconnect, so
// return one only for failures that make the connection untrustworthy. In
// particular a Type the handler does not recognize must be ignored, never
// treated as an error: that is what lets a newer Cloud talk to this Runner.
type Handler func(ctx context.Context, c *Conn, env protocol.Envelope) error

// Options configures the channel.
type Options struct {
	// URL is the Cloud channel endpoint, normally wss://.
	URL string
	// Token authenticates this Runner.
	Token string
	// RunnerVersion is reported in the handshake.
	RunnerVersion string
	// Host describes the VPS.
	Host protocol.HostInfo
	// OnConnect runs after a successful handshake and before any message is
	// read, on every connection including reconnects. Use it to send the state
	// Cloud needs immediately, such as credential flags. An error here fails
	// the session and triggers a reconnect.
	OnConnect func(ctx context.Context, c *Conn) error
	// Logger receives channel diagnostics. Nil discards them.
	Logger *slog.Logger

	// MinBackoff, MaxBackoff and PingInterval override the protocol defaults.
	// Zero means use the default. They exist so tests do not have to wait
	// seconds for a reconnect.
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
	PingInterval time.Duration
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (o Options) minBackoff() time.Duration {
	if o.MinBackoff > 0 {
		return o.MinBackoff
	}
	return protocol.ReconnectMinBackoff
}

func (o Options) maxBackoff() time.Duration {
	if o.MaxBackoff > 0 {
		return o.MaxBackoff
	}
	return protocol.ReconnectMaxBackoff
}

func (o Options) pingInterval() time.Duration {
	if o.PingInterval > 0 {
		return o.PingInterval
	}
	return protocol.PingInterval
}

// Conn is one live connection to Cloud, after a completed handshake.
type Conn struct {
	ws  *websocket.Conn
	log *slog.Logger

	proto    int
	runnerID string
	lastSeq  map[string]uint64
}

// Proto is the protocol version both sides settled on.
func (c *Conn) Proto() int { return c.proto }

// RunnerID is the id Cloud assigned this Runner.
func (c *Conn) RunnerID() string { return c.runnerID }

// LastSeq is the highest seq Cloud holds per active task, from the handshake.
// Events after these are the ones that need replaying.
func (c *Conn) LastSeq() map[string]uint64 { return c.lastSeq }

// Dial opens a connection and completes the handshake. A returned
// protocol.Error means Cloud rejected the Runner rather than the network
// failing, and retrying will not help.
func Dial(ctx context.Context, opts Options) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, opts.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("channel: dial %s: %w", opts.URL, err)
	}
	c := &Conn{ws: ws, log: opts.logger()}
	if err := c.handshake(ctx, opts); err != nil {
		// Drop the socket rather than closing it politely: a peer that just
		// rejected the handshake owes us no close frame, and waiting for one
		// would burn seconds of the caller's context.
		ws.CloseNow()
		return nil, err
	}
	return c, nil
}

// Close ends the connection with the closing handshake, which waits for the
// peer to acknowledge. Use it only when the peer is expected to still be there.
func (c *Conn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "")
}

// CloseNow drops the connection without the closing handshake.
//
// This is the right close for a connection that already failed. A graceful
// Close waits for a close frame the peer may never send, and the websocket
// library allows five seconds for it — five seconds added to every reconnect.
func (c *Conn) CloseNow() error {
	return c.ws.CloseNow()
}

// Send writes an already-built envelope.
func (c *Conn) Send(ctx context.Context, env protocol.Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("channel: encode %s: %w", env.Type, err)
	}
	if err := c.ws.Write(ctx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("channel: send %s: %w", env.Type, err)
	}
	return nil
}

// SendMessage wraps data in an envelope and sends it.
func (c *Conn) SendMessage(ctx context.Context, t protocol.Type, data any) error {
	env, err := protocol.New(newID(), time.Now().UnixMilli(), t, data)
	if err != nil {
		return fmt.Errorf("channel: %w", err)
	}
	return c.Send(ctx, env)
}

// SendTaskMessage wraps data in an envelope bound to a task and its seq, which
// is what task events and state changes travel as.
func (c *Conn) SendTaskMessage(ctx context.Context, t protocol.Type, taskID string, seq uint64, data any) error {
	env, err := protocol.New(newID(), time.Now().UnixMilli(), t, data)
	if err != nil {
		return fmt.Errorf("channel: %w", err)
	}
	return c.Send(ctx, env.ForTask(taskID, seq))
}

// Receive reads the next message. A non-text frame or unparseable JSON is an
// error, because at that point the stream cannot be trusted to be in sync.
func (c *Conn) Receive(ctx context.Context) (protocol.Envelope, error) {
	typ, raw, err := c.ws.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, fmt.Errorf("channel: read: %w", err)
	}
	if typ != websocket.MessageText {
		return protocol.Envelope{}, fmt.Errorf("channel: got %s frame, want text", typ)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return protocol.Envelope{}, fmt.Errorf("channel: decode envelope: %w", err)
	}
	return env, nil
}

func (c *Conn) handshake(ctx context.Context, opts Options) error {
	hello := protocol.Hello{
		Token:         opts.Token,
		RunnerVersion: opts.RunnerVersion,
		ProtoVersions: protocol.SupportedVersions(),
		Host:          opts.Host,
	}
	if err := c.SendMessage(ctx, protocol.TypeHello, hello); err != nil {
		return err
	}

	env, err := c.Receive(ctx)
	if err != nil {
		return err
	}
	switch env.Type {
	case protocol.TypeHelloOK:
		var ok protocol.HelloOK
		if err := env.Decode(&ok); err != nil {
			return fmt.Errorf("channel: %w", err)
		}
		if _, agreed := protocol.Negotiate([]int{ok.Proto}, protocol.SupportedVersions()); !agreed {
			// Cloud picked something we never offered; treat it as a rejection
			// so Run stops instead of reconnecting into the same wall.
			return protocol.Error{
				Code: protocol.ErrUnsupportedVersion,
				Ref:  env.ID,
				Msg: fmt.Sprintf("cloud chose proto %d, runner speaks %v",
					ok.Proto, protocol.SupportedVersions()),
			}
		}
		c.proto, c.runnerID, c.lastSeq = ok.Proto, ok.RunnerID, ok.LastSeq
		c.log.Info("channel up", "runnerId", ok.RunnerID, "proto", ok.Proto, "replayFrom", ok.LastSeq)
		return nil

	case protocol.TypeHelloErr:
		var e protocol.HelloErr
		if err := env.Decode(&e); err != nil {
			return fmt.Errorf("channel: %w", err)
		}
		return protocol.Error{Code: e.Code, Ref: env.ID, Msg: e.Msg}

	default:
		return fmt.Errorf("channel: expected %s or %s, got %s",
			protocol.TypeHelloOK, protocol.TypeHelloErr, env.Type)
	}
}

// Run keeps a connection to Cloud up until ctx ends, reconnecting with
// exponential backoff and jitter.
//
// It gives up only on a rejection that a retry cannot fix — a bad token or an
// unsupported protocol version — because looping on those would hammer Cloud
// and hide a problem that needs a human.
func Run(ctx context.Context, opts Options, h Handler) error {
	log := opts.logger()
	backoff := opts.minBackoff()

	for {
		start := time.Now()
		err := session(ctx, opts, h)

		// A definitive rejection is reported even if the context expired at the
		// same moment: it tells the operator what is actually wrong, where a
		// bare cancellation would not.
		var rejected protocol.Error
		if errors.As(err, &rejected) {
			switch rejected.Code {
			case protocol.ErrAuthFailed, protocol.ErrUnsupportedVersion:
				return fmt.Errorf("channel: cloud rejected this runner: %w", rejected)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if time.Since(start) >= backoffResetAfter {
			backoff = opts.minBackoff()
		}
		wait := jitter(backoff)
		log.Warn("channel down, reconnecting", "err", err, "in", wait)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff = min(backoff*2, opts.maxBackoff())
	}
}

// session runs one connection from dial to failure.
func session(ctx context.Context, opts Options, h Handler) error {
	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	c, err := Dial(dialCtx, opts)
	cancelDial()
	if err != nil {
		return err
	}

	err = c.serve(ctx, opts, h)

	// Spend the closing handshake only on a deliberate shutdown, where Cloud is
	// still listening and will answer it. After a failure the peer is usually
	// already gone, and waiting would delay the reconnect.
	if ctx.Err() != nil {
		c.Close()
	} else {
		c.CloseNow()
	}
	return err
}

// serve runs the message loops of an established connection.
func (c *Conn) serve(ctx context.Context, opts Options, h Handler) error {
	if opts.OnConnect != nil {
		if err := opts.OnConnect(ctx, c); err != nil {
			return fmt.Errorf("channel: on connect: %w", err)
		}
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()

	// Either goroutine failing ends the session; the deferred stop unblocks the
	// other one.
	fail := make(chan error, 2)
	go func() { fail <- c.keepalive(ctx, opts.pingInterval()) }()
	go func() { fail <- c.readLoop(ctx, h) }()
	return <-fail
}

func (c *Conn) readLoop(ctx context.Context, h Handler) error {
	for {
		env, err := c.Receive(ctx)
		if err != nil {
			return err
		}
		if err := env.Validate(); err != nil {
			// A malformed envelope is one bad message, not a dead connection.
			c.log.Warn("dropping malformed message", "err", err)
			continue
		}
		if err := h(ctx, c, env); err != nil {
			return fmt.Errorf("channel: handling %s: %w", env.Type, err)
		}
	}
}

// keepalive pings on an interval and reports the peer dead once enough pings go
// unanswered.
func (c *Conn) keepalive(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	misses := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, interval)
			err := c.ws.Ping(pingCtx)
			cancel()
			if err == nil {
				misses = 0
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			misses++
			if misses >= protocol.MissedPongsDead {
				return fmt.Errorf("channel: %d pings unanswered: %w", misses, err)
			}
			c.log.Debug("ping unanswered", "misses", misses, "err", err)
		}
	}
}

// jitter spreads a delay over the top half of the backoff window, so many
// Runners reconnecting after a Cloud restart do not arrive in lockstep.
func jitter(d time.Duration) time.Duration {
	half := int64(d / 2)
	if half <= 0 {
		return d
	}
	return d/2 + time.Duration(mrand.Int64N(half+1))
}

// newID returns a message id. Envelopes need one and the protocol package takes
// it from the caller, which keeps that package dependency-free.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Not reachable in practice, but a message must never be dropped for
		// want of an id.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}
