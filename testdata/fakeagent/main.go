//go:build linux || darwin

// Command fakeagent stands in for an agent CLI in tests. It runs the steps
// given as arguments, in order:
//
//	print:TEXT     write TEXT and a newline
//	write:TEXT     write TEXT in one write, no newline
//	stream:N:DUR   write "line 1" … "line N", one per DUR
//	sleep:DUR      sleep (a Go duration)
//	ignore-term    ignore SIGTERM from now on
//	tty            write "tty=yes" or "tty=no" for stdin, then for stdout
//	size           write "size=ROWSxCOLS" of the terminal on stdin
//	wait-winch     wait (up to 10 s) for SIGWINCH, then do "size"
//	echo           read one line from stdin, write "got:LINE"
//	pwd            write the working directory
//	env:NAME       write "NAME=VALUE", or "NAME unset"
//	envsplit:NAME  write "NAME=" and the first half of the value, pause 200 ms,
//	               then write the second half and a newline: two writes
//	args           write each argument after "args" on its own line, quoted, and stop
//	spawn          start a grandchild that ignores SIGTERM and sleeps an hour, in
//	               this process group, and write "grandchild=PID"
//	kill:SIG       send signal SIG (e.g. KILL) to itself
//	exit:N         exit with code N
//
// "\n" in TEXT is a newline. With no exit step it exits 0.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	var winch chan os.Signal
	in := bufio.NewReader(os.Stdin)
	args := os.Args[1:]
	for i, step := range args {
		name, arg, _ := strings.Cut(step, ":")
		switch name {
		case "print":
			out(unescape(arg) + "\n")
		case "write":
			out(unescape(arg))
		case "stream":
			n, d, _ := strings.Cut(arg, ":")
			count := atoi(n)
			for j := 1; j <= count; j++ {
				out(fmt.Sprintf("line %d\n", j))
				time.Sleep(duration(d))
			}
		case "sleep":
			time.Sleep(duration(arg))
		case "ignore-term":
			signal.Ignore(syscall.SIGTERM)
		case "tty":
			out(fmt.Sprintf("tty=%s\ntty=%s\n", isTTY(0), isTTY(1)))
		case "size":
			out(size() + "\n")
		case "wait-winch":
			if winch == nil {
				winch = make(chan os.Signal, 1)
				signal.Notify(winch, syscall.SIGWINCH)
			}
			out("waiting for winch\n")
			select {
			case <-winch:
			case <-time.After(10 * time.Second):
				out("no winch\n")
			}
			out(size() + "\n")
		case "echo":
			line, err := in.ReadString('\n')
			if err != nil {
				fail("echo: %v", err)
			}
			out("got:" + strings.TrimRight(line, "\r\n") + "\n")
		case "pwd":
			wd, err := os.Getwd()
			if err != nil {
				fail("pwd: %v", err)
			}
			out(wd + "\n")
		case "env":
			if v, ok := os.LookupEnv(arg); ok {
				out(arg + "=" + v + "\n")
			} else {
				out(arg + " unset\n")
			}
		case "envsplit":
			v := os.Getenv(arg)
			out(arg + "=" + v[:len(v)/2])
			time.Sleep(200 * time.Millisecond)
			out(v[len(v)/2:] + "\n")
		case "args":
			for _, a := range args[i+1:] {
				out(strconv.Quote(a) + "\n")
			}
			return
		case "spawn":
			self, err := os.Executable()
			if err != nil {
				fail("spawn: %v", err)
			}
			cmd := exec.Command(self, "ignore-term", "sleep:1h")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				fail("spawn: %v", err)
			}
			out(fmt.Sprintf("grandchild=%d\n", cmd.Process.Pid))
		case "kill":
			sig := unix.SignalNum("SIG" + arg)
			if sig == 0 {
				fail("kill: unknown signal %q", arg)
			}
			_ = syscall.Kill(os.Getpid(), sig)
			time.Sleep(10 * time.Second)
			fail("kill: still alive after SIG%s", arg)
		case "exit":
			os.Exit(atoi(arg))
		default:
			fail("unknown step %q", step)
		}
	}
}

func out(s string) {
	if _, err := os.Stdout.WriteString(s); err != nil {
		os.Exit(70)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fakeagent: "+format+"\n", args...)
	os.Exit(70)
}

func unescape(s string) string { return strings.ReplaceAll(s, `\n`, "\n") }

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fail("bad number %q", s)
	}
	return n
}

func duration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		fail("bad duration %q", s)
	}
	return d
}

func isTTY(fd int) string {
	if _, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err == nil {
		return "yes"
	}
	return "no"
}

func size() string {
	ws, err := unix.IoctlGetWinsize(0, unix.TIOCGWINSZ)
	if err != nil {
		return "size=none"
	}
	return fmt.Sprintf("size=%dx%d", ws.Row, ws.Col)
}
