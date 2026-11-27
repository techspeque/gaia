//go:build linux || darwin

package sock

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"

	"github.com/techspeque/gaia/internal/core"
	"golang.org/x/sys/unix"
)

const supported = true

// lockFile opens path at 0600 and takes an exclusive, non-blocking flock on
// it. The lock lasts until the file is closed or the process exits, so a
// killed daemon never leaves it held.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, &core.Error{
				Code:    core.CodeConflict,
				Message: "another gaia daemon holds " + path,
				Details: map[string]string{"path": path},
			}
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return f, nil
}

// isRefused reports whether a dial error proves nothing listens.
func isRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }

// fileOwner returns the uid that owns fi.
func fileOwner(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// peerUID returns the uid of the process on the other end of c.
func peerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("connection is %T, not a unix socket", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		uid  int
		cerr error
	)
	if err := raw.Control(func(fd uintptr) { uid, cerr = fdPeerUID(int(fd)) }); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, fmt.Errorf("reading peer credentials: %w", cerr)
	}
	return uid, nil
}
