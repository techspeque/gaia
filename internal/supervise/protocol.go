package supervise

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// Request operations.
const (
	OpStatus = "status"
	OpStream = "stream"
	OpInput  = "input"
	OpResize = "resize"
	OpStop   = "stop"
)

// Protocol limits.
const (
	maxRequestLine = 1 << 20  // one request line, input data included
	maxChunk       = 32 << 10 // one read of stream bytes
	requestTimeout = 10 * time.Second
)

// Request is one client request.
type Request struct {
	Op     string `json:"op"`
	Offset int64  `json:"offset,omitempty"` // stream
	Data   []byte `json:"data,omitempty"`   // input
	Rows   uint16 `json:"rows,omitempty"`   // resize
	Cols   uint16 `json:"cols,omitempty"`   // resize
}

// Status is a supervisor's view of its session.
type Status struct {
	State core.SupervisorState `json:"state"`
	// Size is the scrollback length in bytes.
	Size int64 `json:"size"`
	// Stopping is true once a stop has been requested.
	Stopping bool `json:"stopping"`
}

// Response is the one JSON line a supervisor writes per request, and the
// ready line `gaia supervise` writes to stdout once its agent runs.
type Response struct {
	OK     bool        `json:"ok"`
	Error  *core.Error `json:"error,omitempty"`
	Status *Status     `json:"status,omitempty"`
	Offset int64       `json:"offset,omitempty"` // stream: where the bytes start
	PID    int         `json:"pid,omitempty"`    // ready line: the agent's pid
}

// streamHeader is the line a stream starts with. Unlike Response it always
// carries offset, zero included.
type streamHeader struct {
	OK     bool  `json:"ok"`
	Offset int64 `json:"offset"`
}

// Client talks to the supervisor of one session directory. Each call opens
// its own connection.
type Client struct {
	path string
}

// NewClient returns a client for the supervisor in session directory dir.
func NewClient(dir string) *Client { return &Client{path: filepath.Join(dir, SocketFile)} }

// call sends req and reads the response line. On success the connection and
// its reader are returned for the caller to finish with.
func (c *Client) call(ctx context.Context, req Request) (net.Conn, *bufio.Reader, Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return nil, nil, Response{}, &core.Error{
			Code:    core.CodeUnavailable,
			Message: fmt.Sprintf("supervisor at %s is not reachable: %v", c.path, err),
			Details: map[string]string{"path": c.path},
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	fail := func(err error) (net.Conn, *bufio.Reader, Response, error) {
		stop()
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, nil, Response{}, ctx.Err()
		}
		return nil, nil, Response{}, err
	}
	b, err := json.Marshal(req)
	if err != nil {
		return fail(err)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return fail(err)
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return fail(fmt.Errorf("reading supervisor response: %w", err))
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fail(fmt.Errorf("decoding supervisor response: %w", err))
	}
	if !resp.OK {
		if resp.Error == nil {
			resp.Error = core.NewError(core.CodeInternal, "supervisor refused the request without a reason")
		}
		return fail(resp.Error)
	}
	stop()
	// Keep closing the connection when ctx ends, for streams.
	context.AfterFunc(ctx, func() { _ = conn.Close() })
	return conn, r, resp, nil
}

func (c *Client) simple(ctx context.Context, req Request) (Response, error) {
	conn, _, resp, err := c.call(ctx, req)
	if err != nil {
		return Response{}, err
	}
	_ = conn.Close()
	return resp, nil
}

// Status returns the supervisor's status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	resp, err := c.simple(ctx, Request{Op: OpStatus})
	if err != nil {
		return Status{}, err
	}
	if resp.Status == nil {
		return Status{}, core.NewError(core.CodeInternal, "status response without a status")
	}
	return *resp.Status, nil
}

// Stop stops the session and returns its status once the agent has ended.
func (c *Client) Stop(ctx context.Context) (Status, error) {
	resp, err := c.simple(ctx, Request{Op: OpStop})
	if err != nil {
		return Status{}, err
	}
	if resp.Status == nil {
		return Status{}, core.NewError(core.CodeInternal, "stop response without a status")
	}
	return *resp.Status, nil
}

// Input sends data to the agent's terminal. Interactive sessions only.
func (c *Client) Input(ctx context.Context, data []byte) error {
	_, err := c.simple(ctx, Request{Op: OpInput, Data: data})
	return err
}

// Resize sets the agent's terminal size. Interactive sessions only.
func (c *Client) Resize(ctx context.Context, rows, cols uint16) error {
	_, err := c.simple(ctx, Request{Op: OpResize, Rows: rows, Cols: cols})
	return err
}

// ErrStreamCut is returned by Stream when the connection ends before the
// session's output did.
var ErrStreamCut = errors.New("supervisor stream ended before the session's output did")

// Stream copies the scrollback from byte offset to w, live, until the
// session has ended and every byte has been sent (nil error), the
// connection is cut (ErrStreamCut), ctx ends, or w fails. n counts the bytes
// written to w, so offset+n is where to resume.
//
// The stream carries raw bytes and the supervisor closes it at the end, so
// on EOF the session directory tells a complete stream from a cut one: the
// state file records the end, and the scrollback ends at offset+n.
func (c *Client) Stream(ctx context.Context, offset int64, w io.Writer) (n int64, err error) {
	conn, r, _, err := c.call(ctx, Request{Op: OpStream, Offset: offset})
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	buf := make([]byte, maxChunk)
	for {
		m, rerr := r.Read(buf)
		if m > 0 {
			k, werr := w.Write(buf[:m])
			n += int64(k)
			if werr == nil && k < m {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				return n, werr
			}
		}
		if errors.Is(rerr, io.EOF) {
			return n, c.streamEnd(ctx, offset+n)
		}
		if rerr != nil {
			return n, streamErr(ctx, rerr)
		}
	}
}

// streamEnd reports whether a stream that reached EOF at byte end got the
// session's whole output.
func (c *Client) streamEnd(ctx context.Context, end int64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	dir := filepath.Dir(c.path)
	st, err := ReadState(dir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStreamCut, err)
	}
	if !st.Ended() {
		return fmt.Errorf("%w: the connection closed while the session runs", ErrStreamCut)
	}
	fi, err := os.Stat(filepath.Join(dir, ScrollbackFile))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStreamCut, err)
	}
	if fi.Size() != end {
		return fmt.Errorf("%w: got the scrollback to byte %d of %d", ErrStreamCut, end, fi.Size())
	}
	return nil
}

func streamErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w: %w", ErrStreamCut, err)
}
