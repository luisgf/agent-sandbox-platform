// Command asp-server runs ASP on one host: a control plane and a node, with everything they need
// made for them. One command, no file to edit:
//
//	sudo asp-server
//	asp session start
//
// It keeps its state under --data-dir (default /var/lib/asp): a SQLite database, the keys, a
// self-signed TLS certificate, an administration key and a token for the node. The control plane
// listens on the loopback until told otherwise (--listen), with authentication on. It starts the
// programs the packages install, asp-control-plane and asp-node-agent, which keep reading their
// own settings (/etc/asp/server.yaml, /etc/asp/agent.yaml). To add a host: asp node enroll-token.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/envcfg"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/standalone"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/version"
)

// defaultConfigFile is read when it exists; its drop-ins are in defaultConfigFile + ".d".
const defaultConfigFile = "/etc/asp/standalone.yaml"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// stringList is a flag that can be given several times or with commas.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

// newFlags declares the flags of asp-server over a fresh Options.
func newFlags(stderr io.Writer) (*flag.FlagSet, *standalone.Options, *stringList) {
	fs := flag.NewFlagSet("asp-server", flag.ContinueOnError)
	if stderr != nil {
		fs.SetOutput(stderr)
	}
	opts := &standalone.Options{}
	sans := &stringList{}
	fs.StringVar(&opts.DataDir, "data-dir", standalone.DefaultDataDir, "where everything lives: the database, the keys, the certificate, the disks")
	fs.StringVar(&opts.Listen, "listen", "127.0.0.1:8443", "where the control plane listens (host:port). The loopback by default: 0.0.0.0:8443 lets other hosts join")
	fs.Var(sans, "tls-san", "more names or addresses its TLS certificate must be valid for (comma separated, or repeated)")
	fs.StringVar(&opts.NodeID, "node-id", "", "the id of the node on this host (default: the host name)")
	fs.BoolVar(&opts.NoAgent, "no-agent", false, "run the control plane alone, with no node on this host")
	fs.StringVar(&opts.Profile, "profile", standalone.ProfileDefault, "default, or lab: a node without VMs (--dry-run) for a host with no KVM and for the smokes")
	fs.StringVar(&opts.ControlPlane, "control-plane", "", "the asp-control-plane program (default: beside this one, on the PATH, or where the packages put it)")
	fs.StringVar(&opts.NodeAgent, "node-agent", "", "the asp-node-agent program (same places)")
	fs.StringVar(&opts.User, "user", "asp-control-plane", "the account the control plane runs as when this runs as root")
	fs.StringVar(&opts.Group, "group", "asp", "the group that may read the administration key (if there is one)")
	fs.StringVar(&opts.CLIConfig, "cli-config", "/etc/asp/asp.yaml", `where to leave the configuration of the asp command of this host if it has none ("-" leaves none)`)
	fs.String("config", "", "a settings file (default "+defaultConfigFile+", read when it exists); the keys are these flags with underscores")
	fs.Bool("version", false, "print the version and exit")
	return fs, opts, sans
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, opts, sans := newFlags(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: asp-server [flags]")
		fmt.Fprintln(stderr, "Runs a control plane and a node on this host, with everything they need made for them.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.Lookup("version").Value.String() == "true" {
		fmt.Fprintf(stdout, "asp-server %s\n", version.Get())
		return 0
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "asp-server: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if err := applyConfigFile(fs, fs.Lookup("config").Value.String()); err != nil {
		fmt.Fprintf(stderr, "asp-server: %v\n", err)
		return 2
	}
	opts.TLSSANs = *sans
	if err := opts.Validate(); err != nil {
		fmt.Fprintf(stderr, "asp-server: %v\n", err)
		return 2
	}
	opts.Out = stderr
	if err := standalone.Run(ctx, *opts); err != nil {
		fmt.Fprintf(stderr, "asp-server: %v\n", err)
		return 1
	}
	return 0
}

// applyConfigFile gives the flags the command line did not set the values of the settings file:
// precedence is the flags, then the file, then the defaults. (The environment is not a layer here:
// the programs asp-server starts read it, and a variable meant for one of them must not change
// this.) A key that is no flag is an error that names the nearest one.
func applyConfigFile(fs *flag.FlagSet, named string) error {
	file, known, err := loadConfigFile(fs, named)
	if err != nil {
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for key, value := range file.Values {
		name := known[key]
		if given[name] {
			continue
		}
		if err := fs.Set(name, value); err != nil {
			return fmt.Errorf("%s: %q: %w", file.From[key], key, err)
		}
	}
	return nil
}

// checkKeys reads a settings file and refuses a key that is no flag, without applying it.
func checkKeys(fs *flag.FlagSet, path string) error {
	_, _, err := loadConfigFile(fs, path)
	return err
}

// loadConfigFile reads the settings file named (or the default) and checks its keys against
// the flags; known maps each key to its flag.
func loadConfigFile(fs *flag.FlagSet, named string) (*envcfg.File, map[string]string, error) {
	path, explicit := named, named != ""
	if !explicit {
		path = defaultConfigFile
	}
	file, err := envcfg.LoadFile(path, explicit)
	if err != nil {
		return nil, nil, err
	}
	known := map[string]string{} // key → flag
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name != "config" && f.Name != "version" {
			known[envcfg.Key(f.Name)] = f.Name
		}
	})
	if err := file.Check(known); err != nil {
		return nil, nil, err
	}
	return file, known, nil
}
