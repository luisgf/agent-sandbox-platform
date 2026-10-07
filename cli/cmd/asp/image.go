package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/imagepull"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/version"
)

// Where the guest files go on a node, and where the node-agent looks for them by default.
const (
	defaultImageDir = "/var/lib/asp/images"
	defaultLinkDir  = "/opt/sandbox"
	releasesURL     = "https://github.com/luisgf/agent-sandbox-platform/releases/download"
)

func imageCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "image subcommand required (pull|verify)")
		return 2
	}
	switch args[0] {
	case "pull":
		return cmdImagePull(args[1:], stdout, stderr)
	case "verify":
		return cmdImageVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printRootUsage(stderr)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown image subcommand %q\n", args[0])
		return 2
	}
}

// cmdImagePull installs the guest kernel and image of a release on this node, checked
// against the release's SHA256SUMS.
func cmdImagePull(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("image pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ver := fs.String("version", "", "release to install, e.g. 0.1.0 (default: the version of this asp, when it is a release)")
	baseURL := fs.String("base-url", "", "where the release's files are (default: this project's GitHub release of --version)")
	dir := fs.String("dir", defaultImageDir, "where sets are installed: <dir>/<version>, and <dir>/current for the last")
	linkDir := fs.String("link", defaultLinkDir, "directory the node-agent reads by default; vmlinux, rootfs.img, SHA256SUMS and image.json are linked there (a file there that is not a link is left alone)")
	noLink := fs.Bool("no-link", false, "do not link anything into --link")
	timeout := fs.Duration("timeout", 30*time.Minute, "give up after this long")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	v := strings.TrimPrefix(strings.TrimSpace(*ver), "v")
	if v == "" {
		if cur := version.Get().Version; cur != "dev" {
			v = cur
		} else {
			fmt.Fprintln(stderr, "asp image pull: this asp is not a release build; say which release with --version")
			return 2
		}
	}
	base := strings.TrimSpace(*baseURL)
	if base == "" {
		base = releasesURL + "/v" + v
	}
	opts := imagepull.Options{
		Version: v,
		BaseURL: base,
		Prefix:  "asp-guest_" + v + "_",
		Dir:     filepath.Clean(*dir),
		HTTP:    &http.Client{},
		Progress: func(format string, a ...any) {
			if !*asJSON {
				fmt.Fprintf(stdout, format+"\n", a...)
			}
		},
	}
	if !*noLink {
		opts.LinkDir = filepath.Clean(*linkDir)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	res, err := imagepull.Pull(ctx, opts)
	if err != nil {
		fmt.Fprintf(stderr, "asp image pull: %v\n", err)
		if strings.Contains(err.Error(), imagepull.ErrChecksum.Error()) {
			fmt.Fprintln(stderr, "nothing was installed: what was downloaded is not what the release's SHA256SUMS lists")
		}
		return 1
	}
	if *asJSON {
		return writeJSON(stdout, res)
	}
	state := "installed"
	if res.AlreadyInstalled {
		state = "already installed"
	}
	fmt.Fprintf(stdout, "%s %s in %s (current -> %s)\n", state, v, res.Dir, v)
	for _, p := range res.Skipped {
		fmt.Fprintf(stdout, "left alone: %s is not a link\n", p)
	}
	fmt.Fprintf(stdout, "the node-agent reads it with ASP_GUEST_ROOTFS=%s", filepath.Join(opts.Dir, "current", imagepull.RootFSFile))
	if res.Kernel != "" {
		fmt.Fprintf(stdout, " ASP_GUEST_KERNEL=%s", filepath.Join(opts.Dir, "current", imagepull.KernelFile))
	}
	fmt.Fprintln(stdout, " (or by default through "+defaultLinkDir+")")
	fmt.Fprintln(stdout, "sandboxes already running keep the image they started with; new ones, and resumed ones, boot this kernel: restart the node-agent to measure the new files for attestation")
	return 0
}

// cmdImageVerify checks an installed set against its SHA256SUMS.
func cmdImageVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("image verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := defaultLinkDir
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	checks, err := imagepull.Verify(dir)
	if *asJSON {
		_ = writeJSON(stdout, map[string]any{"dir": dir, "ok": err == nil, "checks": checks})
	} else {
		for _, c := range checks {
			if c.OK {
				fmt.Fprintf(stdout, "ok    %s\n", c.File)
			} else {
				fmt.Fprintf(stdout, "FAIL  %s %s\n", c.File, c.Detail)
			}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "asp image verify: %s: %v\n", dir, err)
		return 1
	}
	return 0
}
