package service

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeRunner records every command and fails those listed in fail (by the
// command line joined with spaces), answering out for those in out.
type fakeRunner struct {
	cmds []string
	fail map[string]bool
	out  map[string]string
}

func newFake() *fakeRunner {
	return &fakeRunner{fail: map[string]bool{}, out: map[string]string{}}
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.cmds = append(f.cmds, line)
	if f.fail[line] {
		return f.out[line], errors.New("exit status 1")
	}
	return f.out[line], nil
}

// mutating returns the recorded commands that change state: not status
// queries.
func (f *fakeRunner) mutating() []string {
	var m []string
	for _, c := range f.cmds {
		if strings.Contains(c, " is-enabled ") || strings.Contains(c, " is-active ") ||
			strings.HasPrefix(c, "launchctl print ") || strings.HasPrefix(c, "loginctl show-user ") {
			continue
		}
		m = append(m, c)
	}
	return m
}

func platform(t *testing.T, goos string, f *fakeRunner) Platform {
	t.Helper()
	return Platform{GOOS: goos, Home: t.TempDir(), UID: 501, User: "alice", Run: f.run,
		Sleep: func(time.Duration) {}}
}

var spec = Spec{
	Args: []string{"/opt/gaia dir/gaia", "host", "--node", "--state", "/home/a/st$a%te"},
	Env:  []EnvVar{{"XDG_RUNTIME_DIR", "/run/user/501"}},
}

func TestRenderSystemd(t *testing.T) {
	b, err := Render(Platform{GOOS: "linux"}, Spec{Args: spec.Args, Env: spec.Env, Unset: []string{"TMPDIR"}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"# " + Marker + "\n",
		`ExecStart="/opt/gaia dir/gaia" "host" "--node" "--state" "/home/a/st$$a%%te"` + "\n",
		`Environment="XDG_RUNTIME_DIR=/run/user/501"` + "\n",
		"UnsetEnvironment=TMPDIR\n",
		"Description=Gaia host\n",
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit lacks %q:\n%s", want, got)
		}
	}
}

// Environment= does no variable expansion, so a $ in a socket path is
// written as it is, not doubled as in ExecStart=.
func TestRenderSystemdEnvDollar(t *testing.T) {
	b, err := Render(Platform{GOOS: "linux"}, Spec{Args: spec.Args,
		Env: []EnvVar{{"XDG_RUNTIME_DIR", `/run/a$b%c"d`}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `Environment="XDG_RUNTIME_DIR=/run/a$b%%c\"d"` + "\n"; !strings.Contains(string(b), want) {
		t.Errorf("unit lacks %q:\n%s", want, b)
	}
}

func TestRenderLaunchd(t *testing.T) {
	p := Platform{GOOS: "darwin", Home: "/Users/a"}
	s := spec
	s.Args = append(slices.Clone(s.Args), "--allow", "<a&b>")
	b, err := Render(p, s)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "<!-- "+Marker+" -->") {
		t.Errorf("plist lacks the marker:\n%s", got)
	}
	// Well-formed XML, and the arguments round-trip.
	var doc struct {
		Dict struct {
			Inner []string `xml:"array>string"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("plist is not XML: %v\n%s", err, got)
	}
	if !slices.Equal(doc.Dict.Inner, s.Args) {
		t.Errorf("ProgramArguments = %q, want %q", doc.Dict.Inner, s.Args)
	}
	for _, want := range []string{"<string>" + LaunchdLabel + "</string>", "<key>XDG_RUNTIME_DIR</key>",
		"/Users/a/Library/Logs/gaia/gaia.log", "<key>RunAtLoad</key>"} {
		if !strings.Contains(got, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
}

func TestRenderRefuses(t *testing.T) {
	for name, s := range map[string]Spec{
		"relative binary": {Args: []string{"gaia", "host"}},
		"no args":         {},
		"newline arg":     {Args: []string{"/bin/gaia", "host\nExecStartPre=/bin/evil"}},
		"bad env name":    {Args: []string{"/bin/gaia"}, Env: []EnvVar{{"A=B", "x"}}},
		"newline env":     {Args: []string{"/bin/gaia"}, Env: []EnvVar{{"A", "x\ny"}}},
	} {
		if _, err := Render(Platform{GOOS: "linux"}, s); err == nil {
			t.Errorf("%s: Render accepted it", name)
		}
	}
	if _, err := Render(Platform{GOOS: "windows"}, spec); !errors.Is(err, ErrUnsupported) {
		t.Errorf("windows: err = %v, want ErrUnsupported", err)
	}
}

func TestUnitPath(t *testing.T) {
	for _, c := range []struct {
		p    Platform
		want string
	}{
		{Platform{GOOS: "linux", Home: "/h"}, "/h/.config/systemd/user/gaia.service"},
		{Platform{GOOS: "linux", Home: "/h", ConfigHome: "/c"}, "/c/systemd/user/gaia.service"},
		{Platform{GOOS: "darwin", Home: "/h"}, "/h/Library/LaunchAgents/io.github.techspeque.gaia.plist"},
	} {
		if got, err := UnitPath(c.p); err != nil || got != c.want {
			t.Errorf("UnitPath(%+v) = %q, %v; want %q", c.p, got, err, c.want)
		}
	}
}

func TestInstallSystemdFresh(t *testing.T) {
	f := newFake()
	f.fail["systemctl --user is-enabled --quiet gaia.service"] = true
	f.fail["systemctl --user is-active --quiet gaia.service"] = true
	f.out["loginctl show-user 501 --property=Linger --value"] = "no\n"
	p := platform(t, "linux", f)
	res, err := Install(context.Background(), p, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Written || !res.Changed {
		t.Errorf("result = %+v, want written and changed", res)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable gaia.service",
		"systemctl --user start gaia.service",
	}
	if !slices.Equal(f.mutating(), want) {
		t.Errorf("commands = %q, want %q", f.mutating(), want)
	}
	if res.LingerHint != "loginctl enable-linger alice" {
		t.Errorf("LingerHint = %q", res.LingerHint)
	}
	for _, c := range f.cmds {
		if strings.Contains(c, "enable-linger") {
			t.Errorf("ran %q; the linger command is only printed", c)
		}
	}
	fi, err := os.Stat(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("unit mode = %v, want 0644", fi.Mode().Perm())
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(res.Path), ".gaia-unit-*")); len(m) > 0 {
		t.Errorf("temporary files left: %q", m)
	}
}

func TestInstallLingerOn(t *testing.T) {
	f := newFake()
	f.out["loginctl show-user 501 --property=Linger --value"] = "yes\n"
	res, err := Install(context.Background(), platform(t, "linux", f), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.LingerHint != "" {
		t.Errorf("LingerHint = %q with lingering on", res.LingerHint)
	}
}

// A second install with the service running writes nothing and runs no
// state-changing command.
func TestInstallTwiceChangesNothing(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newFake()
			p := platform(t, goos, f)
			first, err := Install(context.Background(), p, spec)
			if err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(first.Path, old, old); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(first.Path)
			f.cmds = nil
			res, err := Install(context.Background(), p, spec) // everything reports running
			if err != nil {
				t.Fatal(err)
			}
			if res.Written || res.Changed {
				t.Errorf("second install result = %+v, want nothing changed", res)
			}
			if m := f.mutating(); len(m) > 0 {
				t.Errorf("second install ran %q", m)
			}
			fi, _ := os.Stat(first.Path)
			after, _ := os.ReadFile(first.Path)
			if !fi.ModTime().Equal(old) || string(after) != string(before) {
				t.Errorf("second install rewrote the unit")
			}
		})
	}
}

func TestInstallUnchangedButStopped(t *testing.T) {
	f := newFake()
	p := platform(t, "linux", f)
	if _, err := Install(context.Background(), p, spec); err != nil {
		t.Fatal(err)
	}
	f.cmds = nil
	f.fail["systemctl --user is-active --quiet gaia.service"] = true
	res, err := Install(context.Background(), p, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Written || !res.Changed {
		t.Errorf("result = %+v, want not written but changed", res)
	}
	if want := []string{"systemctl --user start gaia.service"}; !slices.Equal(f.mutating(), want) {
		t.Errorf("commands = %q, want %q", f.mutating(), want)
	}
}

func TestInstallChangedRestarts(t *testing.T) {
	for goos, want := range map[string][]string{
		"linux": {"systemctl --user daemon-reload", "systemctl --user restart gaia.service"},
		"darwin": {"launchctl bootout gui/501/" + LaunchdLabel, "launchctl enable gui/501/" + LaunchdLabel,
			"launchctl bootstrap gui/501 PATH"},
	} {
		t.Run(goos, func(t *testing.T) {
			f := newFake()
			p := platform(t, goos, f)
			if _, err := Install(context.Background(), p, spec); err != nil {
				t.Fatal(err)
			}
			f.cmds = nil
			s := spec
			s.Args = []string{"/opt/gaia", "node", "--host", "h"}
			res, err := Install(context.Background(), p, s)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Written {
				t.Errorf("changed spec not written")
			}
			for i := range want {
				want[i] = strings.ReplaceAll(want[i], "PATH", res.Path)
			}
			if !slices.Equal(f.mutating(), want) {
				t.Errorf("commands = %q, want %q", f.mutating(), want)
			}
			b, _ := os.ReadFile(res.Path)
			if !strings.Contains(string(b), "node") {
				t.Errorf("unit not rewritten:\n%s", b)
			}
		})
	}
}

// An update whose apply step fails leaves the previous unit on disk, so a
// retry applies the new one instead of reporting nothing changed.
func TestInstallFailedUpdateRetried(t *testing.T) {
	for goos, c := range map[string]struct {
		fail  string
		retry []string
	}{
		"linux": {"systemctl --user daemon-reload",
			[]string{"systemctl --user daemon-reload", "systemctl --user restart gaia.service"}},
		"darwin": {"launchctl bootout gui/501/" + LaunchdLabel,
			[]string{"launchctl bootout gui/501/" + LaunchdLabel, "launchctl enable gui/501/" + LaunchdLabel,
				"launchctl bootstrap gui/501 PATH"}},
	} {
		t.Run(goos, func(t *testing.T) {
			f := newFake()
			p := platform(t, goos, f)
			first, err := Install(context.Background(), p, spec)
			if err != nil {
				t.Fatal(err)
			}
			old, _ := os.ReadFile(first.Path)
			s := spec
			s.Args = []string{"/opt/gaia", "node", "--host", "h"}
			f.fail[c.fail] = true
			res, err := Install(context.Background(), p, s)
			if err == nil {
				t.Fatal("install succeeded with a failing apply step")
			}
			if res.Written {
				t.Errorf("result = %+v, want not written after restore", res)
			}
			if b, _ := os.ReadFile(first.Path); string(b) != string(old) {
				t.Errorf("previous unit not restored:\n%s", b)
			}
			delete(f.fail, c.fail)
			f.cmds = nil
			res, err = Install(context.Background(), p, s)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Written || !res.Changed {
				t.Errorf("retry result = %+v, want written and changed", res)
			}
			for i := range c.retry {
				c.retry[i] = strings.ReplaceAll(c.retry[i], "PATH", res.Path)
			}
			if !slices.Equal(f.mutating(), c.retry) {
				t.Errorf("retry commands = %q, want %q", f.mutating(), c.retry)
			}
			if b, _ := os.ReadFile(res.Path); !strings.Contains(string(b), "node") {
				t.Errorf("retry did not write the new unit:\n%s", b)
			}
		})
	}
}

// A fresh install whose apply step fails leaves no unit behind.
func TestInstallFailedFreshRemoved(t *testing.T) {
	f := newFake()
	f.fail["launchctl print gui/501/"+LaunchdLabel] = true
	p := platform(t, "darwin", f)
	path, _ := UnitPath(p)
	f.fail["launchctl bootstrap gui/501 "+path] = true
	if _, err := Install(context.Background(), p, spec); err == nil {
		t.Fatal("install succeeded with a failing bootstrap")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit left after a failed fresh install: %v", err)
	}
}

func TestInstallLaunchdFresh(t *testing.T) {
	f := newFake()
	f.fail["launchctl print gui/501/"+LaunchdLabel] = true
	p := platform(t, "darwin", f)
	res, err := Install(context.Background(), p, spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"launchctl enable gui/501/" + LaunchdLabel, "launchctl bootstrap gui/501 " + res.Path}
	if !slices.Equal(f.mutating(), want) {
		t.Errorf("commands = %q, want %q", f.mutating(), want)
	}
	if fi, err := os.Stat(filepath.Join(p.Home, "Library", "Logs", "gaia")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("log dir: %v %v", fi, err)
	}
}

func TestBootstrapRetriedAfterBootout(t *testing.T) {
	f := newFake()
	p := platform(t, "darwin", f)
	if _, err := Install(context.Background(), p, spec); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "launchctl" && args[0] == "bootstrap" {
			calls++
			if calls < 3 {
				return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
			}
		}
		return f.run(ctx, name, args...)
	}
	s := spec
	s.Args = []string{"/opt/gaia", "host"}
	if _, err := Install(context.Background(), p, s); err != nil {
		t.Fatalf("install: %v", err)
	}
	if calls != 3 {
		t.Errorf("bootstrap tried %d times, want 3", calls)
	}
}

func TestCommandFailureSurfaced(t *testing.T) {
	f := newFake()
	f.fail["systemctl --user is-active --quiet gaia.service"] = true
	f.fail["systemctl --user start gaia.service"] = true
	f.out["systemctl --user start gaia.service"] = "Failed to connect to bus"
	_, err := Install(context.Background(), platform(t, "linux", f), spec)
	if err == nil || !strings.Contains(err.Error(), "systemctl --user start gaia.service") ||
		!strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("err = %v, want the command and its output", err)
	}
}

func TestForeignUnitUntouched(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newFake()
			p := platform(t, goos, f)
			path, _ := UnitPath(p)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			const foreign = "[Service]\nExecStart=/usr/bin/something\n"
			if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Install(context.Background(), p, spec); !errors.Is(err, ErrNotOurs) {
				t.Errorf("install: err = %v, want ErrNotOurs", err)
			}
			if _, err := Uninstall(context.Background(), p); !errors.Is(err, ErrNotOurs) {
				t.Errorf("uninstall: err = %v, want ErrNotOurs", err)
			}
			if b, _ := os.ReadFile(path); string(b) != foreign {
				t.Errorf("foreign unit changed to:\n%s", b)
			}
			if len(f.cmds) > 0 {
				t.Errorf("ran %q against a foreign unit", f.cmds)
			}
		})
	}
}

func TestSymlinkUnitUntouched(t *testing.T) {
	f := newFake()
	p := platform(t, "linux", f)
	path, _ := UnitPath(p)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	target := filepath.Join(p.Home, "elsewhere")
	if err := os.WriteFile(target, []byte("# "+Marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), p, spec); !errors.Is(err, ErrNotOurs) {
		t.Errorf("install: err = %v, want ErrNotOurs", err)
	}
	if _, err := Uninstall(context.Background(), p); !errors.Is(err, ErrNotOurs) {
		t.Errorf("uninstall: err = %v, want ErrNotOurs", err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink replaced: %v %v", fi, err)
	}
}

func TestUninstall(t *testing.T) {
	for goos, want := range map[string][]string{
		"linux":  {"systemctl --user disable --now gaia.service", "systemctl --user daemon-reload"},
		"darwin": {"launchctl bootout gui/501/" + LaunchdLabel},
	} {
		t.Run(goos, func(t *testing.T) {
			f := newFake()
			p := platform(t, goos, f)
			stateDir := filepath.Join(p.Home, "state")
			_ = os.MkdirAll(stateDir, 0o700)
			_ = os.WriteFile(filepath.Join(stateDir, "gaia.db"), []byte("x"), 0o600)
			res, err := Install(context.Background(), p, spec)
			if err != nil {
				t.Fatal(err)
			}
			f.cmds = nil
			res, err = Uninstall(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Changed {
				t.Errorf("uninstall reported nothing changed")
			}
			if !slices.Equal(f.mutating(), want) {
				t.Errorf("commands = %q, want %q", f.mutating(), want)
			}
			if _, err := os.Lstat(res.Path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("unit still there: %v", err)
			}
			if _, err := os.Stat(filepath.Join(stateDir, "gaia.db")); err != nil {
				t.Errorf("state directory touched: %v", err)
			}
			// Twice: nothing to do.
			f.cmds = nil
			res, err = Uninstall(context.Background(), p)
			if err != nil || res.Changed || len(f.cmds) > 0 {
				t.Errorf("second uninstall: %+v %v, ran %q", res, err, f.cmds)
			}
		})
	}
}

func TestUninstallLaunchdNotLoaded(t *testing.T) {
	f := newFake()
	p := platform(t, "darwin", f)
	if _, err := Install(context.Background(), p, spec); err != nil {
		t.Fatal(err)
	}
	f.cmds = nil
	f.fail["launchctl print gui/501/"+LaunchdLabel] = true
	if _, err := Uninstall(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if m := f.mutating(); len(m) > 0 {
		t.Errorf("ran %q for an unloaded agent", m)
	}
}

func TestUnsupportedOS(t *testing.T) {
	p := Platform{GOOS: "windows", Run: newFake().run}
	if _, err := Install(context.Background(), p, spec); !errors.Is(err, ErrUnsupported) {
		t.Errorf("install: %v", err)
	}
	if _, err := Uninstall(context.Background(), p); !errors.Is(err, ErrUnsupported) {
		t.Errorf("uninstall: %v", err)
	}
}

// Only systemctl --user, loginctl show-user and launchctl are run; nothing
// in this package runs sudo or enables lingering.
func TestOnlyUserCommands(t *testing.T) {
	src, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range src {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "sudo") {
			t.Errorf("%s mentions sudo", file)
		}
	}
	for _, goos := range []string{"linux", "darwin"} {
		f := newFake()
		f.fail["systemctl --user is-active --quiet gaia.service"] = true
		f.fail["launchctl print gui/501/"+LaunchdLabel] = true
		p := platform(t, goos, f)
		if _, err := Install(context.Background(), p, spec); err != nil {
			t.Fatal(err)
		}
		f.fail = map[string]bool{}
		if _, err := Uninstall(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		for _, c := range f.cmds {
			ok := strings.HasPrefix(c, "systemctl --user ") || strings.HasPrefix(c, "launchctl ") ||
				strings.HasPrefix(c, "loginctl show-user ")
			if !ok {
				t.Errorf("%s: ran %q", goos, c)
			}
		}
	}
}
