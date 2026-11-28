package host

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/tty"
)

// pipeTerm is a node terminal whose output the test writes.
type pipeTerm struct {
	mode core.SessionMode
	pr   *io.PipeReader
	pw   *io.PipeWriter

	mu     sync.Mutex
	input  bytes.Buffer
	closed bool
}

func newPipeTerm(mode core.SessionMode) *pipeTerm {
	pr, pw := io.Pipe()
	return &pipeTerm{mode: mode, pr: pr, pw: pw}
}

func (p *pipeTerm) Mode() core.SessionMode     { return p.mode }
func (p *pipeTerm) Offset() int64              { return 3 }
func (p *pipeTerm) Read(b []byte) (int, error) { return p.pr.Read(b) }
func (p *pipeTerm) Input(_ context.Context, b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.input.Write(b)
	return nil
}
func (p *pipeTerm) Resize(context.Context, uint16, uint16) error { return nil }
func (p *pipeTerm) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return p.pr.Close()
}

func (p *pipeTerm) inputString() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.input.String()
}

// ttyServer is sessionServer with one running session on node-a, served
// over real HTTP on the socket handler and on the tailnet handler (as
// peer addrB).
func ttyServer(t *testing.T) (socketURL, tailnetURL string, id core.SessionID, f *fakeNode) {
	t.Helper()
	s, h, f, _ := sessionServer(t)
	sess, err := s.StartSession(context.Background(), startReq("node-a", "agent"))
	if err != nil {
		t.Fatal(err)
	}
	sock := httptest.NewServer(s.Handler())
	t.Cleanup(sock.Close)
	tn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = addrB
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(tn.Close)
	return sock.URL, tn.URL, sess.ID, f
}

func TestTTYBridgesToNode(t *testing.T) {
	sockURL, tnURL, id, f := ttyServer(t)
	for name, base := range map[string]string{"socket": sockURL, "tailnet": tnURL} {
		pt := newPipeTerm(core.SessionInteractive)
		var gotID core.SessionID
		f.attach = func(i core.SessionID) (tty.Terminal, error) { gotID = i; return pt, nil }

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		term, err := tty.Dial(ctx, http.DefaultClient, base+core.SessionTTYPath(id))
		if err != nil {
			cancel()
			t.Fatalf("%s: dial: %v", name, err)
		}
		if gotID != id || term.Mode() != core.SessionInteractive || term.Offset() != 3 {
			t.Errorf("%s: attached %s, mode %s, offset %d", name, gotID, term.Mode(), term.Offset())
		}
		if err := term.Input(ctx, []byte("hi\n")); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for pt.inputString() != "hi\n" && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := pt.inputString(); got != "hi\n" {
			t.Errorf("%s: node got input %q", name, got)
		}
		// Ended: the output, then the end.
		go func() { _, _ = pt.pw.Write([]byte("hello")); _ = pt.pw.Close() }()
		out, err := io.ReadAll(term)
		if err != nil || string(out) != "hello" {
			t.Errorf("%s: output %q, %v", name, out, err)
		}
		_ = term.Close()
		cancel()
	}
}

func TestTTYRefusals(t *testing.T) {
	sockURL, tnURL, id, f := ttyServer(t)
	f.attach = func(core.SessionID) (tty.Terminal, error) { return newPipeTerm(core.SessionInteractive), nil }
	dial := func(url string, h http.Header) (*http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
		if err == nil {
			_ = c.CloseNow()
		}
		return res, err
	}
	evil := http.Header{"Origin": {"https://evil.example"}}
	for name, base := range map[string]string{"socket": sockURL, "tailnet": tnURL} {
		res, err := dial(base+core.SessionTTYPath(id), evil)
		if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: foreign origin: %v %v", name, res, err)
		} else if ce := tty.ResponseError(res); ce == nil || ce.Code != core.CodeForbidden {
			t.Errorf("%s: foreign origin error %v", name, ce)
		}
		// Same origin is admitted.
		host := strings.TrimPrefix(base, "http://")
		if res, err := dial(base+core.SessionTTYPath(id), http.Header{"Origin": {"http://" + host}}); err != nil {
			t.Errorf("%s: same origin: %v %v", name, res, err)
		}
	}

	unknown := core.NewSessionID([16]byte{9})
	_, err := tty.Dial(context.Background(), http.DefaultClient, sockURL+core.SessionTTYPath(unknown))
	if !errors.Is(err, core.NewError(core.CodeNotFound, "")) {
		t.Errorf("unknown session: %v", err)
	}

	f.attach = func(core.SessionID) (tty.Terminal, error) { return nil, errors.New("dial tcp: connection refused") }
	_, err = tty.Dial(context.Background(), http.DefaultClient, sockURL+core.SessionTTYPath(id))
	if !errors.Is(err, core.NewError(core.CodeUnavailable, "")) || reasonOf(err) != core.ExitNodeUnavailable.Name() {
		t.Errorf("node unreachable: %v (reason %q)", err, reasonOf(err))
	}

	f.routeErr = errors.New("no route")
	_, err = tty.Dial(context.Background(), http.DefaultClient, sockURL+core.SessionTTYPath(id))
	if !errors.Is(err, core.NewError(core.CodeUnavailable, "")) || reasonOf(err) != core.ExitNodeUnavailable.Name() {
		t.Errorf("no route: %v (reason %q)", err, reasonOf(err))
	}
}
