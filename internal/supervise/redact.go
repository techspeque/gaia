package supervise

import (
	"bytes"
	"slices"

	"github.com/techspeque/gaia/internal/core"
)

// Redactor replaces every occurrence of a known secret in a byte stream with
// core.Redacted before the bytes go anywhere. It holds back only the
// longest buffered tail that could still become a secret, so a secret split
// across any number of writes is caught and no other byte is delayed.
// Matching is leftmost-longest. A Redactor is not safe for concurrent use.
type Redactor struct {
	secrets [][]byte // longest first
	held    []byte
}

// NewRedactor returns a Redactor for secrets. Empty and duplicate values
// are ignored; with no secrets it passes every byte through.
func NewRedactor(secrets []string) *Redactor {
	var ss [][]byte
	for _, s := range secrets {
		if s == "" || slices.ContainsFunc(ss, func(b []byte) bool { return string(b) == s }) {
			continue
		}
		ss = append(ss, []byte(s))
	}
	slices.SortStableFunc(ss, func(a, b []byte) int { return len(b) - len(a) })
	return &Redactor{secrets: ss}
}

// Write takes the next bytes of the stream and returns the bytes that are
// now safe to emit. p is not retained.
func (r *Redactor) Write(p []byte) []byte {
	if len(r.secrets) == 0 {
		return slices.Clone(p)
	}
	return r.scan(append(r.held, p...), false)
}

// Flush returns the held tail, redacted, at the end of the stream.
func (r *Redactor) Flush() []byte { return r.scan(r.held, true) }

func (r *Redactor) scan(buf []byte, final bool) []byte {
	out := make([]byte, 0, len(buf))
	i := 0
next:
	for i < len(buf) {
		rest := buf[i:]
		if !final {
			for _, s := range r.secrets {
				if len(rest) < len(s) && bytes.HasPrefix(s, rest) {
					// rest may still become s: keep it until more arrives.
					r.held = slices.Clone(rest)
					return out
				}
			}
		}
		for _, s := range r.secrets {
			if bytes.HasPrefix(rest, s) {
				out = append(out, core.Redacted...)
				i += len(s)
				continue next
			}
		}
		out = append(out, buf[i])
		i++
	}
	r.held = nil
	return out
}
