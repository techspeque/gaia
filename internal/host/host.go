// Package host runs the host role: the machine registry and the API.
//
// The API is served on whatever listener the caller passes to Serve; in
// this phase that is the user-only local socket (ADR-0004). Nodes reach the
// registry through Register and Heartbeat, in-process for the host's own
// machine (`gaia host --node`).
package host

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/techspeque/gaia/internal/core"
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
	return &Server{st: st, opts: opts, log: opts.Logger}
}

// HeartbeatInterval returns the interval nodes are expected to heartbeat at.
func (s *Server) HeartbeatInterval() time.Duration { return s.opts.HeartbeatInterval }

// Serve serves the API on l until ctx is done, then shuts down, waiting up
// to five seconds for requests in flight. It closes l.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
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
	if serr := <-errc; !errors.Is(serr, http.ErrServerClosed) && serr != nil {
		err = errors.Join(err, serr)
	}
	return err
}

// Handler returns the API handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(core.HealthzPath, s.get(s.healthz))
	mux.HandleFunc(core.VersionPath, s.get(s.version))
	mux.HandleFunc(core.NodesPath, s.get(s.listNodes))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, core.NewError(core.CodeNotFound, "no such endpoint"))
	})
	return s.logRequests(mux)
}

func (s *Server) get(h func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed,
				core.NewError(core.CodeInvalidArgument, "method "+r.Method+" not allowed"))
			return
		}
		body, err := h(r)
		if err != nil {
			s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			writeError(w, http.StatusInternalServerError, core.NewError(core.CodeInternal, "internal error"))
			return
		}
		writeJSON(w, http.StatusOK, body)
	}
}

func (s *Server) healthz(*http.Request) (any, error) {
	return core.HealthResponse{Status: "ok"}, nil
}

func (s *Server) version(*http.Request) (any, error) {
	return core.VersionResponse{APIVersion: core.APIVersion, GaiaVersion: s.opts.Version}, nil
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
// that node, keeping its id; otherwise a new node is created.
func (s *Server) Register(ctx context.Context, req core.RegisterNodeRequest) (core.Node, error) {
	if req.Name == "" || req.TailnetIdentity == "" {
		return core.Node{}, core.NewError(core.CodeInvalidArgument, "name and tailnet identity are required")
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

// Heartbeat marks the node seen now. An unknown id is core.CodeNotFound.
func (s *Server) Heartbeat(ctx context.Context, req core.HeartbeatRequest) (core.Node, error) {
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
