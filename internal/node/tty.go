package node

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/supervise"
	"github.com/techspeque/gaia/internal/tty"
)

// ReplayBytes is how much of a session's most recent output an attachment
// starts with, so the screen is not empty.
const ReplayBytes = 128 << 10

// AttachSession attaches to the terminal of the session with id. The
// output starts with up to ReplayBytes of what the session already wrote
// and follows it live until the session ends, as SessionLogs does; input
// and resizes go to its supervisor, for an interactive session only. ctx
// bounds the attaching; the attachment lasts until it is closed.
func (l *LocalSessions) AttachSession(ctx context.Context, id core.SessionID) (tty.Terminal, error) {
	info, err := l.Manager.Get(id)
	if err != nil {
		return nil, err
	}
	dir := l.Manager.SessionDir(id)
	off, err := replayOffset(ctx, dir)
	if err != nil {
		return nil, err
	}
	actx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(l.SessionLogs(actx, id, off, true, pw)) }()
	return &localTerm{mode: info.Mode, offset: off, out: pr, cancel: cancel, sv: supervise.NewClient(dir)}, nil
}

// replayOffset is where an attachment to the session in dir starts: the
// last ReplayBytes of its scrollback, or the first byte still kept.
func replayOffset(ctx context.Context, dir string) (int64, error) {
	var err error
	for range 3 {
		var st supervise.Status
		if st, err = supervise.NewClient(dir).Status(ctx); err == nil {
			return max(st.Start, st.Size-ReplayBytes), nil
		}
		var sb *supervise.Scrollback
		if sb, err = supervise.OpenScrollback(dir, 0); errors.Is(err, core.NewError(core.CodeInvalidArgument, "")) {
			// Offset 0 was rotated out; the end is still a valid offset.
			sb, err = openAtEnd(dir)
		}
		if err == nil {
			_ = sb.Close()
			return max(sb.Start, sb.End-ReplayBytes), nil
		}
		if !errors.Is(err, core.NewError(core.CodeConflict, "")) {
			return 0, err
		}
		// Running by its state file, yet its supervisor did not answer:
		// it is between ending and recording the end.
		if !sleep(ctx, streamRetryWait) {
			return 0, ctx.Err()
		}
	}
	return 0, err
}

// openAtEnd opens an ended session's scrollback at its end.
func openAtEnd(dir string) (*supervise.Scrollback, error) {
	st, err := supervise.ReadState(dir)
	if err != nil {
		return nil, err
	}
	return supervise.OpenScrollback(dir, st.ScrollbackEnd)
}

// localTerm is an attachment to a session on this node.
type localTerm struct {
	mode   core.SessionMode
	offset int64
	out    *io.PipeReader
	cancel context.CancelFunc
	sv     *supervise.Client
}

func (t *localTerm) Mode() core.SessionMode     { return t.mode }
func (t *localTerm) Offset() int64              { return t.offset }
func (t *localTerm) Read(p []byte) (int, error) { return t.out.Read(p) }

// inputTimeout bounds one input or resize call to the supervisor.
const inputTimeout = 10 * time.Second

func (t *localTerm) Input(ctx context.Context, p []byte) error {
	if t.mode != core.SessionInteractive {
		return core.NewError(core.CodeInvalidArgument, "a headless session takes no input")
	}
	ctx, cancel := context.WithTimeout(ctx, inputTimeout)
	defer cancel()
	return t.sv.Input(ctx, p)
}

func (t *localTerm) Resize(ctx context.Context, rows, cols uint16) error {
	if t.mode != core.SessionInteractive {
		return core.NewError(core.CodeInvalidArgument, "a headless session has no terminal to resize")
	}
	ctx, cancel := context.WithTimeout(ctx, inputTimeout)
	defer cancel()
	return t.sv.Resize(ctx, rows, cols)
}

func (t *localTerm) Close() error {
	t.cancel()
	return t.out.Close()
}

// AttachSession attaches to the terminal of the session with id on the
// node, over the node API.
func (c *SessionClient) AttachSession(ctx context.Context, id core.SessionID) (tty.Terminal, error) {
	return tty.Dial(ctx, c.HTTP, c.BaseURL+core.NodeSessionTTYPath(id))
}

// serveTTY is the node API's terminal route: the origin checked, then the
// session's terminal carried over a WebSocket.
func serveTTY(s *LocalSessions, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := core.CheckOrigin(r.Header.Get("Origin"), r.Host); err != nil {
			log.Warn("terminal upgrade refused: foreign origin", "remote_addr", r.RemoteAddr, "origin", r.Header.Get("Origin"))
			writeNodeError(w, err, log)
			return
		}
		id, err := core.ParseSessionID(r.PathValue("id"))
		if err != nil {
			writeNodeError(w, err, log)
			return
		}
		term, err := s.AttachSession(r.Context(), id)
		if err != nil {
			writeNodeError(w, err, log)
			return
		}
		tty.Serve(w, r, term, log)
	}
}
