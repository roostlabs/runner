// Package redact masks credential values in text leaving the VPS.
//
// Command output is the likeliest way for a token to escape: a build that echoes
// its environment, a curl printing its own headers, a stack trace carrying a URL
// with credentials in it. The Runner knows exactly which values it injected into
// the sandbox, so it can mask those before anything is streamed to Cloud.
//
// This is a backstop, not a guarantee. It masks values it was told about, in the
// form they were given; a credential the sandbox transforms — base64, split
// across lines, url-encoded — passes through unrecognized.
package redact

import (
	"bytes"
	"io"
	"sort"
)

// Mask replaces a credential value wherever one is found.
const Mask = "[REDACTED]"

// Filter masks a fixed set of values.
//
// The zero Filter is valid and masks nothing, which is what a Runner with no
// credentials configured needs.
type Filter struct {
	values [][]byte // longest first
	hold   int      // bytes a streaming writer must retain to catch a split value
}

// New builds a Filter for the given values. Empty values are ignored: masking
// the empty string would replace everything.
func New(values ...string) *Filter {
	f := &Filter{}
	for _, v := range values {
		if v != "" {
			f.values = append(f.values, []byte(v))
		}
	}
	// Longest first, so a credential that contains another is masked whole
	// rather than being partly eaten by the shorter one.
	sort.Slice(f.values, func(i, j int) bool {
		return len(f.values[i]) > len(f.values[j])
	})
	for _, v := range f.values {
		if len(v)-1 > f.hold {
			f.hold = len(v) - 1
		}
	}
	return f
}

// Bytes returns b with every known value masked. It does not modify b.
func (f *Filter) Bytes(b []byte) []byte {
	if f == nil || len(f.values) == 0 {
		return b
	}
	out := b
	copied := false
	for _, v := range f.values {
		if !bytes.Contains(out, v) {
			continue
		}
		if !copied {
			out = append([]byte(nil), out...)
			copied = true
		}
		out = bytes.ReplaceAll(out, v, []byte(Mask))
	}
	return out
}

// String returns s with every known value masked.
func (f *Filter) String(s string) string {
	if f == nil || len(f.values) == 0 {
		return s
	}
	return string(f.Bytes([]byte(s)))
}

// Writer wraps w so everything written through it is masked.
//
// Streaming output arrives in arbitrary chunks, and a credential can straddle
// two of them. The returned writer therefore holds back the last few bytes —
// enough that no value can be split across the boundary — and releases them on
// Flush. Callers must Flush when the stream ends or the tail is lost.
func (f *Filter) Writer(w io.Writer) *StreamWriter {
	hold := 0
	if f != nil {
		hold = f.hold
	}
	return &StreamWriter{filter: f, out: w, hold: hold}
}

// StreamWriter masks a stream of writes.
type StreamWriter struct {
	filter *Filter
	out    io.Writer
	hold   int
	buf    []byte
}

// Write masks and forwards everything except a short tail held back in case a
// value is split across this write and the next.
//
// It reports len(p) written whenever the data was accepted, because a caller
// copying into it must not treat retention as a short write.
func (s *StreamWriter) Write(p []byte) (int, error) {
	if s.hold == 0 {
		n, err := s.out.Write(s.filter.Bytes(p))
		if err != nil {
			return n, err
		}
		return len(p), nil
	}

	s.buf = append(s.buf, p...)
	if len(s.buf) <= s.hold {
		return len(p), nil
	}

	// Mask the whole buffer before deciding what to release. Masking only the
	// part being released would cut a value in half at the boundary — exactly
	// the case the tail exists to prevent.
	masked := s.filter.Bytes(s.buf)
	if len(masked) <= s.hold {
		s.buf = append(s.buf[:0], masked...)
		return len(p), nil
	}

	cut := len(masked) - s.hold
	if _, err := s.out.Write(masked[:cut]); err != nil {
		return 0, err
	}
	s.buf = append(s.buf[:0], masked[cut:]...)
	return len(p), nil
}

// Flush writes the retained tail. Call it once the stream is finished.
func (s *StreamWriter) Flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	_, err := s.out.Write(s.filter.Bytes(s.buf))
	s.buf = s.buf[:0]
	return err
}
