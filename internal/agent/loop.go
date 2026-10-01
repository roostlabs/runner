package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrLoop means the agent kept doing the same thing and getting the same
// answer. It ends the task: a model that re-runs a failing command for the
// fourth time is not about to find a fifth idea, and every pass costs money.
var ErrLoop = errors.New("agent: looping without progress")

// loopWarnAt is how many identical repeats earn the model a warning, and
// loopStopAt how many end the task. The warning comes first because a model
// told that it is repeating itself usually changes course; the stop exists
// for the one that does not.
const (
	loopWarnAt = 2
	loopStopAt = 3
)

// loopWarning is appended to the repeated tool result. It names the count so
// the model can see the pattern rather than infer it.
const loopWarning = "\n\n[roost] You have run this exact call before and received this exact result. " +
	"Repeating it will not change the outcome. Try a different approach, or call submit " +
	"and explain what is blocking you."

// loopGuard notices when a tool call and its result are a repeat.
//
// Progress is defined by the pair: the same command producing a different
// output is progress (something changed in the checkout), and a different
// command producing the same output is at least a different attempt. Only
// the identical pair, seen again and again, is a loop. Writes are exempt:
// writing the same content twice is wasteful but it is how a model retries,
// not a sign that it is stuck.
type loopGuard struct {
	seen map[string]int
}

// observe records one call with its result and reports how many times that
// exact pair has now been seen.
func (g *loopGuard) observe(name string, input []byte, output string) int {
	if name == toolWriteFile || name == toolSubmit {
		return 0
	}
	if g.seen == nil {
		g.seen = map[string]int{}
	}
	key := fingerprint(name, input, output)
	g.seen[key]++
	return g.seen[key]
}

// fingerprint hashes a call and its result. Whitespace at the ends of the
// output is ignored so a trailing newline does not make two runs differ.
func fingerprint(name string, input []byte, output string) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(input)
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(output)))
	return hex.EncodeToString(h.Sum(nil))
}
