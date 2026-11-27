// Package logging builds Gaia's logger: structured JSON, one object per
// line, with every registered secret value redacted at the logger
// (OVERVIEW §8).
//
// Redaction works on the encoded output, so it covers the message, every
// attribute, groups, attributes bound before the secret was registered,
// errors and formatted values alike. slog's JSON handler writes each record
// with a single Write, so a value is never split across two writes.
package logging

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/techspeque/gaia/internal/core"
)

// New returns a JSON logger at info level writing to w, redacting every
// value registered with r. A nil r redacts nothing.
func New(w io.Writer, r *Redactor) *slog.Logger {
	if r == nil {
		r = &Redactor{}
	}
	return slog.New(slog.NewJSONHandler(&redactWriter{w: w, r: r}, nil))
}

// Redactor is a set of secret values to keep out of output. It is safe for
// concurrent use. The zero value is empty and ready.
type Redactor struct {
	mu    sync.RWMutex
	forms [][]byte // every encoded form of every value, longest first
}

// Register adds the value of s. An empty secret is ignored.
func (r *Redactor) Register(s core.Secret) {
	v := s.Reveal()
	if v == "" {
		return
	}
	add := encodedForms(v)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range add {
		if !slices.ContainsFunc(r.forms, func(g []byte) bool { return bytes.Equal(f, g) }) {
			r.forms = append(r.forms, f)
		}
	}
	slices.SortStableFunc(r.forms, func(a, b []byte) int { return cmp.Compare(len(b), len(a)) })
}

// Redact returns s with every registered value replaced by core.Redacted.
func (r *Redactor) Redact(s string) string { return string(r.redact([]byte(s))) }

func (r *Redactor) redact(p []byte) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, f := range r.forms {
		if bytes.Contains(p, f) {
			p = bytes.ReplaceAll(p, f, []byte(core.Redacted))
		}
	}
	return p
}

// encodedForms returns v as written raw and as it appears inside a JSON
// string, with and without HTML escaping, so a value with quotes,
// backslashes, control or HTML characters is still found in JSON output.
func encodedForms(v string) [][]byte {
	forms := [][]byte{[]byte(v)}
	for _, html := range []bool{true, false} {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(html)
		if err := enc.Encode(v); err != nil {
			continue
		}
		s := strings.TrimSuffix(b.String(), "\n")
		s = strings.TrimPrefix(strings.TrimSuffix(s, `"`), `"`)
		if f := []byte(s); !slices.ContainsFunc(forms, func(g []byte) bool { return bytes.Equal(f, g) }) {
			forms = append(forms, f)
		}
	}
	return forms
}

// redactWriter redacts each write before passing it on.
type redactWriter struct {
	w io.Writer
	r *Redactor
}

func (rw *redactWriter) Write(p []byte) (int, error) {
	out := rw.r.redact(p) // ReplaceAll copies; p is never modified
	if _, err := rw.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
