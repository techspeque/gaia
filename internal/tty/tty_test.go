package tty

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// fakeTerm is a Terminal whose output the test writes and whose input and
// resizes it records.
type fakeTerm struct {
	mode   core.SessionMode
	offset int64
	pr     *io.PipeReader
	pw     *io.PipeWriter

	mu      sync.Mutex
	input   bytes.Buffer
	sizes   [][2]uint16
	closed  bool
	changed chan struct{}
}

func newFakeTerm(mode core.SessionMode, offset int64) *fakeTerm {
	pr, pw := io.Pipe()
	return &fakeTerm{mode: mode, offset: offset, pr: pr, pw: pw, changed: make(chan struct{}, 100)}
}

func (f *fakeTerm) Mode() core.SessionMode     { return f.mode }
func (f *fakeTerm) Offset() int64              { return f.offset }
func (f *fakeTerm) Read(p []byte) (int, error) { return f.pr.Read(p) }
func (f *fakeTerm) Input(_ context.Context, p []byte) error {
	f.mu.Lock()
	f.input.Write(p)
	f.mu.Unlock()
	f.changed <- struct{}{}
	return nil
}
func (f *fakeTerm) Resize(_ context.Context, rows, cols uint16) error {
	f.mu.Lock()
	f.sizes = append(f.sizes, [2]uint16{rows, cols})
	f.mu.Unlock()
	f.changed <- struct{}{}
	return nil
}
func (f *fakeTerm) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	_ = f.pr.Close()
	return nil
}

// waitFor polls cond until it holds or 5 s pass.
func (f *fakeTerm) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		ok := cond()
		f.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-f.changed:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("%s: not reached", what)
		}
	}
}

func serveFake(t *testing.T, term *fakeTerm) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := core.CheckOrigin(r.Header.Get("Origin"), r.Host); err != nil {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(core.ErrorResponse{Error: err.(*core.Error)})
			return
		}
		Serve(w, r, term, nil)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readAll(t *testing.T, term Terminal) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	_, err := io.Copy(&out, term)
	return out.Bytes(), err
}

func TestServeDialInteractive(t *testing.T) {
	ft := newFakeTerm(core.SessionInteractive, 7)
	srv := serveFake(t, ft)
	ctx := context.Background()
	term, err := Dial(ctx, srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if term.Mode() != core.SessionInteractive || term.Offset() != 7 {
		t.Fatalf("hello: mode %s offset %d", term.Mode(), term.Offset())
	}
	if err := term.Input(ctx, []byte("ab")); err != nil {
		t.Fatal(err)
	}
	if err := term.Resize(ctx, 30, 100); err != nil {
		t.Fatal(err)
	}
	if err := term.Input(ctx, []byte("c\n")); err != nil {
		t.Fatal(err)
	}
	ft.waitFor(t, "input and resize", func() bool { return ft.input.String() == "abc\n" && len(ft.sizes) == 1 })
	if ft.sizes[0] != [2]uint16{30, 100} {
		t.Errorf("sizes %v", ft.sizes)
	}

	// Output in order, then a normal end: io.EOF.
	go func() {
		for _, s := range []string{"one\n", strings.Repeat("x", 3*chunk), "two\n"} {
			_, _ = ft.pw.Write([]byte(s))
		}
		_ = ft.pw.Close()
	}()
	got, err := readAll(t, term)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "one\n" + strings.Repeat("x", 3*chunk) + "two\n"; string(got) != want {
		t.Errorf("output: %d bytes, want %d", len(got), len(want))
	}
	ft.waitFor(t, "terminal closed", func() bool { return ft.closed })
}

func TestServeHeadlessDropsInput(t *testing.T) {
	ft := newFakeTerm(core.SessionHeadless, 0)
	srv := serveFake(t, ft)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	read := func() (websocket.MessageType, []byte) {
		t.Helper()
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return typ, b
	}
	if typ, b := read(); typ != websocket.MessageText || !strings.Contains(string(b), `"mode":"headless"`) {
		t.Fatalf("hello %v %s", typ, b)
	}
	for range 2 {
		if err := c.Write(ctx, websocket.MessageBinary, []byte("rm -rf /\n")); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","rows":5,"cols":5}`))
	typ, b := read()
	var m core.TTYMessage
	if typ != websocket.MessageText || json.Unmarshal(b, &m) != nil || m.Type != core.TTYNotice || !strings.Contains(m.Message, "headless") {
		t.Fatalf("want a headless notice, got %v %s", typ, b)
	}
	go func() { _, _ = ft.pw.Write([]byte("out")); _ = ft.pw.Close() }()
	// One notice only: the next message is the output.
	if typ, b := read(); typ != websocket.MessageBinary || string(b) != "out" {
		t.Fatalf("after the notice: %v %q", typ, b)
	}
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("end: %v, want a normal closure", err)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.input.Len() != 0 || len(ft.sizes) != 0 {
		t.Errorf("a headless terminal got input %q, sizes %v", ft.input.String(), ft.sizes)
	}
}

func TestServeOutputCutIsDrop(t *testing.T) {
	ft := newFakeTerm(core.SessionInteractive, 0)
	srv := serveFake(t, ft)
	term, err := Dial(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	go func() { _, _ = ft.pw.Write([]byte("part")); ft.pw.CloseWithError(errors.New("supervisor gone")) }()
	got, err := readAll(t, term)
	if string(got) != "part" || !errors.Is(err, ErrDropped) {
		t.Fatalf("got %q, %v; want part and ErrDropped", got, err)
	}
}

func TestClientGoneClosesTerminal(t *testing.T) {
	ft := newFakeTerm(core.SessionInteractive, 0)
	srv := serveFake(t, ft)
	term, err := Dial(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = term.Close()
	ft.waitFor(t, "terminal closed after the client left", func() bool { return ft.closed })
}

func TestDialRefusal(t *testing.T) {
	ft := newFakeTerm(core.SessionInteractive, 0)
	srv := serveFake(t, ft)
	// A foreign origin: the server's JSON error comes back as *core.Error.
	c, res, err := websocket.Dial(context.Background(), srv.URL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"https://evil.example"}},
	})
	if err == nil {
		c.CloseNow()
		t.Fatal("a foreign origin was admitted")
	}
	if res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: %v %v", res, err)
	}
	ce := ResponseError(res)
	if ce == nil || ce.Code != core.CodeForbidden {
		t.Fatalf("ResponseError = %v", ce)
	}

	// Dial decodes the same for an error answer.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(core.ErrorResponse{Error: core.NewError(core.CodeNotFound, "no such session")})
	}))
	defer errSrv.Close()
	_, err = Dial(context.Background(), errSrv.Client(), errSrv.URL)
	if !errors.Is(err, core.NewError(core.CodeNotFound, "")) {
		t.Fatalf("Dial of a refusing server: %v, want not_found", err)
	}
}

func TestLibraryRefusesCrossOrigin(t *testing.T) {
	// The second guard: Serve alone, with no CheckOrigin in front.
	ft := newFakeTerm(core.SessionInteractive, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { Serve(w, r, ft, nil) }))
	defer srv.Close()
	c, res, err := websocket.Dial(context.Background(), srv.URL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"https://evil.example"}},
	})
	if err == nil {
		c.CloseNow()
		t.Fatal("a foreign origin was admitted")
	}
	if res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("got %v, want 403", res)
	}
}
