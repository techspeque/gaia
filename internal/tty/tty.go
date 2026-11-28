// Package tty carries a session's terminal over a WebSocket, by the
// protocol of core.SessionTTYPath: Serve on the server side, Dial for a
// server that relays another (the host to a node).
//
// It imports only internal/core and the WebSocket library. The CLI speaks
// the same protocol with that library directly, as it may import no other
// package of this module.
package tty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"

	"github.com/techspeque/gaia/internal/core"
)

// MaxMessage bounds one WebSocket message either side reads.
const MaxMessage = 1 << 20

// chunk is the most output one binary frame carries.
const chunk = 32 << 10

// Terminal is one attachment to a session's terminal.
type Terminal interface {
	// Mode is the session's mode: only an interactive session takes input.
	Mode() core.SessionMode
	// Offset is the scrollback offset of the first output byte Read
	// returns.
	Offset() int64
	// Read returns the session's output, live. It returns io.EOF once the
	// session has ended and every byte was read.
	Read(p []byte) (int, error)
	// Input sends p to the agent's terminal.
	Input(ctx context.Context, p []byte) error
	// Resize sets the agent's terminal size.
	Resize(ctx context.Context, rows, cols uint16) error
	// Close ends the attachment; the session runs on.
	Close() error
}

// headlessNotice is the notice for input sent to a headless session.
const headlessNotice = "this session is headless: it has no terminal, and input is not accepted"

// Serve upgrades the request to a WebSocket and carries t over it until the
// session ends (close status 1000), the client goes away, or the request's
// context ends. It closes t. The caller has checked the request: its
// origin (core.CheckOrigin) and its right to the session. The WebSocket
// library refuses a cross-origin upgrade on its own as well.
func Serve(w http.ResponseWriter, r *http.Request, t Terminal, log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	defer t.Close()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		log.Warn("terminal upgrade refused", "path", r.URL.Path, "err", err)
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(MaxMessage)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	if err := writeMessage(ctx, c, core.TTYMessage{Type: core.TTYHello, Mode: t.Mode(), Offset: t.Offset()}); err != nil {
		return
	}
	outDone := make(chan error, 1)
	go func() { outDone <- pumpOutput(ctx, c, t) }()
	inDone := make(chan error, 1)
	go func() { inDone <- readInput(ctx, c, t, log) }()

	select {
	case err := <-outDone:
		if err == nil {
			_ = c.Close(websocket.StatusNormalClosure, "the session has ended")
			return
		}
		if ctx.Err() == nil {
			log.Warn("a session's terminal output ended early", "path", r.URL.Path, "err", err)
		}
		_ = c.Close(websocket.StatusInternalError, closeReason("the session's output was cut: "+err.Error()))
	case <-inDone:
		// The client went away, or broke the protocol.
	case <-ctx.Done():
	}
}

// pumpOutput copies t's output to c as binary frames: nil once t reached
// io.EOF.
func pumpOutput(ctx context.Context, c *websocket.Conn, t Terminal) error {
	buf := make([]byte, chunk)
	for {
		n, err := t.Read(buf)
		if n > 0 {
			if werr := c.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readInput passes the client's input and resizes to t until the client's
// side ends, and returns why.
func readInput(ctx context.Context, c *websocket.Conn, t Terminal, log *slog.Logger) error {
	interactive := t.Mode() == core.SessionInteractive
	warned := false
	notice := func(msg string) {
		_ = writeMessage(ctx, c, core.TTYMessage{Type: core.TTYNotice, Message: msg})
	}
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return err
		}
		switch typ {
		case websocket.MessageBinary:
			if !interactive {
				if !warned {
					warned = true
					notice(headlessNotice)
				}
				continue
			}
			if len(data) == 0 {
				continue
			}
			if err := t.Input(ctx, data); err != nil && ctx.Err() == nil {
				log.Debug("terminal input not delivered", "err", err)
				notice("input not delivered: " + errMessage(err))
			}
		case websocket.MessageText:
			var m core.TTYMessage
			if err := json.Unmarshal(data, &m); err != nil {
				notice("a text message that is not a terminal message was ignored")
				continue
			}
			if m.Type != core.TTYResize || !interactive {
				continue
			}
			if err := t.Resize(ctx, m.Rows, m.Cols); err != nil && ctx.Err() == nil {
				log.Debug("terminal resize not delivered", "err", err)
			}
		}
	}
}

// errMessage is err's message for a person: a *core.Error's own message.
func errMessage(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce.Message
	}
	return err.Error()
}

func writeMessage(ctx context.Context, c *websocket.Conn, m core.TTYMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// closeReason cuts s to fit a close frame (123 bytes of reason).
func closeReason(s string) string {
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// ErrDropped is returned by a dialled terminal's Read when the connection
// ended before the session did.
var ErrDropped = errors.New("the terminal connection ended before the session did")

// client is a Terminal served by Serve at the other end of a WebSocket.
type client struct {
	c      *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	hello  core.TTYMessage
	cur    io.Reader // the binary message being read, or nil
}

// Dial attaches to the terminal served at url (http or https) over hc,
// whose Timeout must be zero. A refusal before the upgrade is the server's
// *core.Error when it sent one.
func Dial(ctx context.Context, hc *http.Client, url string) (Terminal, error) {
	c, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient:      hc,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if res != nil && res.StatusCode != http.StatusSwitchingProtocols {
			if ce := ResponseError(res); ce != nil {
				return nil, ce
			}
			return nil, fmt.Errorf("the terminal at %s answered HTTP %d: %w", url, res.StatusCode, err)
		}
		return nil, err
	}
	c.SetReadLimit(MaxMessage)
	hello, err := readHello(ctx, c)
	if err != nil {
		_ = c.CloseNow()
		return nil, err
	}
	cctx, cancel := context.WithCancel(context.Background())
	return &client{c: c, ctx: cctx, cancel: cancel, hello: hello}, nil
}

// ResponseError decodes the JSON error of a refused upgrade, or returns
// nil.
func ResponseError(res *http.Response) *core.Error {
	if res.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<10))
	var er core.ErrorResponse
	if json.Unmarshal(b, &er) == nil && er.Error != nil && er.Error.Code != "" {
		return er.Error
	}
	return nil
}

// readHello reads the server's first message, which must be its hello.
func readHello(ctx context.Context, c *websocket.Conn) (core.TTYMessage, error) {
	typ, data, err := c.Read(ctx)
	if err != nil {
		return core.TTYMessage{}, fmt.Errorf("reading the terminal's hello: %w", err)
	}
	var m core.TTYMessage
	if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type != core.TTYHello || !m.Mode.Valid() {
		return core.TTYMessage{}, errors.New("the terminal did not start with a hello")
	}
	return m, nil
}

func (t *client) Mode() core.SessionMode { return t.hello.Mode }
func (t *client) Offset() int64          { return t.hello.Offset }

func (t *client) Read(p []byte) (int, error) {
	for {
		if t.cur != nil {
			n, err := t.cur.Read(p)
			if errors.Is(err, io.EOF) {
				t.cur = nil
				err = nil
			}
			if n > 0 || err != nil {
				return n, err
			}
			continue
		}
		typ, r, err := t.c.Reader(t.ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return 0, io.EOF
			}
			return 0, fmt.Errorf("%w: %w", ErrDropped, err)
		}
		if typ == websocket.MessageBinary {
			t.cur = r
			continue
		}
		// Text: a notice from the far end, meant for the person attached
		// there; the relaying server has its own.
		if _, err := io.Copy(io.Discard, r); err != nil {
			return 0, fmt.Errorf("%w: %w", ErrDropped, err)
		}
	}
}

func (t *client) Input(ctx context.Context, p []byte) error {
	return t.c.Write(ctx, websocket.MessageBinary, p)
}

func (t *client) Resize(ctx context.Context, rows, cols uint16) error {
	return writeMessage(ctx, t.c, core.TTYMessage{Type: core.TTYResize, Rows: rows, Cols: cols})
}

func (t *client) Close() error {
	t.cancel()
	return t.c.CloseNow()
}
