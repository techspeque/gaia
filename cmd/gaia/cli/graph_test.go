package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const modPath = "github.com/techspeque/gaia"

// allowedModulePackages are the only packages of this module a CLI command
// may reach. Everything else of the module (host, node, net, store, …) is
// the daemon roles' and, from workstream 0.6 on, links tsnet.
var allowedModulePackages = map[string]bool{
	modPath + "/cmd/gaia/cli":  true,
	modPath + "/internal/core": true,
	modPath + "/internal/sock": true,
}

// forbiddenDeps returns the packages in deps a CLI command may not reach:
// anything from tailscale.com, and any package of this module not in
// allowedModulePackages.
func forbiddenDeps(deps []string) []string {
	var bad []string
	for _, p := range deps {
		switch {
		case p == "tailscale.com" || strings.HasPrefix(p, "tailscale.com/"):
			bad = append(bad, p)
		case (p == modPath || strings.HasPrefix(p, modPath+"/")) && !allowedModulePackages[p]:
			bad = append(bad, p)
		}
	}
	return bad
}

// TestCLIDoesNotReachTsnet checks the import graph of this package, for
// each daemon platform and Windows: no CLI command can start tsnet
// (ADR-0004 rule 2).
func TestCLIDoesNotReachTsnet(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is needed to read the import graph: %v", err)
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		cmd := exec.Command(gobin, "list", "-deps", modPath+"/cmd/gaia/cli")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "CGO_ENABLED=0")
		out, err := cmd.Output()
		if err != nil {
			var stderr string
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = string(ee.Stderr)
			}
			t.Fatalf("GOOS=%s go list: %v\n%s", goos, err, stderr)
		}
		deps := strings.Fields(string(out))
		if len(deps) == 0 {
			t.Fatalf("GOOS=%s: go list returned no packages", goos)
		}
		if bad := forbiddenDeps(deps); len(bad) > 0 {
			t.Errorf("GOOS=%s: cmd/gaia/cli reaches %v", goos, bad)
		}
	}
}

// TestForbiddenDeps proves the check is not vacuous.
func TestForbiddenDeps(t *testing.T) {
	cases := []struct {
		pkg string
		bad bool
	}{
		{"tailscale.com/tsnet", true},
		{"tailscale.com", true},
		{modPath + "/internal/net", true},
		{modPath + "/internal/host", true},
		{modPath + "/internal/node", true},
		{modPath + "/internal/store", true},
		{modPath + "/cmd/gaia", true},
		{modPath, true},
		{modPath + "/internal/core", false},
		{modPath + "/internal/sock", false},
		{modPath + "/cmd/gaia/cli", false},
		{"net/http", false},
		{"golang.org/x/sys/unix", false},
		{"tailscale.community/x", false},
	}
	for _, c := range cases {
		if got := len(forbiddenDeps([]string{c.pkg})) > 0; got != c.bad {
			t.Errorf("%s: forbidden=%v, want %v", c.pkg, got, c.bad)
		}
	}
}
