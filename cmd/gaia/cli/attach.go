package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/term"

	"github.com/techspeque/gaia/internal/core"
)

// DetachKey ends an attachment and leaves the session running: Ctrl-].
const DetachKey = 0x1d

// maxTTYMessage bounds one WebSocket message the CLI reads.
const maxTTYMessage = 1 << 20

// endWait bounds how long attach waits, after a session's output ended,
// for the host to record how it ended.
const endWait = 5 * time.Second

// Terminal is the caller's terminal, as attach drives it.
type Terminal interface {
	// MakeRaw puts the terminal in raw mode and returns the function that
	// restores the mode it had.
	MakeRaw() (restore func() error, err error)
	// Size returns the terminal's size.
	Size() (rows, cols uint16, err error)
	// Resized delivers a value each time the terminal is resized, until
	// stop is called. A nil channel never delivers.
	Resized() (ch <-chan struct{}, stop func())
}

// fdTerminal is the terminal on a file descriptor.
type fdTerminal struct{ fd int }

// StdinTerminal returns the terminal on standard input, or nil when
// standard input is not a terminal.
func StdinTerminal() Terminal {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil
	}
	return fdTerminal{fd: fd}
}

func (t fdTerminal) MakeRaw() (func() error, error) {
	st, err := term.MakeRaw(t.fd)
	if err != nil {
		return nil, err
	}
	return func() error { return term.Restore(t.fd, st) }, nil
}

func (t fdTerminal) Size() (rows, cols uint16, err error) {
	w, h, err := term.GetSize(t.fd)
	if err != nil {
		return 0, 0, err
	}
	return uint16(min(h, 0xffff)), uint16(min(w, 0xffff)), nil
}

// notifyInterrupt delivers the signals that end an attachment (and leave
// the session running). Tests replace it.
var notifyInterrupt = func() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	return ch, func() { signal.Stop(ch) }
}

// attachment is a terminal WebSocket to a session, after its hello.
type attachment struct {
	conn  *websocket.Conn
	hello core.TTYMessage
}

// Attach connects, then attaches to the terminal of the session with id.
// A refusal before the upgrade is the daemon's *core.Error.
func (c *Client) Attach(ctx context.Context, id core.SessionID) (*attachment, error) {
	if _, err := c.Connect(ctx); err != nil {
		return nil, err
	}
	path := core.SessionTTYPath(id)
	conn, res, err := websocket.Dial(ctx, "ws://gaia"+path, &websocket.DialOptions{
		HTTPClient:      c.httpClientTimeout(0),
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if res != nil && res.StatusCode != http.StatusSwitchingProtocols {
			return nil, daemonError(res, path)
		}
		return nil, err
	}
	conn.SetReadLimit(maxTTYMessage)
	typ, b, err := conn.Read(ctx)
	var hello core.TTYMessage
	if err == nil && (typ != websocket.MessageText || json.Unmarshal(b, &hello) != nil || hello.Type != core.TTYHello || !hello.Mode.Valid()) {
		err = errors.New("the daemon's terminal did not start with a hello")
	}
	if err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("attaching to session %s: %w", id, err)
	}
	return &attachment{conn: conn, hello: hello}, nil
}

// attachEnd is how an attachment ended.
type attachEnd int

const (
	endDetached attachEnd = iota // the detach key, a signal, or stdin's end in a headless session
	endSession                   // the session ended and its output was all shown
	endDropped                   // the connection ended first
)

// attachUsage is the usage text of `gaia attach`.
const attachUsage = "usage: gaia attach SESSION"

// Attach runs `gaia attach SESSION` and returns the exit code.
func Attach(args []string, env Env) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		fmt.Fprintln(env.Stderr, attachUsage)
		fmt.Fprintln(env.Stderr, "Shows the session's terminal, recent output first. Input goes to an interactive session; Ctrl-] detaches and the session runs on.")
		return 0
	}
	id, code, ok := sessionArg(args, "attach", attachUsage, env)
	if !ok {
		return code
	}
	return attach(context.Background(), id, env)
}

// attach attaches the caller's terminal to the session with id until it
// detaches, the session ends or the connection drops, and returns the exit
// code: 0 on detach; the agent's exit code once the session ended (1 when
// it has none); 95 when the connection dropped.
func attach(ctx context.Context, id core.SessionID, env Env) int {
	a, err := env.Client.Attach(ctx, id)
	if code, ok := notFound(err, id, env); ok {
		return code
	}
	if err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	end, err := a.run(ctx, id, env)
	if err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	switch end {
	case endDetached:
		fmt.Fprintf(env.Stderr, "gaia: detached from session %s; it runs on (`gaia attach %s` to return)\n", id, id)
		return 0
	case endDropped:
		fmt.Fprintf(env.Stderr, "gaia: lost the connection to session %s; it may still be running: `gaia attach %s`\n", id, id)
		return int(core.ExitSessionLost)
	}
	return sessionEnded(ctx, id, env)
}

// sessionEnded reports how the session ended, once the host knows, and
// returns the exit code for it.
func sessionEnded(ctx context.Context, id core.SessionID, env Env) int {
	deadline := time.Now().Add(endWait)
	for {
		sess, err := env.Client.GetSession(ctx, id)
		if err == nil && sess.State.Terminal() {
			if sess.ExitCode != nil {
				fmt.Fprintf(env.Stderr, "gaia: session %s %s (exit code %d)\n", id, sess.State, *sess.ExitCode)
				return *sess.ExitCode
			}
			fmt.Fprintf(env.Stderr, "gaia: session %s %s\n", id, sess.State)
			return 1
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(env.Stderr, "gaia: session %s ended; the host has not recorded its exit yet (`gaia ls` shows it)\n", id)
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// run carries the caller's terminal over a until it ends, and restores the
// terminal before it returns, whatever the reason.
func (a *attachment) run(ctx context.Context, id core.SessionID, env Env) (attachEnd, error) {
	defer a.conn.CloseNow()
	interactive := a.hello.Mode == core.SessionInteractive
	nl := "\n"
	if interactive && env.Term != nil {
		restore, err := env.Term.MakeRaw()
		if err != nil {
			return 0, fmt.Errorf("putting the terminal in raw mode: %w", err)
		}
		var once sync.Once
		defer once.Do(func() { _ = restore() })
		nl = "\r\n"
	}
	if interactive {
		fmt.Fprintf(env.Stderr, "gaia: attached to session %s; Ctrl-] detaches%s", id, nl)
	} else {
		fmt.Fprintf(env.Stderr, "gaia: watching headless session %s: output only, input is not accepted; Ctrl-C detaches%s", id, nl)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigs, stopSigs := notifyInterrupt()
	defer stopSigs()

	ended := make(chan attachEnd, 3)
	go func() { ended <- a.output(ctx, env, nl) }()
	if interactive && env.Term != nil {
		// Before any input: the agent has the caller's size by the time it
		// reads what the caller typed.
		a.sendSize(ctx, env.Term)
	}
	if env.Stdin != nil {
		go func() {
			if a.input(ctx, env.Stdin, interactive) {
				ended <- endDetached
			}
		}()
	}
	if interactive && env.Term != nil {
		resized, stopResized := env.Term.Resized()
		defer stopResized()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-resized:
					a.sendSize(ctx, env.Term)
				}
			}
		}()
	}
	select {
	case end := <-ended:
		if end == endDetached {
			_ = a.conn.Close(websocket.StatusNormalClosure, "detached")
		}
		return end, nil
	case <-sigs:
		_ = a.conn.Close(websocket.StatusNormalClosure, "detached")
		return endDetached, nil
	}
}

// output writes the session's output to stdout and its notices to stderr
// until the connection ends, and says how it ended.
func (a *attachment) output(ctx context.Context, env Env, nl string) attachEnd {
	for {
		typ, b, err := a.conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return endSession
			}
			if ctx.Err() != nil {
				return endDetached
			}
			return endDropped
		}
		if typ == websocket.MessageBinary {
			if _, err := env.Stdout.Write(b); err != nil {
				// Nowhere to show the session: leave it running.
				return endDetached
			}
			continue
		}
		var m core.TTYMessage
		if json.Unmarshal(b, &m) == nil && m.Type == core.TTYNotice && m.Message != "" {
			fmt.Fprintf(env.Stderr, "%sgaia: %s%s", nl, m.Message, nl)
		}
	}
}

// input sends what the caller types to an interactive session, up to the
// detach key, and reports whether the caller detached. A headless session
// is sent nothing; the detach key still detaches it.
func (a *attachment) input(ctx context.Context, in io.Reader, interactive bool) bool {
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		p := buf[:n]
		i := bytes.IndexByte(p, DetachKey)
		if i >= 0 {
			p = p[:i]
		}
		if interactive && len(p) > 0 {
			if werr := a.conn.Write(ctx, websocket.MessageBinary, p); werr != nil {
				return false
			}
		}
		if i >= 0 {
			return true
		}
		if err != nil {
			// The caller's input ended; the output goes on.
			return false
		}
	}
}

// sendSize sends the caller's terminal size.
func (a *attachment) sendSize(ctx context.Context, t Terminal) {
	rows, cols, err := t.Size()
	if err != nil || rows == 0 || cols == 0 {
		return
	}
	b, _ := json.Marshal(core.TTYMessage{Type: core.TTYResize, Rows: rows, Cols: cols})
	_ = a.conn.Write(ctx, websocket.MessageText, b)
}
