// Package node runs the node role: the in-process node of
// `gaia host --node` (RunLocal), which registers the host's own machine,
// and the node on another machine (`gaia node`), which registers with the
// host over the tailnet, heartbeats (Run) and forwards its local socket's
// API requests to the host (Handler, Forwarder). This package does not
// touch tsnet: the caller supplies the connection to the host.
package node

import (
	"context"
	"errors"
	"log/slog"
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
// node called name, the host's own name, reporting gaiaVersion.
// tailnetName is the host's assigned tailnet name, "" in local-only mode.
func LocalRequest(name, tailnetName, gaiaVersion string) core.RegisterNodeRequest {
	return core.RegisterNodeRequest{
		Name:            name,
		TailnetIdentity: LocalIdentity,
		TailnetName:     tailnetName,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		GaiaVersion:     gaiaVersion,
		APIVersion:      core.APIVersion,
	}
}

// MachineName turns a hostname into a node name that passes
// core.ValidNodeName: its first label, lower case, with every character
// outside a-z, 0-9 and '-' replaced by '-', cut to 63 characters, with no
// leading or trailing '-'. It is "" when nothing is left.
func MachineName(hostname string) string {
	h, _, _ := strings.Cut(strings.ToLower(hostname), ".")
	b := []byte(h)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	if len(b) > 63 {
		b = b[:63]
	}
	return strings.Trim(string(b), "-")
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
