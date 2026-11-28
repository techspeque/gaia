package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/host"
	gnet "github.com/techspeque/gaia/internal/net"
	"github.com/techspeque/gaia/internal/service"
)

const serviceUsage = `usage: gaia service install host [--name NAME] [--node] [--tailnet ...] [--state DIR] ...
       gaia service install node --host NAME [--name NAME] [--state DIR] [--control-url URL]
       gaia service uninstall

install registers the daemon as a service of the current user (a systemd
user unit on Linux, a launchd agent on macOS) and starts it; the role's
flags are those of "gaia host" or "gaia node", except --join and
--auth-key-file. uninstall stops and removes it; the state directory stays.
`

// serviceEnvVars are the variables that decide where the local socket is
// (sock.Dir). The unit carries them as the installing shell has them, so
// the CLI in that shell finds the daemon; it carries no other variable.
var serviceEnvVars = []string{"XDG_RUNTIME_DIR", "TMPDIR"}

// servicePlatform and serviceExecutable are replaced in tests.
var (
	servicePlatform   = hostPlatform
	serviceExecutable = os.Executable
)

// hostPlatform is this machine as internal/service needs it.
func hostPlatform(goos string) (service.Platform, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return service.Platform{}, fmt.Errorf("no home directory: %w", err)
	}
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return service.Platform{
		GOOS:       goos,
		Home:       home,
		ConfigHome: os.Getenv("XDG_CONFIG_HOME"),
		UID:        os.Getuid(),
		User:       name,
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(out), err
		},
	}, nil
}

// runService runs `gaia service install|uninstall`.
func runService(args []string, stdout, stderr io.Writer, goos string) int {
	if goos != "linux" && goos != "darwin" {
		fmt.Fprintf(stderr, "gaia service: not supported on %s yet; Linux (systemd) and macOS (launchd) only\n", goos)
		return 1
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, serviceUsage)
		return 2
	}
	switch args[0] {
	case "install":
		return serviceInstall(args[1:], stdout, stderr, goos)
	case "uninstall":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "gaia service uninstall: unexpected argument %q\n", args[1])
			return 2
		}
		return serviceUninstall(stdout, stderr, goos)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, serviceUsage)
		return 0
	}
	fmt.Fprintf(stderr, "gaia service: unknown command %q\n\n%s", args[0], serviceUsage)
	return 2
}

func serviceInstall(args []string, stdout, stderr io.Writer, goos string) int {
	exe, err := serviceExecutable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gaia service: cannot find this binary: %v\n", err)
		return 1
	}
	sa, code := parseServiceArgs(args, goos, os.Getenv, os.UserHomeDir, stderr)
	if code == helpShown {
		return 0
	}
	if code != 0 {
		return code
	}
	if sa.tailnet && !gnet.HasState(sa.state) {
		join := "gaia host --tailnet ... --state " + sa.state + " with the auth key in $GAIA_AUTHKEY"
		if sa.role == "node" {
			join = "gaia node --host ... --join --state " + sa.state + " with the auth key in $GAIA_AUTHKEY"
		}
		fmt.Fprintf(stderr, "gaia service: %s holds no tailnet state, and the service is given no auth key.\n"+
			"Join the tailnet once by hand first (%s), stop it, then install the service.\n", sa.state, join)
		return 1
	}
	p, err := servicePlatform(goos)
	if err != nil {
		fmt.Fprintf(stderr, "gaia service: %v\n", err)
		return 1
	}
	spec := service.Spec{Args: append([]string{exe, sa.role}, sa.flags...)}
	for _, name := range serviceEnvVars {
		if v := os.Getenv(name); v != "" {
			spec.Env = append(spec.Env, service.EnvVar{Name: name, Value: v})
		} else if goos == "linux" {
			spec.Unset = append(spec.Unset, name)
		}
	}
	res, err := service.Install(context.Background(), p, spec)
	if err != nil {
		if errors.Is(err, service.ErrNotOurs) {
			fmt.Fprintf(stderr, "gaia service: %v; refusing to overwrite it\n", err)
		} else {
			fmt.Fprintf(stderr, "gaia service: %v\n", err)
		}
		return 1
	}
	switch {
	case !res.Changed:
		fmt.Fprintf(stdout, "gaia service: %s is installed and running; nothing changed\n", res.Path)
	case res.Written:
		fmt.Fprintf(stdout, "gaia service: wrote %s and started gaia %s\n", res.Path, sa.role)
	default:
		fmt.Fprintf(stdout, "gaia service: %s unchanged; started gaia %s\n", res.Path, sa.role)
	}
	if res.LogHint != "" && res.Changed {
		fmt.Fprintf(stdout, "logs: %s\n", res.LogHint)
	}
	if res.LingerHint != "" {
		fmt.Fprintf(stdout, "Lingering is off for this user: the service stops at logout and does not start at boot.\n"+
			"To keep it running, run this once yourself:\n  %s\n", res.LingerHint)
	}
	return 0
}

func serviceUninstall(stdout, stderr io.Writer, goos string) int {
	p, err := servicePlatform(goos)
	if err != nil {
		fmt.Fprintf(stderr, "gaia service: %v\n", err)
		return 1
	}
	res, err := service.Uninstall(context.Background(), p)
	if err != nil {
		if errors.Is(err, service.ErrNotOurs) {
			fmt.Fprintf(stderr, "gaia service: %v; refusing to remove it\n", err)
		} else {
			fmt.Fprintf(stderr, "gaia service: %v\n", err)
		}
		return 1
	}
	if !res.Changed {
		fmt.Fprintln(stdout, "gaia service: not installed; nothing changed")
		return 0
	}
	fmt.Fprintf(stdout, "gaia service: stopped the service and removed %s; the state directory is left in place\n", res.Path)
	return 0
}

// serviceArgs is the parsed role of `gaia service install`.
type serviceArgs struct {
	role    string   // "host" or "node"
	flags   []string // the daemon's flags, canonical, --state absolute
	state   string   // absolute state directory
	tailnet bool     // the daemon joins a tailnet, so needs tailnet state
}

// serviceRefusedFlags are daemon flags a service never gets: they carry or
// point at the tailnet auth key, which no unit file may hold.
var serviceRefusedFlags = []string{"join", "auth-key-file"}

// helpShown is parseServiceArgs's code after -h printed the flags.
const helpShown = -1

// parseServiceArgs parses `host …` or `node …` into the daemon arguments of
// the unit. It returns a non-zero exit code, having printed why, when they
// are unusable: 2 for misuse, 1 otherwise, helpShown after -h.
func parseServiceArgs(args []string, goos string, getenv func(string) string, home func() (string, error), stderr io.Writer) (serviceArgs, int) {
	if len(args) == 0 || (args[0] != "host" && args[0] != "node") {
		fmt.Fprint(stderr, serviceUsage)
		return serviceArgs{}, 2
	}
	sa := serviceArgs{role: args[0]}
	fs := flag.NewFlagSet("gaia service install "+sa.role, flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateFlag := fs.String("state", "", "state directory (default: "+describeDefaultState(goos)+")")
	controlURL := fs.String("control-url", "", "tailnet control server URL")
	fs.Bool("join", false, "refused: join once by hand before installing")
	fs.String("auth-key-file", "", "refused: the service is given no auth key")
	var (
		withNode, withTailnet *bool
		allow                 stringList
		allowFile, hostName   *string
		nameFlag              *string
		beat                  *time.Duration
		missed                *int
	)
	if sa.role == "host" {
		nameFlag = fs.String("name", "", "the host's name on the tailnet and of its own node (default: as for gaia host)")
		withNode = fs.Bool("node", false, "also run this machine as a node (local-only mode)")
		withTailnet = fs.Bool("tailnet", false, "join a tailnet and serve the API on it")
		fs.Var(&allow, "allow", "tailnet peer allowed to call the API; repeatable; needs --tailnet")
		allowFile = fs.String("allow-file", "", "file of allowed tailnet peers; needs --tailnet")
		beat = fs.Duration("heartbeat-interval", host.DefaultHeartbeatInterval, "how often nodes heartbeat")
		missed = fs.Int("missed-heartbeats", host.DefaultMissedHeartbeats, "heartbeats a node may miss")
	} else {
		hostName = fs.String("host", "", "the host's tailnet name; required")
		nameFlag = fs.String("name", "", "this node's name (default: as for gaia node)")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return sa, helpShown
		}
		return sa, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gaia service install: unexpected argument %q\n", fs.Arg(0))
		return sa, 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, name := range serviceRefusedFlags {
		if set[name] {
			fmt.Fprintf(stderr, "gaia service install: --%s is not accepted: a service holds no auth key. "+
				"Join the tailnet once by hand (gaia %s ... --join), stop it, then install the service.\n", name, sa.role)
			return sa, 2
		}
	}

	state := *stateFlag
	if state == "" {
		var err error
		if state, err = defaultStateDir(goos, getenv, home); err != nil {
			fmt.Fprintf(stderr, "gaia service install: %v; pass --state DIR\n", err)
			return sa, 1
		}
	}
	abs, err := filepath.Abs(state)
	if err != nil {
		fmt.Fprintf(stderr, "gaia service install: --state: %v\n", err)
		return sa, 1
	}
	sa.state = abs
	sa.flags = []string{"--state=" + abs}
	if *nameFlag != "" {
		if err := core.ValidNodeName(*nameFlag); err != nil {
			fmt.Fprintf(stderr, "gaia service install %s: --name: %s\n", sa.role, err.(*core.Error).Message)
			return sa, 2
		}
	}

	if sa.role == "node" {
		if *hostName == "" {
			fmt.Fprintln(stderr, "gaia service install node: --host NAME is required: the host's tailnet name")
			return sa, 2
		}
		sa.flags = append(sa.flags, "--host="+*hostName)
		if *nameFlag != "" {
			sa.flags = append(sa.flags, "--name="+*nameFlag)
		}
		if *controlURL != "" {
			sa.flags = append(sa.flags, "--control-url="+*controlURL)
		}
		sa.tailnet = true
		return sa, 0
	}

	if *beat < 10*time.Millisecond || *missed < 1 {
		fmt.Fprintln(stderr, "gaia service install host: --heartbeat-interval must be at least 10ms and --missed-heartbeats at least 1")
		return sa, 2
	}
	if !*withTailnet {
		var bad []string
		for _, name := range tailnetOnlyFlags {
			if set[name] {
				bad = append(bad, "--"+name)
			}
		}
		if len(bad) > 0 {
			fmt.Fprintf(stderr, "gaia service install host: %s needs --tailnet\n", strings.Join(bad, ", "))
			return sa, 2
		}
	}
	if *nameFlag != "" {
		sa.flags = append(sa.flags, "--name="+*nameFlag)
	}
	if *withNode {
		sa.flags = append(sa.flags, "--node")
	}
	if *withTailnet {
		sa.tailnet = true
		sa.flags = append(sa.flags, "--tailnet")
		if *controlURL != "" {
			sa.flags = append(sa.flags, "--control-url="+*controlURL)
		}
		a, err := host.ParseAllowlist(allow)
		if err != nil {
			fmt.Fprintf(stderr, "gaia service install host: --allow: %v\n", err)
			return sa, 2
		}
		for _, e := range allow {
			sa.flags = append(sa.flags, "--allow="+e)
		}
		n := a.Len()
		if *allowFile != "" {
			af, err := filepath.Abs(*allowFile)
			if err != nil {
				fmt.Fprintf(stderr, "gaia service install host: --allow-file: %v\n", err)
				return sa, 1
			}
			f, err := os.Open(af)
			if err != nil {
				fmt.Fprintf(stderr, "gaia service install host: --allow-file: %v\n", err)
				return sa, 1
			}
			fa, err := host.ReadAllowlist(f, af)
			_ = f.Close()
			if err != nil {
				fmt.Fprintf(stderr, "gaia service install host: --allow-file: %v\n", err)
				return sa, 1
			}
			n += fa.Len()
			sa.flags = append(sa.flags, "--allow-file="+af)
		}
		if n == 0 {
			fmt.Fprintln(stderr, "gaia service install host: --tailnet needs an allowlist (--allow or --allow-file); an empty one admits no one")
			return sa, 1
		}
	}
	if set["heartbeat-interval"] {
		sa.flags = append(sa.flags, "--heartbeat-interval="+beat.String())
	}
	if set["missed-heartbeats"] {
		sa.flags = append(sa.flags, "--missed-heartbeats="+strconv.Itoa(*missed))
	}
	return sa, 0
}
