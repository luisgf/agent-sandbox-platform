// Package version says which build of ASP a binary is.
//
// A release build sets Version, Commit and Date with -ldflags (see the Makefile and
// .goreleaser.yaml). A plain go build reports "dev", and takes the commit the Go
// toolchain stamped into the binary, so a binary built from a checkout still says
// what it was built from.
//
// The node-agent, the control plane and the CLI are separate modules, so each has
// its own copy of this package: they must be the same files.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set by the linker at release time:
//
//	-X <module>/internal/version.Version=0.1.0
//	-X <module>/internal/version.Commit=<git sha>
//	-X <module>/internal/version.Date=<RFC 3339>
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is what a binary says about itself.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	Modified  bool   `json:"modified,omitempty"` // built from a tree with uncommitted changes
	GoVersion string `json:"go_version"`
}

// Get returns the build information of this binary.
func Get() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date, GoVersion: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = s.Value
				}
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	return info
}

// Short is the one-token form carried in registrations and metrics: "0.1.0", or for
// a build that is not a release "dev+1a2b3c4d" ("-dirty" when the tree had changes).
func (i Info) Short() string {
	if i.Version != "dev" {
		return i.Version
	}
	s := "dev"
	if c := shortCommit(i.Commit); c != "" {
		s += "+" + c
	}
	if i.Modified {
		s += "-dirty"
	}
	return s
}

// String is the form a --version prints.
func (i Info) String() string {
	var parts []string
	if c := shortCommit(i.Commit); c != "" {
		parts = append(parts, "commit "+c)
	}
	if i.Date != "" {
		parts = append(parts, "built "+i.Date)
	}
	parts = append(parts, i.GoVersion)
	return fmt.Sprintf("%s (%s)", i.Short(), strings.Join(parts, ", "))
}

func shortCommit(c string) string {
	if len(c) > 8 {
		return c[:8]
	}
	return c
}

// Short is Get().Short().
func Short() string { return Get().Short() }
