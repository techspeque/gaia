// Package sock is the user-only local socket between the gaia CLI and the
// daemon on the same machine (ADR-0004 rule 3): where it lives, how the
// daemon binds it and how a client dials it.
//
// The socket is a unix socket at mode 0600 in a runtime directory at mode
// 0700 owned by the user. The daemon holds an exclusive lock on a file next
// to it for as long as it listens, removes a leftover socket only after
// dialling it is refused, and closes any connection whose peer runs as
// another user.
package sock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// File names inside the runtime directory.
const (
	SocketName = "gaia.sock"
	lockName   = "gaia.lock"
)

// probeTimeout bounds the dial that decides whether a leftover socket is
// stale.
const probeTimeout = time.Second

// Dir returns the runtime directory: $XDG_RUNTIME_DIR/gaia when
// XDG_RUNTIME_DIR is set, else <os.TempDir()>/gaia-<uid>.
func Dir() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "gaia")
	}
	return filepath.Join(os.TempDir(), "gaia-"+strconv.Itoa(os.Getuid()))
}

// Path returns the socket path inside the runtime directory dir.
func Path(dir string) string { return filepath.Join(dir, SocketName) }

// Dial connects to the daemon socket inside dir.
func Dial(ctx context.Context, dir string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", Path(dir))
}

// Listener is the daemon's end of the local socket. Accept returns only
// connections from peers running as the daemon's own user.
type Listener struct {
	net.Listener
	path    string
	lock    *os.File
	uid     int
	peerUID func(net.Conn) (int, error)
	onDeny  func(error)

	closeOnce sync.Once
	closeErr  error
}

// Options adjusts Listen. The zero value is the production behaviour.
type Options struct {
	// OnDeny is called, if set, with the reason each time a connection is
	// refused by the peer-credential check.
	OnDeny func(error)
	// uid overrides the uid peers must have; tests only.
	uid *int
}

// Listen prepares the runtime directory dir and binds the socket in it.
// See the package documentation for the checks it makes.
func Listen(dir string, opts Options) (*Listener, error) {
	if !supported {
		return nil, core.NewError(core.CodeUnavailable, "the local socket is not supported on this platform yet")
	}
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	path := Path(dir)
	if len(path) > maxPathLen {
		return nil, &core.Error{
			Code: core.CodeInvalidArgument,
			Message: fmt.Sprintf("socket path %s is %d bytes, longer than the %d this platform allows: set XDG_RUNTIME_DIR to a shorter directory",
				path, len(path), maxPathLen),
			Details: map[string]string{"path": path},
		}
	}
	lock, err := lockFile(filepath.Join(dir, lockName))
	if err != nil {
		return nil, err
	}
	l, err := bind(path)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	uid := os.Getuid()
	if opts.uid != nil {
		uid = *opts.uid
	}
	return &Listener{Listener: l, path: path, lock: lock, uid: uid, peerUID: peerUID, onDeny: opts.OnDeny}, nil
}

// Path returns the bound socket path.
func (l *Listener) Path() string { return l.path }

// Accept waits for the next connection from a peer with the daemon's uid.
// Connections from any other peer are closed unread.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := l.peerUID(c)
		if err == nil && uid != l.uid {
			err = fmt.Errorf("peer uid %d is not the daemon's uid %d", uid, l.uid)
		}
		if err != nil {
			_ = c.Close()
			if l.onDeny != nil {
				l.onDeny(fmt.Errorf("local socket connection refused: %w", err))
			}
			continue
		}
		return c, nil
	}
}

// Close stops listening, removes the socket and releases the lock. Calls
// after the first return the first call's result.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		errs := []error{l.Listener.Close()}
		if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		errs = append(errs, l.lock.Close())
		l.closeErr = errors.Join(errs...)
	})
	return l.closeErr
}

// ensureDir creates dir at 0700 if missing, and refuses it unless it is a
// real directory owned by the current user with no group or other bits.
func ensureDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return unsafeDir(dir, fi.Mode(), "is a symbolic link")
	case !fi.IsDir():
		return unsafeDir(dir, fi.Mode(), "is not a directory")
	case fi.Mode().Perm()&0o077 != 0:
		return unsafeDir(dir, fi.Mode(), "is accessible by group or others: chmod 700 it")
	}
	if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
		return unsafeDir(dir, fi.Mode(), fmt.Sprintf("is owned by uid %d, not by this user", owner))
	}
	return nil
}

func unsafeDir(dir string, mode fs.FileMode, reason string) *core.Error {
	return &core.Error{
		Code:    core.CodeInvalidArgument,
		Message: fmt.Sprintf("runtime directory %s %s", dir, reason),
		Details: map[string]string{"path": dir, "mode": fmt.Sprintf("%04o", mode.Perm())},
	}
}

// bind removes a stale socket at path, binds a new one and makes it 0600.
// The caller holds the lock.
func bind(path string) (net.Listener, error) {
	if err := removeStale(path); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The directory is 0700, so no other user can reach the socket in the
	// moment before chmod.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	if fi.Mode()&fs.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		_ = l.Close()
		return nil, &core.Error{
			Code:    core.CodeInternal,
			Message: fmt.Sprintf("socket %s has mode %s after bind, want 0600", path, fi.Mode()),
			Details: map[string]string{"path": path},
		}
	}
	return l, nil
}

// removeStale removes a socket at path that no daemon answers on. A socket
// that answers, or anything that is not a socket, is left alone and refused.
func removeStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return &core.Error{
			Code:    core.CodeInvalidArgument,
			Message: fmt.Sprintf("%s exists and is not a socket; remove it by hand if it is not needed", path),
			Details: map[string]string{"path": path},
		}
	}
	c, err := net.DialTimeout("unix", path, probeTimeout)
	if err == nil {
		_ = c.Close()
		return daemonRunning(path)
	}
	if !isRefused(err) {
		return fmt.Errorf("probing leftover socket %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func daemonRunning(path string) *core.Error {
	return &core.Error{
		Code:    core.CodeConflict,
		Message: "another gaia daemon is already running on " + path,
		Details: map[string]string{"path": path},
	}
}
