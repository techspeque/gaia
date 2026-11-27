//go:build linux || darwin

package sock

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// shortDir returns a fresh directory with a path short enough for a unix
// socket on every platform (t.TempDir can exceed 104 bytes on darwin).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func runtimeDir(t *testing.T) string { return filepath.Join(shortDir(t), "gaia") }

func mustListen(t *testing.T, dir string, opts Options) *Listener {
	t.Helper()
	l, err := Listen(dir, opts)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func wantCode(t *testing.T, err error, code core.ErrorCode) {
	t.Helper()
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("err = %v, want core error %s", err, code)
	}
}

// echo accepts one connection at a time and writes "ok" to it.
func serveOK(l net.Listener) {
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("ok"))
			_ = c.Close()
		}
	}()
}

func TestDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := Dir(); got != "/run/user/1000/gaia" {
		t.Errorf("Dir() = %q", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got, want := Dir(), filepath.Join(os.TempDir(), "gaia-"+strconv.Itoa(os.Getuid())); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

func TestListenModes(t *testing.T) {
	dir := runtimeDir(t)
	l := mustListen(t, dir, Options{})
	di, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("runtime dir mode %04o, want 0700", di.Mode().Perm())
	}
	si, err := os.Lstat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if si.Mode()&fs.ModeSocket == 0 || si.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %s, want socket 0600", si.Mode())
	}
	li, err := os.Lstat(filepath.Join(dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	if li.Mode().Perm() != 0o600 {
		t.Errorf("lock mode %04o, want 0600", li.Mode().Perm())
	}
}

func TestDialRoundTripAndClose(t *testing.T) {
	dir := runtimeDir(t)
	l := mustListen(t, dir, Options{})
	serveOK(l)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	_ = c.Close()
	if err != nil || string(b) != "ok" {
		t.Fatalf("read %q, %v", b, err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(l.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket still present after Close: %v", err)
	}
	// The lock is released: a new daemon can listen.
	mustListen(t, dir, Options{})
}

func TestRefusesUnsafeDir(t *testing.T) {
	for _, mode := range []fs.FileMode{0o755, 0o750, 0o701, 0o770} {
		dir := runtimeDir(t)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		_, err := Listen(dir, Options{})
		wantCode(t, err, core.CodeInvalidArgument)
	}

	// A symlink to a good directory is refused.
	base := shortDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, err := Listen(link, Options{})
	wantCode(t, err, core.CodeInvalidArgument)

	// A file in place of the directory is refused.
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Listen(file, Options{})
	wantCode(t, err, core.CodeInvalidArgument)
}

func TestSecondListenerRefused(t *testing.T) {
	dir := runtimeDir(t)
	l := mustListen(t, dir, Options{})
	_, err := Listen(dir, Options{})
	wantCode(t, err, core.CodeConflict)
	// The first listener's socket is untouched.
	if _, err := os.Lstat(l.Path()); err != nil {
		t.Fatalf("first socket gone: %v", err)
	}
}

func TestStaleSocketRemoved(t *testing.T) {
	dir := runtimeDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A socket nobody listens on, as a killed daemon leaves it.
	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: Path(dir), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ul.SetUnlinkOnClose(false)
	_ = ul.Close()
	if _, err := os.Lstat(Path(dir)); err != nil {
		t.Fatalf("stale socket not created: %v", err)
	}
	l := mustListen(t, dir, Options{})
	serveOK(l)
	c, err := Dial(context.Background(), dir)
	if err != nil {
		t.Fatalf("dial new socket: %v", err)
	}
	_ = c.Close()
}

func TestLiveSocketKept(t *testing.T) {
	// A daemon answering on the path without holding the lock (as an older
	// build might): the socket answers, so it is not removed.
	dir := runtimeDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := net.Listen("unix", Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	serveOK(other)
	_, err = Listen(dir, Options{})
	wantCode(t, err, core.CodeConflict)
	if _, err := os.Lstat(Path(dir)); err != nil {
		t.Fatalf("live socket removed: %v", err)
	}
}

func TestNonSocketAtPathKept(t *testing.T) {
	dir := runtimeDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dir), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Listen(dir, Options{})
	wantCode(t, err, core.CodeInvalidArgument)
	b, err := os.ReadFile(Path(dir))
	if err != nil || string(b) != "keep" {
		t.Fatalf("file changed: %q, %v", b, err)
	}
}

func TestTooLongPathRefused(t *testing.T) {
	dir := filepath.Join(shortDir(t), strings.Repeat("d", 120))
	_, err := Listen(dir, Options{})
	wantCode(t, err, core.CodeInvalidArgument)
	if !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Errorf("message %q does not name XDG_RUNTIME_DIR", err)
	}
}

func TestPeerWithOtherUIDDisconnected(t *testing.T) {
	dir := runtimeDir(t)
	other := os.Getuid() + 1
	var denied = make(chan error, 1)
	l := mustListen(t, dir, Options{uid: &other, OnDeny: func(err error) { denied <- err }})
	serveOK(l)
	c, err := Dial(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(c)
	if len(b) != 0 {
		t.Errorf("foreign peer received %q", b)
	}
	select {
	case err := <-denied:
		if !strings.Contains(err.Error(), "peer uid") {
			t.Errorf("deny reason %q", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no deny reported")
	}
}

func TestPeerUIDIsOwn(t *testing.T) {
	dir := runtimeDir(t)
	l := mustListen(t, dir, Options{})
	got := make(chan int, 1)
	go func() {
		c, err := l.Listener.Accept()
		if err != nil {
			got <- -1
			return
		}
		defer c.Close()
		uid, err := peerUID(c)
		if err != nil {
			got <- -2
			return
		}
		got <- uid
	}()
	c, err := Dial(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if uid := <-got; uid != os.Getuid() {
		t.Errorf("peer uid %d, want %d", uid, os.Getuid())
	}
}

func TestDialNoDaemon(t *testing.T) {
	// The runtime directory does not exist.
	missing := runtimeDir(t)
	if _, err := Dial(context.Background(), missing); !errors.Is(err, ErrNoDaemon) {
		t.Errorf("missing dir: %v, want ErrNoDaemon", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Dial created the runtime directory: %v", err)
	}

	// The directory exists but holds no socket.
	empty := runtimeDir(t)
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(context.Background(), empty); !errors.Is(err, ErrNoDaemon) {
		t.Errorf("missing socket: %v, want ErrNoDaemon", err)
	}

	// A socket nobody listens on, as a killed daemon leaves it.
	stale := runtimeDir(t)
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: Path(stale), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ul.SetUnlinkOnClose(false)
	_ = ul.Close()
	if _, err := Dial(context.Background(), stale); !errors.Is(err, ErrNoDaemon) {
		t.Errorf("stale socket: %v, want ErrNoDaemon", err)
	}
}

func TestDialRefusesUnsafeDir(t *testing.T) {
	// A daemon listening in a directory that later became group-readable.
	dir := runtimeDir(t)
	l := mustListen(t, dir, Options{})
	serveOK(l)
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	_, err := Dial(context.Background(), dir)
	wantCode(t, err, core.CodeInvalidArgument)
	if errors.Is(err, ErrNoDaemon) {
		t.Error("an unsafe directory is reported as no daemon")
	}

	// A symlink to a good directory with a live daemon.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(shortDir(t), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	_, err = Dial(context.Background(), link)
	wantCode(t, err, core.CodeInvalidArgument)
}
