// Package logging builds Gaia's logger: structured JSON, one object per
// line, with every registered secret value redacted at the logger
// (OVERVIEW §8).
//
// Redaction works on the encoded output, so it covers the message, every
// attribute, groups, attributes bound before the secret was registered,
// errors and formatted values alike. It works token by token, so the
// record stays valid JSON. slog's JSON handler writes each record
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

// redactJSON redacts one encoded JSON record token by token, so the result
// stays valid JSON. A string token (key or value) is decoded, redacted and
// re-encoded only if it changed. A bare token (number, true, false, null)
// that contains a registered value becomes the string core.Redacted.
func (r *Redactor) redactJSON(p []byte) []byte {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); {
		c := p[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(p) && p[j] != '"' {
				if p[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(p) { // unterminated: not JSON, redact the bytes
				return append(out, r.redact(p[i:])...)
			}
			out = append(out, r.redactString(p[i:j+1])...)
			i = j + 1
		case isJSONDelim(c):
			out = append(out, c)
			i++
		default:
			j := i
			for j < len(p) && !isJSONDelim(p[j]) && p[j] != '"' {
				j++
			}
			tok := p[i:j]
			if red := r.redact(tok); !bytes.Equal(red, tok) {
				q, _ := json.Marshal(core.Redacted)
				out = append(out, q...)
			} else {
				out = append(out, tok...)
			}
			i = j
		}
	}
	return out
}

// redactString redacts one quoted JSON string token.
func (r *Redactor) redactString(tok []byte) []byte {
	var s string
	if err := json.Unmarshal(tok, &s); err != nil {
		return r.redact(tok)
	}
	red := r.Redact(s)
	if red == s {
		return tok
	}
	q, err := json.Marshal(red)
	if err != nil {
		return r.redact(tok)
	}
	return q
}

func isJSONDelim(c byte) bool {
	switch c {
	case '{', '}', '[', ']', ':', ',', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// redactWriter redacts each write before passing it on.
type redactWriter struct {
	w io.Writer
	r *Redactor
}

func (rw *redactWriter) Write(p []byte) (int, error) {
	out := rw.r.redactJSON(p) // builds a new slice; p is never modified
	if _, err := rw.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
