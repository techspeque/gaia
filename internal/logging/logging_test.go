package logging

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/techspeque/gaia/internal/core"
)

const secret = "sk-test-7f3a9c1e5b"

type stringer struct{}

func (stringer) String() string { return "token=" + secret }

type carrier struct {
	Name  string
	Token string
}

func lines(t *testing.T, b *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(b)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q is not JSON: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestSecretNeverAppears(t *testing.T) {
	var buf bytes.Buffer
	r := &Redactor{}
	log := New(&buf, r)
	// Bound before the secret is registered: still redacted.
	bound := log.With("early", secret).WithGroup("g")
	r.Register(core.NewSecret(secret))

	log.Info("message carries " + secret)
	log.Info("attr", "k", secret)
	log.Info("group", slog.Group("outer", slog.Group("inner", "k", "x"+secret+"y")))
	log.Info("error", "err", errors.New("upstream said "+secret))
	log.Info("stringer", "v", stringer{})
	log.Info("struct", "v", carrier{Name: "n", Token: secret})
	log.Info("sprintf", "v", fmt.Sprintf("%v", []string{secret}))
	log.With("w", secret).Info("with attrs")
	log.WithGroup("grp").Info("with group", "k", secret)
	bound.Info("bound", "k", secret)
	log.Info("secret value", "s", core.NewSecret(secret))

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("secret in log output:\n%s", out)
	}
	ls := lines(t, &buf)
	if len(ls) != 11 {
		t.Fatalf("got %d lines, want 11", len(ls))
	}
	if got := ls[0]["msg"]; got != "message carries "+core.Redacted {
		t.Errorf("msg = %q", got)
	}
	if got := ls[1]["k"]; got != core.Redacted {
		t.Errorf("attr = %q", got)
	}
}

func TestEscapedSecretRedacted(t *testing.T) {
	for _, v := range []string{`a"b\c`, "tab\there", "<html>&amp;", "line\nbreak"} {
		var buf bytes.Buffer
		r := &Redactor{}
		r.Register(core.NewSecret(v))
		New(&buf, r).Info("m", "k", v, "e", errors.New(v))
		ls := lines(t, &buf)
		if len(ls) != 1 {
			t.Fatalf("%q: got %d lines", v, len(ls))
		}
		for _, k := range []string{"k", "e"} {
			if ls[0][k] != core.Redacted {
				t.Errorf("%q: %s = %q, want redacted; raw %s", v, k, ls[0][k], buf.String())
			}
		}
	}
}

// TestNumericSecretKeepsJSONValid covers a secret that also appears as a
// bare JSON token: the record must stay valid JSON and the value redacted.
func TestNumericSecretKeepsJSONValid(t *testing.T) {
	var buf bytes.Buffer
	r := &Redactor{}
	log := New(&buf, r)
	bound := log.With("early", 123456)
	r.Register(core.NewSecret("123456"))
	bound.Info("m", "count", 123456, "big", 91234567, "s", "x123456y", "ok", true)
	line := bytes.TrimSpace(buf.Bytes())
	if !json.Valid(line) {
		t.Fatalf("record is not valid JSON: %s", line)
	}
	if bytes.Contains(line, []byte("123456")) {
		t.Fatalf("secret in log output: %s", line)
	}
	ls := lines(t, &buf)
	for k, want := range map[string]any{
		"early": core.Redacted, "count": core.Redacted, "big": core.Redacted,
		"s": "x" + core.Redacted + "y", "ok": true,
	} {
		if ls[0][k] != want {
			t.Errorf("%s = %v, want %v", k, ls[0][k], want)
		}
	}
}

func TestOverlappingValuesLongestFirst(t *testing.T) {
	r := &Redactor{}
	r.Register(core.NewSecret("abc"))
	r.Register(core.NewSecret("abcdef"))
	if got := r.Redact("x abcdef y abc"); got != "x "+core.Redacted+" y "+core.Redacted {
		t.Errorf("got %q", got)
	}
}

func TestUnregisteredAndEmpty(t *testing.T) {
	var buf bytes.Buffer
	r := &Redactor{}
	r.Register(core.NewSecret(""))
	New(&buf, r).Info("plain", "k", "visible")
	ls := lines(t, &buf)
	if len(ls) != 1 || ls[0]["k"] != "visible" || ls[0]["msg"] != "plain" || ls[0]["level"] != "INFO" {
		t.Errorf("got %v", ls)
	}
	New(&buf, nil).Info("nil redactor")
	if !strings.Contains(buf.String(), "nil redactor") {
		t.Error("nil redactor dropped output")
	}
}

func TestConcurrentRegisterAndLog(t *testing.T) {
	var buf safeBuffer
	r := &Redactor{}
	log := New(&buf, r)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); r.Register(core.NewSecret(fmt.Sprintf("%s-%d", secret, i))) }()
		go func() { defer wg.Done(); log.Info("m", "i", i) }()
	}
	wg.Wait()
	for i := range 8 {
		log.Info("after", "v", fmt.Sprintf("%s-%d", secret, i))
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("secret in output:\n%s", buf.String())
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
