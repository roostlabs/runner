package main

import (
	"context"
	"sync"
	"time"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/channel"
	"github.com/roostlabs/runner/internal/metrics"
)

// metricsInterval is how often a sample goes up while Cloud is subscribed.
const metricsInterval = 5 * time.Second

// metricsStream is the one metrics loop a Runner runs, or none.
//
// A subscription belongs to a connection: Cloud subscribes when a dashboard
// is watching and again after every reconnect, so a new connection starts
// with the stream off and the loop is bound to the connection that asked.
type metricsStream struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

// startMetrics begins sampling onto c, replacing any loop already running.
func (s *service) startMetrics(c *channel.Conn) {
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	if s.metrics.cancel != nil {
		s.metrics.cancel()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.metrics.cancel = cancel
	s.log.Info("metrics stream on", "every", metricsInterval)
	go s.sampleMetrics(ctx, c)
}

// stopMetrics ends the loop. Safe when none is running.
func (s *service) stopMetrics() {
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	if s.metrics.cancel != nil {
		s.metrics.cancel()
		s.metrics.cancel = nil
		s.log.Info("metrics stream off")
	}
}

// sampleMetrics sends one sample per interval until stopped or until the
// connection refuses a write, which means it is gone and Cloud will
// subscribe again on the next one if it still wants the stream.
func (s *service) sampleMetrics(ctx context.Context, c *channel.Conn) {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()

	// The first CPU reading is a baseline and reports zero; sampling once
	// now means the first sample sent is already a real rate.
	s.sampler.Host()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sample := protocol.Metrics{
			Host:      s.sampler.Host(),
			Sandboxes: metrics.Sandboxes(ctx),
		}
		if sample.Sandboxes == nil {
			sample.Sandboxes = []protocol.SandboxMetrics{}
		}
		if err := c.SendMessage(ctx, protocol.TypeMetrics, sample); err != nil {
			if ctx.Err() == nil {
				s.log.Debug("metrics stream ended with the connection", "err", err)
				s.stopMetrics()
			}
			return
		}
	}
}
