package supervise

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

const (
	// exitUsage is the exit code for bad flags, as for the other daemons.
	exitUsage = 2
	// maxSpec bounds the spec read from stdin.
	maxSpec = 16 << 20
	// drainTimeout bounds reading the agent's last output after it exits.
	drainTimeout = 2 * time.Second
	// closeTimeout bounds how long open streams get to send their last
	// bytes after the session ends.
	closeTimeout = 5 * time.Second
)

// Main is `gaia supervise --session-dir DIR`, with the spec as JSON on
// stdin. It writes one ready line (a Response) to stdout: ok with the
// agent's pid once the agent runs, or the error, and then exits
// core.ExitStartFailed. After a ready line it returns when the session has
// ended, 0 if the end was recorded and core.ExitInternal if not.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("gaia supervise", flag.ContinueOnError)
	fl.SetOutput(stderr)
	dir := fl.String("session-dir", "", "the session directory, created at 0700")
	if err := fl.Parse(args); err != nil {
		return exitUsage
	}
	if *dir == "" || fl.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: gaia supervise --session-dir DIR < spec.json")
		return exitUsage
	}
	if !supported {
		return refuse(stdout, stderr, core.NewError(core.CodeUnavailable, "the session supervisor is not supported on this platform yet"))
	}
	var spec Spec
	dec := json.NewDecoder(io.LimitReader(stdin, maxSpec))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return refuse(stdout, stderr, core.NewError(core.CodeInvalidArgument, "reading the spec from stdin: "+err.Error()))
	}
	s, err := prepare(*dir, spec, stderr)
	if err != nil {
		return refuse(stdout, stderr, err)
	}
	// The agent runs: from here a lost starter or a stop signal must not
	// end the supervisor, so the signals are handled before the ready line.
	// Doing it after the agent started keeps SIGHUP and SIGPIPE at their
	// defaults in the agent.
	sigs := s.handleSignals()
	if err := writeLine(stdout, Response{OK: true, PID: s.child.pid}); err != nil {
		// The starter is gone; the session goes on without it.
		s.logf("writing the ready line: %v", err)
	}
	return s.run(sigs)
}

func refuse(stdout, stderr io.Writer, err error) int {
	// A starter that has gone away must not turn the refusal into SIGPIPE.
	ignoreHangup()
	ce := asCoreError(err)
	fmt.Fprintf(stderr, "gaia supervise: %v\n", ce)
	_ = writeLine(stdout, Response{Error: ce})
	return int(core.ExitStartFailed)
}

func asCoreError(err error) *core.Error {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce
	}
	return core.NewError(core.CodeInternal, err.Error())
}

func writeLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// supervisor is one running session.
type supervisor struct {
	dir    string
	spec   Spec
	child  *child
	ln     net.Listener
	scroll *os.File
	red    *Redactor
	stderr io.Writer

	mu        sync.Mutex
	state     core.SupervisorState
	size      int64
	outClosed bool
	ended     bool
	stopping  bool
	notify    chan struct{} // closed and replaced on every change

	inMu     sync.Mutex
	inClosed bool

	stopOnce  sync.Once
	childDone chan struct{}
	endedCh   chan struct{}
}

// prepare makes the session directory, binds the socket, starts the agent
// and writes the first state. On error nothing it created is left behind
// and no agent runs.
func prepare(dir string, spec Spec, stderr io.Writer) (_ *supervisor, err error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	sockPath := filepath.Join(dir, SocketFile)
	if len(sockPath) > maxSocketPath() {
		return nil, &core.Error{
			Code:    core.CodeInvalidArgument,
			Message: fmt.Sprintf("socket path %s is %d bytes, longer than the %d this platform allows", sockPath, len(sockPath), maxSocketPath()),
			Details: map[string]string{"path": sockPath},
		}
	}
	created, err := makeSessionDir(dir)
	if err != nil {
		return nil, err
	}
	var cleanup []func()
	defer func() {
		if err != nil {
			for i := len(cleanup) - 1; i >= 0; i-- {
				cleanup[i]()
			}
			if created {
				_ = os.Remove(dir)
			}
		}
	}()
	scrollPath := filepath.Join(dir, ScrollbackFile)
	scroll, err := os.OpenFile(scrollPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND|oNoFollow, 0o600)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = scroll.Close(); _ = os.Remove(scrollPath) })
	ln, err := listen(sockPath)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = ln.Close(); _ = os.Remove(sockPath) })
	c, err := startChild(spec)
	if err != nil {
		return nil, err
	}
	st := core.SupervisorState{
		Version:   core.SupervisorStateVersion,
		SessionID: spec.SessionID,
		PID:       c.pid,
		Mode:      spec.Mode,
		Argv:      redactArgv(spec.Argv, spec.Secrets),
		StartedAt: time.Now().UTC().Round(0),
	}
	if err = WriteState(dir, st); err != nil {
		c.kill()
		c.wait()
		_ = c.out.Close()
		_ = os.Remove(filepath.Join(dir, StateFile))
		return nil, err
	}
	return &supervisor{
		dir:       dir,
		spec:      spec,
		child:     c,
		ln:        ln,
		scroll:    scroll,
		red:       NewRedactor(spec.Secrets),
		stderr:    stderr,
		state:     st,
		notify:    make(chan struct{}),
		childDone: make(chan struct{}),
		endedCh:   make(chan struct{}),
	}, nil
}

// redactArgv returns a copy of argv with every known secret replaced by
// core.Redacted, for the state file and status. The agent runs the
// original argv.
func redactArgv(argv, secrets []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		r := NewRedactor(secrets)
		out[i] = string(append(r.Write([]byte(a)), r.Flush()...))
	}
	return out
}

// makeSessionDir creates dir at 0700, or accepts an existing empty
// directory owned by this user and makes it 0700. created reports whether
// it made dir.
func makeSessionDir(dir string) (created bool, err error) {
	err = os.Mkdir(dir, 0o700)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return false, err
	}
	bad := func(reason string) error {
		return &core.Error{
			Code:    core.CodeInvalidArgument,
			Message: fmt.Sprintf("session directory %s %s", dir, reason),
			Details: map[string]string{"path": dir},
		}
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, bad("exists and is not a directory")
	}
	if uid, ok := fileUID(fi); ok && uid != os.Getuid() {
		return false, bad("is owned by another user")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	if len(entries) != 0 {
		return false, bad("is not empty")
	}
	return false, os.Chmod(dir, 0o700)
}

// listen binds a unix socket at path and makes it 0600. The session
// directory is 0700, so no other user can reach it before the chmod.
func listen(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func (s *supervisor) logf(format string, args ...any) {
	fmt.Fprintf(s.stderr, "gaia supervise %s: %s\n", s.spec.SessionID, fmt.Sprintf(format, args...))
}

// handleSignals makes the supervisor immune to SIGHUP and SIGPIPE and
// starts delivering stop signals on the returned channel.
func (s *supervisor) handleSignals() chan os.Signal {
	ignoreHangup()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, stopSignals...)
	return sigs
}

// run serves the session until the agent has exited and the end is
// recorded. sigs is from handleSignals.
func (s *supervisor) run(sigs chan os.Signal) int {
	defer signal.Stop(sigs)
	go func() {
		for {
			select {
			case <-sigs:
				s.stop()
			case <-s.endedCh:
				return
			}
		}
	}()

	readerDone := make(chan struct{})
	go s.readOutput(readerDone)
	var conns sync.WaitGroup
	serveDone := make(chan struct{})
	go s.serve(&conns, serveDone)

	code, sig := s.child.wait()
	close(s.childDone)
	// The session ends with its agent: nothing it left in its process
	// group goes on writing into the session's output.
	s.child.kill()
	select {
	case <-readerDone:
	case <-time.After(drainTimeout):
		s.logf("the agent's output did not close within %s of its exit", drainTimeout)
	}
	s.finishOutput()
	exit := 0
	if err := s.finishState(code, sig); err != nil {
		s.logf("recording the end: %v", err)
		exit = int(core.ExitInternal)
	}

	_ = s.ln.Close() // removes the socket
	_ = os.Remove(filepath.Join(s.dir, SocketFile))
	<-serveDone
	done := make(chan struct{})
	go func() { conns.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeTimeout):
	}
	return exit
}

// readOutput copies the agent's output through the redactor into the
// scrollback until the output closes.
func (s *supervisor) readOutput(done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, 32<<10)
	for {
		n, err := s.child.out.Read(buf)
		if n > 0 {
			s.appendOutput(s.red.Write, buf[:n])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, fs.ErrClosed) && !isEndOfOutput(err) {
				s.logf("reading the agent's output: %v", err)
			}
			return
		}
	}
}

// appendOutput passes p through redact and appends the result to the
// scrollback. Output arriving after finishOutput is dropped.
func (s *supervisor) appendOutput(redact func([]byte) []byte, p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outClosed {
		return
	}
	b := redact(p)
	if len(b) == 0 {
		return
	}
	n, err := s.scroll.Write(b)
	s.size += int64(n)
	if err != nil {
		s.logf("writing the scrollback: %v", err)
	}
	s.broadcast()
}

// finishOutput writes the redactor's held tail and closes the scrollback
// and the agent's terminal.
func (s *supervisor) finishOutput() {
	s.appendOutput(func([]byte) []byte { return s.red.Flush() }, nil)
	s.mu.Lock()
	s.outClosed = true
	if err := s.scroll.Sync(); err != nil {
		s.logf("syncing the scrollback: %v", err)
	}
	_ = s.scroll.Close()
	s.mu.Unlock()

	s.inMu.Lock()
	s.inClosed = true
	s.inMu.Unlock()
	_ = s.child.out.Close()
}

// finishState records the agent's end in the state file, then marks the
// session ended for clients.
func (s *supervisor) finishState(code *int, sig string) error {
	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	ended := time.Now().UTC().Round(0)
	if ended.Before(st.StartedAt) { // the clock went back
		ended = st.StartedAt
	}
	st.ExitCode, st.Signal, st.EndedAt = code, sig, &ended
	err := WriteState(s.dir, st)
	s.mu.Lock()
	s.state = st
	s.ended = true
	s.broadcast()
	s.mu.Unlock()
	close(s.endedCh)
	return err
}

// broadcast wakes every stream. Callers hold mu.
func (s *supervisor) broadcast() {
	close(s.notify)
	s.notify = make(chan struct{})
}

// stop sends SIGTERM to the agent's group, then SIGKILL after the grace
// period if the agent is still alive. Calls after the first do nothing.
func (s *supervisor) stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopping = true
		s.mu.Unlock()
		select {
		case <-s.childDone:
			return
		default:
		}
		s.child.terminate()
		go func() {
			t := time.NewTimer(s.spec.grace())
			defer t.Stop()
			select {
			case <-s.childDone:
			case <-t.C:
				s.child.kill()
			}
		}()
	})
}

func (s *supervisor) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{State: s.state, Size: s.size, Stopping: s.stopping}
}

func (s *supervisor) serve(conns *sync.WaitGroup, done chan<- struct{}) {
	defer close(done)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		conns.Go(func() {
			defer conn.Close()
			s.handle(conn)
		})
	}
}

func (s *supervisor) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(requestTimeout))
	line, err := bufio.NewReader(io.LimitReader(conn, maxRequestLine)).ReadBytes('\n')
	if err != nil {
		_ = writeLine(conn, Response{Error: core.NewError(core.CodeInvalidArgument, "reading the request line: "+err.Error())})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		_ = writeLine(conn, Response{Error: core.NewError(core.CodeInvalidArgument, "decoding the request: "+err.Error())})
		return
	}
	reply := func(err error) {
		if err != nil {
			_ = writeLine(conn, Response{Error: asCoreError(err)})
			return
		}
		_ = writeLine(conn, Response{OK: true})
	}
	switch req.Op {
	case OpStatus:
		st := s.status()
		_ = writeLine(conn, Response{OK: true, Status: &st})
	case OpStop:
		s.stop()
		<-s.endedCh
		st := s.status()
		_ = writeLine(conn, Response{OK: true, Status: &st})
	case OpInput:
		reply(s.input(req.Data))
	case OpResize:
		reply(s.resize(req.Rows, req.Cols))
	case OpStream:
		s.stream(conn, req.Offset)
	default:
		reply(core.NewError(core.CodeInvalidArgument, fmt.Sprintf("unknown op %q", req.Op)))
	}
}

func noTerminal(op string) error {
	return core.NewError(core.CodeInvalidArgument, "a headless session has no terminal: "+op+" needs an interactive session")
}

func (s *supervisor) endedErr() error {
	return core.NewError(core.CodeConflict, "the session has ended")
}

func (s *supervisor) input(data []byte) error {
	if s.child.tty == nil {
		return noTerminal("input")
	}
	s.inMu.Lock()
	closed := s.inClosed
	s.inMu.Unlock()
	if closed {
		return s.endedErr()
	}
	if _, err := s.child.tty.Write(data); err != nil {
		return core.NewError(core.CodeConflict, "writing to the agent's terminal: "+err.Error())
	}
	return nil
}

func (s *supervisor) resize(rows, cols uint16) error {
	if s.child.tty == nil {
		return noTerminal("resize")
	}
	if rows == 0 || cols == 0 {
		return core.NewError(core.CodeInvalidArgument, fmt.Sprintf("terminal size %dx%d has a zero dimension", rows, cols))
	}
	s.inMu.Lock()
	defer s.inMu.Unlock()
	if s.inClosed {
		return s.endedErr()
	}
	if err := s.child.resize(rows, cols); err != nil {
		return core.NewError(core.CodeConflict, "resizing the agent's terminal: "+err.Error())
	}
	return nil
}

// stream sends the header {"ok":true,"offset":N}, then the raw scrollback
// bytes from offset, live, and closes the connection once the session has
// ended and every byte is sent.
func (s *supervisor) stream(conn net.Conn, offset int64) {
	s.mu.Lock()
	size := s.size
	s.mu.Unlock()
	if offset < 0 || offset > size {
		_ = writeLine(conn, Response{Error: &core.Error{
			Code:    core.CodeInvalidArgument,
			Message: fmt.Sprintf("offset %d is outside the scrollback (0 to %d)", offset, size),
			Details: map[string]string{"offset": fmt.Sprint(offset), "size": fmt.Sprint(size)},
		}})
		return
	}
	f, err := os.Open(filepath.Join(s.dir, ScrollbackFile))
	if err != nil {
		_ = writeLine(conn, Response{Error: core.NewError(core.CodeInternal, "opening the scrollback: "+err.Error())})
		return
	}
	defer f.Close()
	if err := writeLine(conn, streamHeader{OK: true, Offset: offset}); err != nil {
		return
	}
	buf := make([]byte, maxChunk)
	for {
		s.mu.Lock()
		size, ended, changed := s.size, s.ended, s.notify
		s.mu.Unlock()
		for offset < size {
			n := min(int64(maxChunk), size-offset)
			m, err := f.ReadAt(buf[:n], offset)
			if m > 0 {
				if _, werr := conn.Write(buf[:m]); werr != nil {
					return
				}
				offset += int64(m)
			}
			if err != nil && m == 0 {
				s.logf("reading the scrollback at %d: %v", offset, err)
				return
			}
		}
		if ended {
			return // the caller closes the connection
		}
		<-changed
	}
}
