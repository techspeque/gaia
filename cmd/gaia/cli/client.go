// Package cli is the gaia command-line client: the commands that talk to
// the daemon on the same machine over the user-only local socket.
//
// The CLI never joins the tailnet and never starts tsnet (ADR-0004 rule 2).
// This package imports only internal/core, internal/sock and the standard
// library; a test on the import graph keeps it that way, so no CLI command
// can reach the daemon roles' networking.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/sock"
)

// DefaultTimeout bounds one client request, connect included.
const DefaultTimeout = 10 * time.Second

// maxBody bounds how much of a response the client reads.
const maxBody = 16 << 20

// Client talks to the local daemon. The zero value uses the default runtime
// directory and this binary's API version.
type Client struct {
	// Dir is the runtime directory holding the socket. Default: sock.Dir().
	Dir string
	// APIVersion is the API version this CLI speaks. Default: core.APIVersion.
	APIVersion int
	// Version is this binary's Gaia version, used in messages.
	Version string
	// Timeout bounds each request. Default: DefaultTimeout.
	Timeout time.Duration
}

func (c *Client) dir() string {
	if c.Dir == "" {
		return sock.Dir()
	}
	return c.Dir
}

func (c *Client) apiVersion() int {
	if c.APIVersion == 0 {
		return core.APIVersion
	}
	return c.APIVersion
}

func (c *Client) httpClient() *http.Client {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	dir := c.dir()
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return sock.Dial(ctx, dir)
			},
			DisableKeepAlives: true,
		},
	}
}

// Connect asks the daemon for its version and fails with a
// core.CodeVersionMismatch error unless it speaks this CLI's API version.
// Every command calls it before its own request.
func (c *Client) Connect(ctx context.Context) (core.VersionResponse, error) {
	var v core.VersionResponse
	status, err := c.do(ctx, http.MethodGet, core.VersionPath, &v)
	if status == http.StatusNotFound {
		// A daemon that no longer serves this API at all.
		return v, c.mismatch(core.VersionResponse{})
	}
	if err != nil {
		return v, err
	}
	if v.APIVersion != c.apiVersion() {
		return v, c.mismatch(v)
	}
	return v, nil
}

// ListNodes connects, then returns the daemon's registry.
func (c *Client) ListNodes(ctx context.Context) (core.ListNodesResponse, error) {
	var l core.ListNodesResponse
	if _, err := c.Connect(ctx); err != nil {
		return l, err
	}
	if _, err := c.do(ctx, http.MethodGet, core.NodesPath, &l); err != nil {
		return l, err
	}
	if l.Nodes == nil {
		l.Nodes = []core.Node{}
	}
	return l, nil
}

// RemoveNode connects, then removes the node called name from the host's
// registry and returns it as it was. On a node, the request goes to the
// host through the node.
func (c *Client) RemoveNode(ctx context.Context, name string) (core.Node, error) {
	var r core.RemoveNodeResponse
	if err := core.ValidNodeName(name); err != nil {
		return r.Node, err
	}
	if _, err := c.Connect(ctx); err != nil {
		return r.Node, err
	}
	_, err := c.do(ctx, http.MethodDelete, core.NodePath(name), &r)
	return r.Node, err
}

// do sends a body-less request for path and decodes a 2xx body into out.
// A non-2xx answer is returned as the daemon's *core.Error with the HTTP
// status.
func (c *Client) do(ctx context.Context, method, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://gaia"+path, nil)
	if err != nil {
		return 0, err
	}
	res, err := c.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return res.StatusCode, fmt.Errorf("reading the daemon's answer to %s: %w", path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var er core.ErrorResponse
		if json.Unmarshal(body, &er) == nil && er.Error != nil && er.Error.Code != "" {
			return res.StatusCode, er.Error
		}
		return res.StatusCode, core.NewError(core.CodeInternal,
			fmt.Sprintf("the daemon answered %s with HTTP %d", path, res.StatusCode))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return res.StatusCode, core.NewError(core.CodeInternal,
			fmt.Sprintf("the daemon's answer to %s is not valid: %v", path, err))
	}
	return res.StatusCode, nil
}

// mismatch builds the version error for a daemon reporting d. A zero d is
// a daemon that does not serve this CLI's API.
func (c *Client) mismatch(d core.VersionResponse) *core.Error {
	mine := fmt.Sprintf("%s, API v%d", orUnknown(c.Version), c.apiVersion())
	var msg string
	switch {
	case d.APIVersion == 0:
		msg = fmt.Sprintf("the daemon does not serve API v%d, which this gaia (%s) speaks: install the daemon's gaia version on this machine and run the command again",
			c.apiVersion(), mine)
	case d.APIVersion > c.apiVersion():
		msg = fmt.Sprintf("this gaia (%s) is older than the daemon (%s, API v%d): install gaia %s on this machine and run the command again",
			mine, orUnknown(d.GaiaVersion), d.APIVersion, orUnknown(d.GaiaVersion))
	default:
		msg = fmt.Sprintf("the daemon (%s, API v%d) is older than this gaia (%s): restart the daemon with this binary (stop `gaia host` or `gaia node` and start it again)",
			orUnknown(d.GaiaVersion), d.APIVersion, mine)
	}
	return &core.Error{
		Code:    core.CodeVersionMismatch,
		Message: msg,
		Details: map[string]string{
			"cli_api_version":    fmt.Sprint(c.apiVersion()),
			"daemon_api_version": fmt.Sprint(d.APIVersion),
		},
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown version"
	}
	return s
}

// noDaemonMessage is the one message for a machine with no daemon.
func noDaemonMessage(dir string) string {
	return fmt.Sprintf("no Gaia daemon is running on this machine (%s): start one with `gaia host` (add --node on a single machine), or `gaia node --host NAME` to join another host",
		sock.Path(dir))
}

// Fail reports err on stderr as one line prefixed "gaia: " and returns the
// reserved exit code for it.
func (c *Client) Fail(stderr io.Writer, err error) int {
	code, msg := classify(err, c.dir())
	fmt.Fprintln(stderr, "gaia: "+msg)
	return int(code)
}

// classify maps a client error to its exit code and message.
func classify(err error, dir string) (core.ExitCode, string) {
	if errors.Is(err, sock.ErrNoDaemon) {
		return core.ExitNoDaemon, noDaemonMessage(dir)
	}
	var ce *core.Error
	if errors.As(err, &ce) {
		if ce.Code == core.CodeVersionMismatch {
			return core.ExitVersionMismatch, ce.Message
		}
		return core.ExitInternal, ce.Error()
	}
	return core.ExitInternal, err.Error()
}
