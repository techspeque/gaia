// Package host runs the host role: the machine registry and the API.
//
// The API is served on the user-only local socket (Serve) and, with
// `gaia host --tailnet`, on the tailnet (ServeTailnet), where every request
// is authorised by the peer's tailnet identity against an Allowlist
// (ADR-0004 rules 4, 5). /healthz is served on the socket only. Nodes reach the
// registry through Register and Heartbeat, in-process for the host's own
// machine (`gaia host --node`).
package host

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/techspeque/gaia/internal/core"
	gnet "github.com/techspeque/gaia/internal/net"
	"github.com/techspeque/gaia/internal/store"
)

// Defaults for Options.
const (
	DefaultHeartbeatInterval = 10 * time.Second
	DefaultMissedHeartbeats  = 3
)

// shutdownTimeout bounds how long Serve waits for in-flight requests.
const shutdownTimeout = 5 * time.Second

// Options configures a Server. Zero fields take their defaults.
type Options struct {
	// Version is the Gaia version this binary reports.
	Version string
	// HeartbeatInterval is how often nodes heartbeat.
	HeartbeatInterval time.Duration
	// MissedHeartbeats is how many intervals may pass without a heartbeat
	// before a node is offline.
	MissedHeartbeats int
	// Logger receives request and error logs. Default: discard.
	Logger *slog.Logger
	// Now is the clock. Default: time.Now.
	Now func() time.Time
	// Allowlist names the tailnet peers TailnetHandler admits. Default:
	// no one.
	Allowlist Allowlist
	// APIVersion is the API version this host speaks and reports. Nodes at
	// it and one behind may register. Default: core.APIVersion.
	APIVersion int
}

// Server is the host's registry and API.
type Server struct {
	st   *store.Store
	opts Options
	log  *slog.Logger
}

// New returns a Server over st.
func New(st *store.Store, opts Options) *Server {
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.MissedHeartbeats <= 0 {
		opts.MissedHeartbeats = DefaultMissedHeartbeats
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.APIVersion <= 0 {
		opts.APIVersion = core.APIVersion
	}
	return &Server{st: st, opts: opts, log: opts.Logger}
}

// HeartbeatInterval returns the interval nodes are expected to heartbeat at.
func (s *Server) HeartbeatInterval() time.Duration { return s.opts.HeartbeatInterval }

// Serve serves the API on l until ctx is done, then shuts down, waiting up
// to five seconds for requests in flight. It closes l.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	return s.serve(ctx, l, s.Handler())
}

// ServeTailnet serves TailnetHandler(w) on l, a TLS listener on the
// tailnet, as Serve does.
func (s *Server) ServeTailnet(ctx context.Context, l net.Listener, w WhoIser) error {
	return s.serve(ctx, l, s.TailnetHandler(w))
}

func (s *Server) serve(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := srv.Shutdown(sctx)
	if errors.Is(err, context.DeadlineExceeded) {
		// Requests in flight had their grace period. What is left is cut,
		// typically a tailnet connection a node opened and abandoned
		// before its TLS handshake, which would hold until the handshake
		// timeout.
		s.log.Warn("connections still open after the shutdown grace period; closing them")
		err = srv.Close()
	}
	if serr := <-errc; !errors.Is(serr, http.ErrServerClosed) && serr != nil {
		err = errors.Join(err, serr)
	}
	return err
}

// Handler returns the API handler for the local socket.
func (s *Server) Handler() http.Handler {
	return s.logRequests(s.routes(true))
}

// WhoIser names the tailnet peer behind a connection's remote address.
// *net.Tailnet is one.
type WhoIser interface {
	WhoIs(ctx context.Context, remoteAddr string) (gnet.Peer, error)
}

// TailnetHandler returns the API handler for the tailnet: the /api/v1
// routes, without /healthz, behind a WhoIs check. A peer that WhoIs cannot
// name or that Options.Allowlist does not admit gets 403 with code
// forbidden and a generic message, before any routing, and a warn log line.
func (s *Server) TailnetHandler(w WhoIser) http.Handler {
	api := s.routes(false)
	return s.logRequests(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		peer, err := w.WhoIs(r.Context(), r.RemoteAddr)
		switch {
		case err != nil:
			s.log.Warn("tailnet request refused: peer unknown", "remote_addr", r.RemoteAddr, "err", err)
		case !s.opts.Allowlist.Allows(peer):
			s.log.Warn("tailnet request refused: peer not allowlisted", "remote_addr", r.RemoteAddr,
				"login", peer.LoginName, "node", peer.NodeName, "tags", peer.Tags)
		default:
			api.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), peerKey{}, peer)))
			return
		}
		writeError(rw, http.StatusForbidden, core.NewError(core.CodeForbidden, "permission denied"))
	}))
}

// routes returns the API routes. The socket mux adds /healthz; the
// tailnet mux adds the node's own registration and heartbeat, which need
// the peer identity TailnetHandler puts in the request context.
func (s *Server) routes(socket bool) *http.ServeMux {
	mux := http.NewServeMux()
	if socket {
		mux.HandleFunc(core.HealthzPath, s.methods(map[string]handler{http.MethodGet: s.healthz}))
	}
	mux.HandleFunc(core.VersionPath, s.methods(map[string]handler{http.MethodGet: s.version}))
	nodes := map[string]handler{http.MethodGet: s.listNodes}
	if !socket {
		nodes[http.MethodPost] = s.registerHTTP
		mux.HandleFunc(core.HeartbeatPath, s.methods(map[string]handler{http.MethodPost: s.heartbeatHTTP}))
	}
	mux.HandleFunc(core.NodesPath, s.methods(nodes))
	mux.HandleFunc(core.NodesPath+"/{name}", s.methods(map[string]handler{http.MethodDelete: s.removeHTTP}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, core.NewError(core.CodeNotFound, "no such endpoint"))
	})
	return mux
}

// handler answers one request with a body to encode as JSON, or an error.
type handler func(*http.Request) (any, error)

// methods dispatches on the request method; GET also answers HEAD. Any
// other method is 405 with the Allow header.
func (s *Server) methods(hs map[string]handler) http.HandlerFunc {
	allow := make([]string, 0, len(hs)+1)
	for m := range hs {
		allow = append(allow, m)
	}
	if _, ok := hs[http.MethodGet]; ok {
		allow = append(allow, http.MethodHead)
	}
	slices.Sort(allow)
	return func(w http.ResponseWriter, r *http.Request) {
		m := r.Method
		if m == http.MethodHead {
			m = http.MethodGet
		}
		h, ok := hs[m]
		if !ok {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			writeError(w, http.StatusMethodNotAllowed,
				core.NewError(core.CodeInvalidArgument, "method "+r.Method+" not allowed"))
			return
		}
		s.respond(w, r, h)
	}
}

// respond runs h and writes its body, or its error. A typed *core.Error
// other than internal is the caller's to see and is sent with its status;
// any other error is logged and answered with a generic internal error, so
// no detail leaks.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, h handler) {
	body, err := h(r)
	if err == nil {
		writeJSON(w, http.StatusOK, body)
		return
	}
	var ce *core.Error
	if errors.As(err, &ce) && ce.Code != core.CodeInternal {
		writeError(w, statusFor(ce.Code), ce)
		return
	}
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, core.NewError(core.CodeInternal, "internal error"))
}

// statusFor maps an error code to its HTTP status.
func statusFor(c core.ErrorCode) int {
	switch c {
	case core.CodeInvalidArgument:
		return http.StatusBadRequest
	case core.CodeUnauthorized:
		return http.StatusUnauthorized
	case core.CodeForbidden:
		return http.StatusForbidden
	case core.CodeNotFound:
		return http.StatusNotFound
	case core.CodeConflict, core.CodeVersionMismatch:
		return http.StatusConflict
	case core.CodeUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// maxRequestBody bounds a request body the API decodes.
const maxRequestBody = 64 << 10

// decode reads r's JSON body into v.
func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(v); err != nil {
		return core.NewError(core.CodeInvalidArgument, "the request body is not valid JSON for "+r.URL.Path)
	}
	return nil
}

func (s *Server) healthz(*http.Request) (any, error) {
	return core.HealthResponse{Status: "ok"}, nil
}

func (s *Server) version(*http.Request) (any, error) {
	return core.VersionResponse{APIVersion: s.opts.APIVersion, GaiaVersion: s.opts.Version}, nil
}

// peerKey is the request-context key of the tailnet peer.
type peerKey struct{}

// peerFrom returns the tailnet peer TailnetHandler admitted.
func peerFrom(r *http.Request) (gnet.Peer, bool) {
	p, ok := r.Context().Value(peerKey{}).(gnet.Peer)
	return p, ok
}

// peerIdentity is the node identity of an admitted peer: its stable id.
// The handlers that need it run only on the tailnet mux.
func peerIdentity(r *http.Request) (string, error) {
	p, ok := peerFrom(r)
	if !ok || p.StableID == "" {
		return "", core.NewError(core.CodeForbidden, "permission denied")
	}
	return p.StableID, nil
}

// registerHTTP registers the calling peer. Its tailnet identity is the
// peer's, from WhoIs; the body's is ignored.
func (s *Server) registerHTTP(r *http.Request) (any, error) {
	ident, err := peerIdentity(r)
	if err != nil {
		return nil, err
	}
	var req core.RegisterNodeRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	req.TailnetIdentity = ident
	n, err := s.Register(r.Context(), req)
	if err != nil {
		return nil, err
	}
	return core.RegisterNodeResponse{Node: n, HeartbeatIntervalMS: s.opts.HeartbeatInterval.Milliseconds()}, nil
}

// heartbeatHTTP records a heartbeat from the calling peer, which must be
// the node it names.
func (s *Server) heartbeatHTTP(r *http.Request) (any, error) {
	ident, err := peerIdentity(r)
	if err != nil {
		return nil, err
	}
	var req core.HeartbeatRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	n, err := s.heartbeat(r.Context(), req, ident)
	if err != nil {
		return nil, err
	}
	return core.HeartbeatResponse{Node: n}, nil
}

// removeHTTP removes the node named by the path.
func (s *Server) removeHTTP(r *http.Request) (any, error) {
	n, err := s.RemoveNode(r.Context(), r.PathValue("name"))
	if err != nil {
		return nil, err
	}
	return core.RemoveNodeResponse{Node: n}, nil
}

func (s *Server) listNodes(r *http.Request) (any, error) {
	nodes, err := s.st.ListNodes(r.Context())
	if err != nil {
		return nil, err
	}
	now := s.opts.Now()
	out := core.ListNodesResponse{Nodes: make([]core.Node, 0, len(nodes))}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, s.apiNode(n, now))
	}
	return out, nil
}

// apiNode converts a stored node, deriving its state from last_seen.
func (s *Server) apiNode(n store.Node, now time.Time) core.Node {
	state := core.NodeOffline
	if now.Sub(n.LastSeen) <= s.opts.HeartbeatInterval*time.Duration(s.opts.MissedHeartbeats) {
		state = core.NodeOnline
	}
	return core.Node{
		ID:              n.ID,
		Name:            n.Name,
		TailnetIdentity: n.TailnetIdentity,
		OS:              n.OS,
		Arch:            n.Arch,
		GaiaVersion:     n.GaiaVersion,
		APIVersion:      n.APIVersion,
		State:           state,
		LastSeen:        n.LastSeen,
	}
}

// Register records a node. A tailnet identity already registered updates
// that node, keeping its id; otherwise a new node is created. A node whose
// API version the host does not serve is a core.CodeVersionMismatch error
// that says what to upgrade; a name another identity holds is
// core.CodeConflict.
func (s *Server) Register(ctx context.Context, req core.RegisterNodeRequest) (core.Node, error) {
	if req.TailnetIdentity == "" {
		return core.Node{}, core.NewError(core.CodeInvalidArgument, "name and tailnet identity are required")
	}
	if err := core.ValidNodeName(req.Name); err != nil {
		return core.Node{}, err
	}
	if err := s.negotiate(req); err != nil {
		return core.Node{}, err
	}
	now := s.opts.Now().UTC()
	n := store.Node{
		Name:            req.Name,
		TailnetIdentity: req.TailnetIdentity,
		OS:              req.OS,
		Arch:            req.Arch,
		GaiaVersion:     req.GaiaVersion,
		APIVersion:      req.APIVersion,
		CreatedAt:       now,
		LastSeen:        now,
	}
	existing, found, err := s.findByIdentity(ctx, req.TailnetIdentity)
	if err != nil {
		return core.Node{}, err
	}
	if found {
		n.ID, n.CreatedAt = existing.ID, existing.CreatedAt
		err = s.st.UpdateNode(ctx, n)
	} else {
		if n.ID, err = newNodeID(); err != nil {
			return core.Node{}, err
		}
		err = s.st.CreateNode(ctx, n)
	}
	if err != nil {
		return core.Node{}, err
	}
	s.log.Info("node registered", "id", n.ID.String(), "name", n.Name, "updated", found)
	return s.apiNode(n, now), nil
}

// negotiate accepts a node at the host's API version or one behind (never
// below 1), and otherwise returns the version error with the instruction.
func (s *Server) negotiate(req core.RegisterNodeRequest) error {
	hostV, nodeV := s.opts.APIVersion, req.APIVersion
	oldest := max(1, hostV-1)
	if nodeV >= oldest && nodeV <= hostV {
		return nil
	}
	hostGaia := s.opts.Version
	if hostGaia == "" {
		hostGaia = "unknown version"
	}
	var msg string
	if nodeV > hostV {
		msg = fmt.Sprintf("node %q speaks API v%d, newer than the host (gaia %s, API v%d): upgrade gaia on the host to the node's version (%s) and restart `gaia host`",
			req.Name, nodeV, hostGaia, hostV, orUnknown(req.GaiaVersion))
	} else {
		msg = fmt.Sprintf("node %q speaks API v%d; the host (gaia %s) accepts API v%d to v%d: install gaia %s on the node and restart `gaia node`",
			req.Name, nodeV, hostGaia, oldest, hostV, hostGaia)
	}
	return &core.Error{
		Code:    core.CodeVersionMismatch,
		Message: msg,
		Details: map[string]string{
			"node_api_version":  strconv.Itoa(nodeV),
			"host_api_version":  strconv.Itoa(hostV),
			"host_gaia_version": hostGaia,
		},
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown version"
	}
	return s
}

// Heartbeat marks the node seen now. An unknown id is core.CodeNotFound.
// It is the in-process node's; a node on the tailnet heartbeats through
// the API, where its identity is checked.
func (s *Server) Heartbeat(ctx context.Context, req core.HeartbeatRequest) (core.Node, error) {
	return s.heartbeat(ctx, req, "")
}

// heartbeat is Heartbeat; a non-empty ident must be the node's tailnet
// identity, else core.CodeForbidden and nothing is recorded.
func (s *Server) heartbeat(ctx context.Context, req core.HeartbeatRequest, ident string) (core.Node, error) {
	if req.ID.IsZero() {
		return core.Node{}, core.NewError(core.CodeInvalidArgument, "heartbeat without a node id")
	}
	if ident != "" {
		n, err := s.st.GetNode(ctx, req.ID)
		if err != nil {
			return core.Node{}, err
		}
		if n.TailnetIdentity != ident {
			s.log.Warn("heartbeat refused: peer is not the node", "id", req.ID.String(), "name", n.Name)
			return core.Node{}, core.NewError(core.CodeForbidden, "permission denied")
		}
	}
	now := s.opts.Now().UTC()
	if err := s.st.UpdateNodeLastSeen(ctx, req.ID, now); err != nil {
		return core.Node{}, err
	}
	n, err := s.st.GetNode(ctx, req.ID)
	if err != nil {
		return core.Node{}, err
	}
	return s.apiNode(n, now), nil
}

// RemoveNode removes the node called name and returns it as it was. An
// unknown name is core.CodeNotFound.
func (s *Server) RemoveNode(ctx context.Context, name string) (core.Node, error) {
	if err := core.ValidNodeName(name); err != nil {
		return core.Node{}, err
	}
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		return core.Node{}, err
	}
	for _, n := range nodes {
		if n.Name != name {
			continue
		}
		if err := s.st.RemoveNode(ctx, n.ID); err != nil {
			return core.Node{}, err
		}
		s.log.Info("node removed", "id", n.ID.String(), "name", n.Name)
		return s.apiNode(n, s.opts.Now()), nil
	}
	return core.Node{}, &core.Error{
		Code:    core.CodeNotFound,
		Message: fmt.Sprintf("no node named %q", name),
		Details: map[string]string{"name": name},
	}
}

// findByIdentity scans the registry. Registries hold a handful of machines.
func (s *Server) findByIdentity(ctx context.Context, ident string) (store.Node, bool, error) {
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		return store.Node{}, false, err
	}
	for _, n := range nodes {
		if n.TailnetIdentity == ident {
			return n, true, nil
		}
	}
	return store.Node{}, false, nil
}

func newNodeID() (core.NodeID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return core.NodeID{}, err
	}
	return core.NewNodeID(b), nil
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

// statusRecorder captures the status code for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}
