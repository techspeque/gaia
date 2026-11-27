// Package service registers the Gaia daemon as a user-level service: a
// systemd user unit on Linux, a launchd agent on macOS. It never needs root
// and runs only `systemctl --user`, `loginctl show-user` and `launchctl`,
// through an injected Runner. Every file it writes carries Marker, and a
// file without it is neither overwritten nor removed.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Marker is the text every unit Gaia writes contains. Install and Uninstall
// touch only a file that holds it.
const Marker = "Written by gaia service install; gaia service uninstall removes it. Do not edit."

// Names of the one Gaia service per machine.
const (
	SystemdUnit  = "gaia.service"
	LaunchdLabel = "io.github.techspeque.gaia"
)

// ErrNotOurs is returned when the unit path holds a file Gaia did not write.
var ErrNotOurs = errors.New("not written by gaia")

// ErrUnsupported is returned on an OS with no user service support here.
var ErrUnsupported = errors.New("gaia service supports Linux and macOS only")

// Runner runs an OS command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// Platform is the machine the service is installed on.
type Platform struct {
	GOOS       string
	Home       string
	ConfigHome string // $XDG_CONFIG_HOME, or "" for ~/.config (Linux)
	UID        int
	User       string // login name, for the linger hint
	Run        Runner
	Sleep      func(time.Duration) // nil: time.Sleep
}

// EnvVar is one environment variable of the service.
type EnvVar struct{ Name, Value string }

// Spec is what the service runs.
type Spec struct {
	Args  []string // absolute binary path, then its arguments
	Env   []EnvVar // set in the service's environment
	Unset []string // removed from the service's environment (systemd only)
}

// Result reports what Install or Uninstall did.
type Result struct {
	Path    string
	Written bool // the unit file was created or rewritten
	Changed bool // anything changed: a file or the service's state
	// LingerHint, when not empty, is the one command the user must run so
	// the service starts at boot without a login (Linux). Gaia never runs it.
	LingerHint string
	LogHint    string // where the daemon's log goes
}

// UnitPath is where the unit lives on p.
func UnitPath(p Platform) (string, error) {
	switch p.GOOS {
	case "linux":
		cfg := p.ConfigHome
		if cfg == "" {
			cfg = filepath.Join(p.Home, ".config")
		}
		return filepath.Join(cfg, "systemd", "user", SystemdUnit), nil
	case "darwin":
		return filepath.Join(p.Home, "Library", "LaunchAgents", LaunchdLabel+".plist"), nil
	}
	return "", ErrUnsupported
}

func logPath(p Platform) string {
	return filepath.Join(p.Home, "Library", "Logs", "gaia", "gaia.log")
}

// Render returns the unit file for s on p.
func Render(p Platform, s Spec) ([]byte, error) {
	if len(s.Args) == 0 || !filepath.IsAbs(s.Args[0]) {
		return nil, errors.New("the service needs an absolute binary path")
	}
	for _, a := range s.Args {
		if strings.ContainsAny(a, "\x00\n\r") {
			return nil, fmt.Errorf("argument %q holds a control character", a)
		}
	}
	for _, e := range s.Env {
		if e.Name == "" || strings.ContainsAny(e.Name, "=\x00\n\r ") || strings.ContainsAny(e.Value, "\x00\n\r") {
			return nil, fmt.Errorf("bad environment variable %q", e.Name)
		}
	}
	switch p.GOOS {
	case "linux":
		return renderSystemd(s), nil
	case "darwin":
		return renderLaunchd(s, logPath(p)), nil
	}
	return nil, ErrUnsupported
}

// Install writes the unit for s, enables and starts it. A byte-identical
// Gaia unit is not rewritten, and a service already enabled and running is
// left alone.
func Install(ctx context.Context, p Platform, s Spec) (Result, error) {
	path, err := UnitPath(p)
	if err != nil {
		return Result{}, err
	}
	res := Result{Path: path}
	want, err := Render(p, s)
	if err != nil {
		return res, err
	}
	have, exists, err := readOurs(path)
	if err != nil {
		return res, err
	}
	if !exists || !bytes.Equal(have, want) {
		if p.GOOS == "darwin" {
			if err := os.MkdirAll(filepath.Dir(logPath(p)), 0o700); err != nil {
				return res, err
			}
		}
		if err := writeAtomic(path, want); err != nil {
			return res, err
		}
		res.Written, res.Changed = true, true
	}
	switch p.GOOS {
	case "linux":
		err = installSystemd(ctx, p, &res)
	case "darwin":
		res.LogHint = logPath(p)
		err = installLaunchd(ctx, p, path, &res)
	}
	return res, err
}

// Uninstall stops the service and removes its unit. It leaves every other
// file, the state directory included, in place. An absent unit is not an
// error.
func Uninstall(ctx context.Context, p Platform) (Result, error) {
	path, err := UnitPath(p)
	if err != nil {
		return Result{}, err
	}
	res := Result{Path: path}
	_, exists, err := readOurs(path)
	if err != nil || !exists {
		return res, err
	}
	switch p.GOOS {
	case "linux":
		if _, err := run(ctx, p, "systemctl", "--user", "disable", "--now", SystemdUnit); err != nil {
			return res, err
		}
	case "darwin":
		if loaded(ctx, p) {
			if _, err := run(ctx, p, "launchctl", "bootout", target(p)); err != nil {
				return res, err
			}
		}
	}
	if err := os.Remove(path); err != nil {
		return res, err
	}
	res.Changed = true
	if p.GOOS == "linux" {
		if _, err := run(ctx, p, "systemctl", "--user", "daemon-reload"); err != nil {
			return res, err
		}
	}
	return res, nil
}

// readOurs reads the unit at path. It reports exists=false when there is
// none, and ErrNotOurs when the path is not a regular file holding Marker.
func readOurs(path string) ([]byte, bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !fi.Mode().IsRegular() {
		return nil, true, fmt.Errorf("%s: %w (not a regular file)", path, ErrNotOurs)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, true, err
	}
	if !bytes.Contains(b, []byte(Marker)) {
		return nil, true, fmt.Errorf("%s: %w", path, ErrNotOurs)
	}
	return b, true, nil
}

// writeAtomic writes b to path through a temporary file in the same
// directory, so a reader never sees a partial unit.
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".gaia-unit-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// run runs one command, wrapping a failure with the command and its output.
func run(ctx context.Context, p Platform, name string, args ...string) (string, error) {
	out, err := p.Run(ctx, name, args...)
	if err != nil {
		cmd := strings.Join(append([]string{name}, args...), " ")
		if o := strings.TrimSpace(out); o != "" {
			return out, fmt.Errorf("%s: %w: %s", cmd, err, o)
		}
		return out, fmt.Errorf("%s: %w", cmd, err)
	}
	return out, nil
}

// succeeds reports whether the command exits 0, for status queries.
func succeeds(ctx context.Context, p Platform, name string, args ...string) bool {
	_, err := p.Run(ctx, name, args...)
	return err == nil
}

func installSystemd(ctx context.Context, p Platform, res *Result) error {
	res.LogHint = "journalctl --user -u " + SystemdUnit
	if res.Written {
		if _, err := run(ctx, p, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
	}
	if !succeeds(ctx, p, "systemctl", "--user", "is-enabled", "--quiet", SystemdUnit) {
		if _, err := run(ctx, p, "systemctl", "--user", "enable", SystemdUnit); err != nil {
			return err
		}
		res.Changed = true
	}
	active := succeeds(ctx, p, "systemctl", "--user", "is-active", "--quiet", SystemdUnit)
	switch {
	case res.Written && active:
		if _, err := run(ctx, p, "systemctl", "--user", "restart", SystemdUnit); err != nil {
			return err
		}
	case !active:
		if _, err := run(ctx, p, "systemctl", "--user", "start", SystemdUnit); err != nil {
			return err
		}
		res.Changed = true
	}
	out, err := p.Run(ctx, "loginctl", "show-user", strconv.Itoa(p.UID), "--property=Linger", "--value")
	if err != nil || strings.TrimSpace(out) != "yes" {
		res.LingerHint = "loginctl enable-linger " + p.User
	}
	return nil
}

func target(p Platform) string { return "gui/" + strconv.Itoa(p.UID) + "/" + LaunchdLabel }

func loaded(ctx context.Context, p Platform) bool {
	return succeeds(ctx, p, "launchctl", "print", target(p))
}

func installLaunchd(ctx context.Context, p Platform, path string, res *Result) error {
	isLoaded := loaded(ctx, p)
	if !res.Written && isLoaded {
		return nil
	}
	if isLoaded {
		if _, err := run(ctx, p, "launchctl", "bootout", target(p)); err != nil {
			return err
		}
	}
	if _, err := run(ctx, p, "launchctl", "enable", target(p)); err != nil {
		return err
	}
	res.Changed = true
	// bootout returns before launchd has finished removing the job, and a
	// bootstrap in that window fails; retry for a few seconds.
	sleep := p.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	domain := "gui/" + strconv.Itoa(p.UID)
	var err error
	for range 10 {
		if _, err = run(ctx, p, "launchctl", "bootstrap", domain, path); err == nil {
			return nil
		}
		if !isLoaded {
			return err
		}
		sleep(300 * time.Millisecond)
	}
	return err
}
