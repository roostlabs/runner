package redact

import (
	"bytes"
	"strings"
	"testing"
)

func TestStringMasksKnownValues(t *testing.T) {
	f := New("ghp_secret", "sk_key")
	got := f.String("cloning with ghp_secret and calling the api with sk_key")
	if strings.Contains(got, "ghp_secret") || strings.Contains(got, "sk_key") {
		t.Fatalf("value survived: %s", got)
	}
	if want := "cloning with " + Mask + " and calling the api with " + Mask; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestEmptyFilterIsPassthrough(t *testing.T) {
	const in = "nothing to hide"
	if got := New().String(in); got != in {
		t.Errorf("String() = %q, want it unchanged", got)
	}
	// A nil Filter is what a Runner with no credentials ends up holding.
	var f *Filter
	if got := f.String(in); got != in {
		t.Errorf("nil filter String() = %q, want it unchanged", got)
	}
}

// Masking the empty string would replace every position in the input.
func TestEmptyValuesIgnored(t *testing.T) {
	const in = "plain text"
	if got := New("", "").String(in); got != in {
		t.Errorf("String() = %q, want it unchanged", got)
	}
}

// A short credential contained in a longer one must not eat the longer one
// first and leave the rest of it exposed.
func TestLongestValueMaskedFirst(t *testing.T) {
	f := New("secret", "secret_extended_value")
	got := f.String("token=secret_extended_value")
	if strings.Contains(got, "extended") {
		t.Errorf("the longer value was left partly exposed: %s", got)
	}
	if want := "token=" + Mask; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestBytesDoesNotModifyInput(t *testing.T) {
	in := []byte("token=ghp_secret")
	original := string(in)
	New("ghp_secret").Bytes(in)
	if string(in) != original {
		t.Errorf("input was modified: %q, want %q", in, original)
	}
}

func TestWriterMasksWholeWrites(t *testing.T) {
	var out bytes.Buffer
	w := New("ghp_secret").Writer(&out)

	if _, err := w.Write([]byte("using ghp_secret now\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := out.String(); strings.Contains(got, "ghp_secret") {
		t.Errorf("value survived: %s", got)
	}
}

// The real failure mode: output arrives in arbitrary chunks and a credential
// straddles two of them, so a naive per-write filter never sees it whole.
func TestWriterMasksValueSplitAcrossWrites(t *testing.T) {
	const secret = "ghp_supersecrettoken"

	for _, split := range []int{1, 5, len(secret) / 2, len(secret) - 1} {
		var out bytes.Buffer
		w := New(secret).Writer(&out)

		payload := "prefix " + secret + " suffix"
		at := len("prefix ") + split
		if _, err := w.Write([]byte(payload[:at])); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := w.Write([]byte(payload[at:])); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		got := out.String()
		if strings.Contains(got, secret) {
			t.Errorf("split at %d leaked the value: %s", split, got)
		}
		if want := "prefix " + Mask + " suffix"; got != want {
			t.Errorf("split at %d produced %q, want %q", split, got, want)
		}
	}
}

// One byte at a time is the worst case and the one a pty or an unbuffered
// process actually produces.
func TestWriterMasksByteAtATime(t *testing.T) {
	const secret = "sk_bytewise"
	var out bytes.Buffer
	w := New(secret).Writer(&out)

	for _, b := range []byte("key " + secret + " done") {
		if _, err := w.Write([]byte{b}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := out.String(); got != "key "+Mask+" done" {
		t.Errorf("got %q, want %q", got, "key "+Mask+" done")
	}
}

// Retaining a tail is an implementation detail; a caller copying into the
// writer must not see it as a short write.
func TestWriterReportsFullLength(t *testing.T) {
	w := New("ghp_secret").Writer(&bytes.Buffer{})
	p := []byte("ab")
	n, err := w.Write(p)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(p) {
		t.Errorf("Write reported %d, want %d", n, len(p))
	}
}

// Without Flush the retained tail would be silently dropped, so a value at the
// very end of a stream must still come out masked.
func TestWriterFlushEmitsTail(t *testing.T) {
	var out bytes.Buffer
	w := New("ghp_secret").Writer(&out)

	if _, err := w.Write([]byte("ends with ghp_secret")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if out.String() == "ends with "+Mask {
		t.Fatal("tail was emitted before Flush; the split protection is not working")
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := out.String(); got != "ends with "+Mask {
		t.Errorf("after Flush got %q, want %q", got, "ends with "+Mask)
	}
}

func TestWriterWithNoValuesIsPassthrough(t *testing.T) {
	var out bytes.Buffer
	w := New().Writer(&out)
	if _, err := w.Write([]byte("streamed immediately")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// With nothing to mask there is no reason to hold anything back.
	if got := out.String(); got != "streamed immediately" {
		t.Errorf("got %q, want it written straight through", got)
	}
}
