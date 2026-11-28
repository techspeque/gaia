package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/techspeque/gaia/internal/core"
	gnet "github.com/techspeque/gaia/internal/net"
	"github.com/techspeque/gaia/internal/node"
)

// nameFile, in a daemon's state directory, records the daemon's name: the
// name it registers under and asks the control server for. Recording it
// keeps the name stable across starts and machine renames.
const nameFile = "name"

// namePrefix starts a daemon's default name, so that Gaia's tailnet device
// on a machine never asks for the name of the machine's own Tailscale
// device, which the control server would answer with a suffixed name.
const namePrefix = "gaia-"

// machineName is node.MachineName of the machine's hostname, or "" when the
// hostname is unknown or has no usable character.
func machineName(hostname func() (string, error)) string {
	h, err := hostname()
	if err != nil {
		return ""
	}
	return node.MachineName(h)
}

// defaultName is gaia-<machine>, cut to a valid node name, or gaia-<role>
// when machine is "".
func defaultName(machine, role string) string {
	if machine == "" {
		return namePrefix + role
	}
	n := namePrefix + machine
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	return n
}

// legacyName is the name a daemon of role took before names were recorded:
// the machine name, or gaia-<role> when it is unknown.
func legacyName(machine, role string) string {
	if machine == "" {
		return namePrefix + role
	}
	return machine
}

// resolveName returns the daemon's name: flagName when given; else the name
// recorded in state; else, for a state directory a daemon used before names
// were recorded (it exists and is not empty), the name it had then, so an
// existing install keeps its tailnet name; else defaultName. It reads state
// and writes nothing; recordName stores the result.
func resolveName(flagName, state, machine, role string) (string, error) {
	if flagName != "" {
		return flagName, nil
	}
	file := filepath.Join(state, nameFile)
	b, err := os.ReadFile(file)
	switch {
	case err == nil:
		name := strings.TrimSpace(string(b))
		if err := core.ValidNodeName(name); err != nil {
			return "", fmt.Errorf("%s: %s; correct it, or pass --name", file, err.(*core.Error).Message)
		}
		return name, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("reading the recorded name: %w", err)
	}
	entries, err := os.ReadDir(state)
	switch {
	case err == nil && len(entries) > 0:
		return legacyName(machine, role), nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("state directory: %w", err)
	}
	return defaultName(machine, role), nil
}

// selfNamer is *gnet.Tailnet's Self.
type selfNamer interface {
	Self(ctx context.Context) (gnet.SelfName, error)
}

// reportTailnetName logs the tailnet name the control server gave this
// daemon, which asked for asked, and returns its short name ("" when
// unknown). When it is not the one asked for, as on a machine where that
// name is already taken, a warning says which name to use: --host on the
// nodes for the host, the node: allowlist entry for a node.
func reportTailnetName(ctx context.Context, t selfNamer, asked, role string, log *slog.Logger) string {
	self, err := t.Self(ctx)
	if err != nil {
		log.Warn("cannot read this daemon's tailnet name", "err", err)
		return ""
	}
	log.Info("tailnet name assigned", "asked", asked, "tailnet_name", self.Name, "tailnet_dns_name", self.DNSName)
	if self.Name == "" || self.Name == asked {
		return self.Name
	}
	use := "start nodes with --host " + self.Name
	if role == "node" {
		use = "allow this node on the host as node:" + self.Name
	}
	log.Warn("the control server gave this "+role+" another tailnet name than it asked for, as that name is taken; "+use,
		"asked", asked, "tailnet_name", self.Name, "tailnet_dns_name", self.DNSName)
	return self.Name
}

// recordName writes name to the state directory's name file, mode 0600,
// unless it already holds it. The directory must exist.
func recordName(state, name string) error {
	file := filepath.Join(state, nameFile)
	if b, err := os.ReadFile(file); err == nil && strings.TrimSpace(string(b)) == name {
		return nil
	}
	if err := os.WriteFile(file, []byte(name+"\n"), 0o600); err != nil {
		return fmt.Errorf("recording the name: %w", err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return fmt.Errorf("recording the name: %w", err)
	}
	return nil
}
