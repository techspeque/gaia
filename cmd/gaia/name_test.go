package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/techspeque/gaia/internal/core"
	gnet "github.com/techspeque/gaia/internal/net"
)

func TestDefaultName(t *testing.T) {
	long := strings.Repeat("a", 57) + "-bcdef" // 63 characters
	for _, c := range []struct{ machine, role, want string }{
		{"studio", "host", "gaia-studio"},
		{"studio", "node", "gaia-studio"},
		{"", "host", "gaia-host"},
		{"", "node", "gaia-node"},
		// Cut to 63 characters, and the '-' the cut leaves at the end trimmed.
		{long, "node", "gaia-" + strings.Repeat("a", 57)},
	} {
		got := defaultName(c.machine, c.role)
		if got != c.want {
			t.Errorf("defaultName(%q, %q) = %q, want %q", c.machine, c.role, got, c.want)
		}
		if err := core.ValidNodeName(got); err != nil {
			t.Errorf("defaultName(%q) = %q is not a valid name: %v", c.machine, got, err)
		}
	}
}

func TestResolveName(t *testing.T) {
	write := func(t *testing.T, dir, file, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		flag  string
		role  string
		want  string
		err   string
	}{
		{name: "fresh, missing dir", role: "host", want: "gaia-studio"},
		{name: "fresh, empty dir", setup: func(t *testing.T, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}, role: "node", want: "gaia-studio"},
		{name: "flag wins", setup: func(t *testing.T, dir string) { write(t, dir, nameFile, "recorded\n") },
			flag: "chosen", role: "node", want: "chosen"},
		{name: "recorded", setup: func(t *testing.T, dir string) { write(t, dir, nameFile, "gaia-old-box\n") },
			role: "node", want: "gaia-old-box"},
		{name: "joined before names were recorded", setup: func(t *testing.T, dir string) {
			write(t, dir, "tsnet/tailscaled.state", "{}")
		}, role: "node", want: "studio"},
		{name: "host registry from before", setup: func(t *testing.T, dir string) { write(t, dir, "gaia.db", "") },
			role: "host", want: "studio"},
		{name: "never joined, incidental file", setup: func(t *testing.T, dir string) { write(t, dir, "authkey", "tskey-auth-x\n") },
			role: "host", want: "gaia-studio"},
		{name: "never joined node, incidental file", setup: func(t *testing.T, dir string) { write(t, dir, "authkey", "tskey-auth-x\n") },
			role: "node", want: "gaia-studio"},
		{name: "empty tsnet dir", setup: func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "tsnet"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, role: "node", want: "gaia-studio"},
		{name: "node ignores a registry", setup: func(t *testing.T, dir string) { write(t, dir, "gaia.db", "") },
			role: "node", want: "gaia-studio"},
		{name: "bad recorded name", setup: func(t *testing.T, dir string) { write(t, dir, nameFile, "Bad_Name\n") },
			role: "host", err: "invalid node name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if c.setup != nil {
				c.setup(t, dir)
			}
			got, err := resolveName(c.flag, dir, "studio", c.role)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err %v, want %q", err, c.err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("resolveName = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestRecordName(t *testing.T) {
	dir := t.TempDir()
	if err := recordName(dir, "gaia-studio"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, nameFile)
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %s, want 0600", fi.Mode().Perm())
	}
	if got, err := resolveName("", dir, "other", "node"); err != nil || got != "gaia-studio" {
		t.Errorf("after recording: %q, %v", got, err)
	}
	// A new name replaces the recorded one.
	if err := recordName(dir, "gaia-desk"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != "gaia-desk\n" {
		t.Errorf("file holds %q", b)
	}
}

type fakeSelf struct {
	name gnet.SelfName
	err  error
}

func (f fakeSelf) Self(context.Context) (gnet.SelfName, error) { return f.name, f.err }

func TestReportTailnetName(t *testing.T) {
	ts := func(n string) gnet.SelfName { return gnet.SelfName{Name: n, DNSName: n + ".tail.ts.net"} }
	for _, c := range []struct {
		name       string
		self       fakeSelf
		role       string
		want       string
		warn, hint string
	}{
		{"as asked", fakeSelf{name: ts("gaia-studio")}, "host", "gaia-studio", "", ""},
		{"suffixed host", fakeSelf{name: ts("gaia-studio-1")}, "host", "gaia-studio-1", "another tailnet name", "--host gaia-studio-1"},
		{"suffixed node", fakeSelf{name: ts("gaia-studio-1")}, "node", "gaia-studio-1", "another tailnet name", "node:gaia-studio-1"},
		{"no MagicDNS", fakeSelf{}, "node", "", "", ""},
		{"status fails", fakeSelf{err: errors.New("down")}, "node", "", "cannot read", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))
			got := reportTailnetName(context.Background(), c.self, "gaia-studio", c.role, log)
			if got != c.want {
				t.Errorf("returned %q, want %q", got, c.want)
			}
			out := buf.String()
			if c.self.err == nil && !logged(out, `"msg":"tailnet name assigned"`, `"tailnet_name":"`+c.want+`"`) {
				t.Errorf("no info line with the name:\n%s", out)
			}
			hasWarn := strings.Contains(out, `"level":"WARN"`)
			if (c.warn != "") != hasWarn || !strings.Contains(out, c.warn) || !strings.Contains(out, c.hint) {
				t.Errorf("want warning %q with %q:\n%s", c.warn, c.hint, out)
			}
		})
	}
}

func TestRefusedHint(t *testing.T) {
	if refusedHint(nil, "h", "h.ts.net") != nil {
		t.Error("nil error gained a hint")
	}
	other := errors.New("i/o timeout")
	if refusedHint(other, "h", "h.ts.net") != other {
		t.Error("other error changed")
	}
	for _, refused := range []error{
		errors.New("dial tcp 100.64.0.1:443: connect: connection refused"),
		// What a tsnet dial to a peer with port 443 closed returns.
		errors.New("connect tcp 100.64.0.1:443: connection was refused"),
	} {
		err := refusedHint(refused, "studio", "studio.tail.ts.net")
		if !errors.Is(err, refused) || !strings.Contains(err.Error(), "--host studio is studio.tail.ts.net") ||
			!strings.Contains(err.Error(), "gaia-<machine name>") {
			t.Errorf("hint for %q: %v", refused, err)
		}
	}
}
