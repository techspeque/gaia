package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// Defaults for RunConfig.
const (
	DefaultMinBackoff = 500 * time.Millisecond
	DefaultMaxBackoff = 30 * time.Second
	// DefaultHeartbeatInterval is used when the host names none.
	DefaultHeartbeatInterval = 10 * time.Second
	// DialTimeout bounds connecting to the host, TLS handshake included.
	DialTimeout = 5 * time.Second
	// DefaultConfirmAfter is how soon after a registration the first
	// heartbeat goes out, confirming the path to the host before a full
	// interval has passed.
	DefaultConfirmAfter = time.Second
	// RecoveryWait bounds how long a forwarded request waits for the host's
	// response headers while the node's Link is not ready. In steady state
	// there is no such bound.
	RecoveryWait = 3 * time.Second
)

// maxResponse bounds how much of a host response the node reads.
const maxResponse = 1 << 20

// Host is the host's registry as a node on the tailnet sees it. *Client is
// one.
type Host interface {
	Register(context.Context, core.RegisterNodeRequest) (core.RegisterNodeResponse, error)
	Heartbeat(context.Context, core.HeartbeatRequest) (core.HeartbeatResponse, error)
}

// Request describes this machine for registration under name, reporting
// gaiaVersion. The tailnet identity is left empty: the host takes it from
// the connection, never from the request.
func Request(name, gaiaVersion string) core.RegisterNodeRequest {
	return core.RegisterNodeRequest{
		Name:        name,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		GaiaVersion: gaiaVersion,
		APIVersion:  core.APIVersion,
	}
}

// Client calls the host's registry API over HTTP.
type Client struct {
	// HTTP carries the requests; its transport reaches the host.
	HTTP *http.Client
	// BaseURL is the host's API origin, e.g. https://gaia-host.
	BaseURL string
}

// Register registers this node.
func (c *Client) Register(ctx context.Context, req core.RegisterNodeRequest) (core.RegisterNodeResponse, error) {
	var res core.RegisterNodeResponse
	return res, c.post(ctx, core.NodesPath, req, &res)
}

// Heartbeat sends one heartbeat.
func (c *Client) Heartbeat(ctx context.Context, req core.HeartbeatRequest) (core.HeartbeatResponse, error) {
	var res core.HeartbeatResponse
	return res, c.post(ctx, core.HeartbeatPath, req, &res)
}

// post sends in as JSON to path and decodes a 2xx answer into out. A
// non-2xx answer is the host's *core.Error.
func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("reading the host's answer to %s: %w", path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var er core.ErrorResponse
		if json.Unmarshal(b, &er) == nil && er.Error != nil && er.Error.Code != "" {
			return er.Error
		}
		return fmt.Errorf("the host answered %s with HTTP %d", path, res.StatusCode)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("the host's answer to %s is not valid: %w", path, err)
	}
	return nil
}

// RunConfig configures Run. Zero durations take their defaults.
type RunConfig struct {
	Host    Host
	Request core.RegisterNodeRequest
	// MinBackoff and MaxBackoff bound the wait between registration
	// attempts while the host is unreachable; the wait doubles each time.
	MinBackoff, MaxBackoff time.Duration
	// ConfirmAfter is how soon after each registration the first heartbeat
	// is sent, at most the host's interval. Default: DefaultConfirmAfter.
	ConfirmAfter time.Duration
	// Link, if set, is kept up to date with where the node stands with the
	// host: registering, confirming (registered, no heartbeat answered yet)
	// or ready.
	Link *Link
	// Logger receives progress and failures. Default: discard.
	Logger *slog.Logger
}

// Run registers with the host, then heartbeats at the interval the host
// names, until ctx is done, when it returns nil. The first heartbeat after
// each registration goes out after ConfirmAfter, so a path to the host
// that is still settling is found quickly. A failed heartbeat (the host is
// unreachable or no longer knows this node) sends it back to registering,
// retried with backoff for as long as it takes: a node never stops because
// the host is down. The one error Run returns is a
// core.CodeVersionMismatch refusal, which only an upgrade fixes.
func Run(ctx context.Context, cfg RunConfig) error {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	link := cfg.Link
	confirm := cfg.ConfirmAfter
	if confirm <= 0 {
		confirm = DefaultConfirmAfter
	}
	minB, maxB := cfg.MinBackoff, cfg.MaxBackoff
	if minB <= 0 {
		minB = DefaultMinBackoff
	}
	if maxB < minB {
		maxB = max(minB, DefaultMaxBackoff)
	}
	backoff := minB
	for {
		link.Set(PhaseRegistering)
		res, err := cfg.Host.Register(ctx, cfg.Request)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if errors.Is(err, core.NewError(core.CodeVersionMismatch, "")) {
				return err
			}
			link.Failed(err)
			log.Warn("registration with the host failed; retrying", "err", err, "retry_in", backoff.String())
			if !sleep(ctx, backoff) {
				return nil
			}
			backoff = min(2*backoff, maxB)
			continue
		}
		backoff = minB
		interval := time.Duration(res.HeartbeatIntervalMS) * time.Millisecond
		if interval <= 0 {
			interval = DefaultHeartbeatInterval
		}
		link.Set(PhaseConfirming)
		log.Info("node registered", "id", res.Node.ID.String(), "name", res.Node.Name,
			"heartbeat_interval", interval.String())
		err = heartbeat(ctx, cfg.Host, res.Node.ID, min(confirm, interval), interval, link)
		if ctx.Err() != nil {
			return nil
		}
		link.Failed(err)
		log.Warn("heartbeat failed; registering again", "err", err)
	}
}

// heartbeat sends a heartbeat for id after first, then every interval, until
// one fails, and returns that error, or nil when ctx is done. Each answered
// heartbeat sets link ready.
func heartbeat(ctx context.Context, h Host, id core.NodeID, first, interval time.Duration, link *Link) error {
	if !sleep(ctx, first) {
		return nil
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := h.Heartbeat(ctx, core.HeartbeatRequest{ID: id}); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		link.Set(PhaseReady)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// sleep waits d, or reports false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Handler is the API on a node's local socket: /healthz answered by the
// node itself, every other /api/v1 request passed to forward (the host),
// except the node's own registration and heartbeat, which no local process
// may send in the node's name.
func Handler(forward http.Handler) http.Handler {
	mux := http.NewServeMux()
	notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, core.NewError(core.CodeNotFound, "no such endpoint"))
	})
	mux.HandleFunc(core.HealthzPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed,
				core.NewError(core.CodeInvalidArgument, "method "+r.Method+" not allowed"))
			return
		}
		writeJSON(w, http.StatusOK, core.HealthResponse{Status: "ok"})
	})
	mux.Handle("POST "+core.NodesPath, notFound)
	mux.Handle(core.HeartbeatPath, notFound)
	mux.Handle(core.APIPrefix+"/", forward)
	mux.Handle("/", notFound)
	return mux
}

// Forwarder passes requests to the host at base unchanged, over transport.
// When the host cannot be reached the answer is 503 with code unavailable
// and a message naming hostName.
//
// While link is not ready (the node is joining, registering or confirming
// its path to the host), a request waits at most RecoveryWait for the
// host's response headers, and a failure says that the node is
// re-establishing its connection, at what stage and for how long. A nil
// link is always ready.
func Forwarder(base *url.URL, transport http.RoundTripper, hostName string, link *Link, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &httputil.ReverseProxy{
		Rewrite:   func(pr *httputil.ProxyRequest) { pr.SetURL(base) },
		Transport: &recoveringTransport{rt: transport, link: link, wait: RecoveryWait},
		ErrorLog:  slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("forwarding to the host failed", "method", r.Method, "path", r.URL.Path, "err", err)
			e := &core.Error{
				Code:    core.CodeUnavailable,
				Message: fmt.Sprintf("the host %s is unreachable from this node: %v", hostName, err),
				Details: map[string]string{"host": hostName},
			}
			var re *recoveringError
			if errors.As(err, &re) {
				e.Message = re.message(hostName, time.Now())
				e.Details["link"] = string(re.state.Phase)
			}
			writeError(w, http.StatusServiceUnavailable, e)
		},
	}
}

// recoveringError is a forwarded request that failed while the link was not
// ready.
type recoveringError struct {
	state    LinkState
	wait     time.Duration
	timedOut bool
	err      error
}

func (e *recoveringError) Error() string {
	if e.timedOut {
		return fmt.Sprintf("no answer from the host within %s while %s", e.wait, e.state.Describe(time.Now()))
	}
	return fmt.Sprintf("%v (while %s)", e.err, e.state.Describe(time.Now()))
}

func (e *recoveringError) Unwrap() error { return e.err }

func (e *recoveringError) message(host string, now time.Time) string {
	cause := fmt.Sprintf("%v", e.err)
	if e.timedOut {
		cause = fmt.Sprintf("the host did not answer within %s", e.wait)
	}
	return fmt.Sprintf("this node is re-establishing its connection to the host %s (%s): %s; try again in a few seconds",
		host, e.state.Describe(now), cause)
}

// recoveringTransport bounds the wait for response headers while link is
// not ready; see Forwarder.
type recoveringTransport struct {
	rt   http.RoundTripper
	link *Link
	wait time.Duration
}

func (t *recoveringTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.link.Ready() {
		return t.rt.RoundTrip(req)
	}
	ctx, cancel := context.WithCancel(req.Context())
	timer := time.AfterFunc(t.wait, cancel)
	res, err := t.rt.RoundTrip(req.WithContext(ctx))
	if !timer.Stop() && req.Context().Err() == nil {
		// The wait ran out, whether or not headers arrived just after.
		if res != nil {
			_ = res.Body.Close()
		}
		return nil, &recoveringError{state: t.link.State(), wait: t.wait, timedOut: true, err: context.DeadlineExceeded}
	}
	if err != nil {
		cancel()
		if req.Context().Err() != nil {
			return nil, err
		}
		return nil, &recoveringError{state: t.link.State(), wait: t.wait, err: err}
	}
	// Headers arrived in time: the body streams for as long as it takes.
	res.Body = &cancelOnClose{ReadCloser: res.Body, cancel: cancel}
	return res, nil
}

// cancelOnClose releases a request's context when its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// DialHost connects to the host and returns the connection with the name
// TLS checks the host's certificate against.
type DialHost func(ctx context.Context) (conn net.Conn, serverName string, err error)

// HostTransport is the HTTP transport to the host: TLS 1.3 over the
// connection dial returns.
//
// The host is authenticated by the tailnet, not by TLS: dial reaches it
// over WireGuard, at the peer the control server names. TLS encrypts the
// API traffic and checks what it can. The server name sent is the tailnet
// IP dialled, so the host answers with its kept self-signed certificate
// rather than ordering a Tailscale one over ACME for the node; that
// certificate must be self-signed and name the IP. A certificate that
// verifies against the system roots for the host's MagicDNS name is also
// accepted.
func HostTransport(dial DialHost) *http.Transport {
	return &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// A dial to a host that is down hangs rather than failing;
			// bound it, so a forwarded CLI request is answered "host
			// unreachable" before the CLI gives up waiting.
			ctx, cancel := context.WithTimeout(ctx, DialTimeout)
			defer cancel()
			raw, name, err := dial(ctx)
			if err != nil {
				return nil, err
			}
			c := tls.Client(raw, hostTLSConfig(name, raw.RemoteAddr()))
			if err := c.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			return c, nil
		},
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        2,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// hostTLSConfig checks the host certificate as HostTransport describes.
func hostTLSConfig(serverName string, remote net.Addr) *tls.Config {
	ip := ""
	if h, _, err := net.SplitHostPort(remote.String()); err == nil {
		ip = h
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: ip,
		// Verification is VerifyConnection's, below.
		InsecureSkipVerify: true, //nolint:gosec // see HostTransport
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyHost(cs.PeerCertificates, serverName, ip)
		},
	}
}

// verifyHost accepts certs when the chain verifies against the system roots
// for serverName, or when the leaf is self-signed and names ip.
func verifyHost(certs []*x509.Certificate, serverName, ip string) error {
	if len(certs) == 0 {
		return errors.New("the host presented no certificate")
	}
	leaf := certs[0]
	if serverName != "" {
		inter := x509.NewCertPool()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: serverName, Intermediates: inter}); err == nil {
			return nil
		}
	}
	// Signed by its own key. (Not CheckSignatureFrom: the host's
	// certificate is not a CA, so that refuses it as a parent.)
	if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		return fmt.Errorf("the host certificate is neither valid for %q nor self-signed: %w", serverName, err)
	}
	if ip == "" || leaf.VerifyHostname(ip) != nil {
		return fmt.Errorf("the host's self-signed certificate does not name %s", ip)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, e *core.Error) {
	writeJSON(w, status, core.ErrorResponse{Error: e})
}
