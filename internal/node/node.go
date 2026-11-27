// Package node runs the node role. In this phase that is the in-process
// node of `gaia host --node`: it registers the host's own machine and keeps
// its last_seen current. The remote node, supervisor client and forwarder
// come in later slices.
package node

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// LocalIdentity is the tailnet identity of the in-process node in
// local-only mode, where there is no tailnet. It is stable, so restarting
// the host updates the same node instead of adding one.
const LocalIdentity = "local"

// Registrar is the host's registry as a node sees it. *host.Server
// implements it in-process; later slices implement it over the tailnet.
type Registrar interface {
	Register(context.Context, core.RegisterNodeRequest) (core.Node, error)
	Heartbeat(context.Context, core.HeartbeatRequest) (core.Node, error)
}

// LocalRequest describes this machine for registration as the in-process
// node, reporting gaiaVersion.
func LocalRequest(gaiaVersion string) core.RegisterNodeRequest {
	return core.RegisterNodeRequest{
		Name:            localName(),
		TailnetIdentity: LocalIdentity,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		GaiaVersion:     gaiaVersion,
		APIVersion:      core.APIVersion,
	}
}

// localName is the hostname up to its first dot, or "local".
func localName() string {
	h, err := os.Hostname()
	if err != nil {
		return "local"
	}
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	if h == "" {
		return "local"
	}
	return h
}

// RunLocal registers req with r, then heartbeats every interval until ctx
// is done, when it returns nil. A failed first registration is returned. A
// failed heartbeat is logged and retried at the next tick; a heartbeat for
// a node the host no longer knows registers again.
func RunLocal(ctx context.Context, r Registrar, req core.RegisterNodeRequest, interval time.Duration, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	n, err := r.Register(ctx, req)
	if err != nil {
		return err
	}
	log.Info("local node registered", "id", n.ID.String(), "name", n.Name)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		_, err := r.Heartbeat(ctx, core.HeartbeatRequest{ID: n.ID})
		if errors.Is(err, core.NewError(core.CodeNotFound, "")) {
			var again core.Node
			if again, err = r.Register(ctx, req); err == nil {
				n = again
				log.Info("local node registered again", "id", n.ID.String())
				continue
			}
		}
		if err != nil && ctx.Err() == nil {
			log.Warn("local node heartbeat failed", "err", err)
		}
	}
}
