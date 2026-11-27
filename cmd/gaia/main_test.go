package main

import (
	"bytes"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, errOut.String())
	}
	got := strings.TrimSpace(out.String())
	for _, want := range []string{"gaia " + version, "(commit ", runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("--version output %q lacks %q", got, want)
		}
	}
}

func TestVersionString(t *testing.T) {
	want := "gaia 1.2.3 (commit abc) " + runtime.GOOS + "/" + runtime.GOARCH
	if got := versionString("1.2.3", "abc"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveCommit(t *testing.T) {
	info := func(settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{Settings: settings}, true }
	}
	none := func() (*debug.BuildInfo, bool) { return nil, false }
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef0123"}

	cases := []struct {
		name    string
		ldflags string
		info    func() (*debug.BuildInfo, bool)
		want    string
	}{
		{"ldflags wins", "deadbeef", info(rev), "deadbeef"},
		{"vcs revision", "", info(rev), "0123456789ab"},
		{"dirty tree", "", info(rev, debug.BuildSetting{Key: "vcs.modified", Value: "true"}), "0123456789ab-dirty"},
		{"no vcs", "", info(), "unknown"},
		{"no build info", "", none, "unknown"},
	}
	for _, c := range cases {
		if got := resolveCommit(c.ldflags, c.info); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Errorf("stderr %q lacks the unknown-command message", errOut.String())
	}
}
