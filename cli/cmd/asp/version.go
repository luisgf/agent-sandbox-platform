package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/version"
)

// cmdVersion prints which build of asp this is.
func cmdVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("asp version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the build information as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	info := version.Get()
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			fmt.Fprintln(stderr, "asp:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "asp %s\n", info)
	return 0
}
