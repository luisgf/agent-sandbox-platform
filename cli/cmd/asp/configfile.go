package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/envcfg"
)

// The CLI reads /etc/asp/asp.yaml, then ~/.config/asp/asp.yaml over it, each with its drop-ins
// in <file>.d/*.yaml. `asp --config FILE <command>` (or ASP_CONFIG) names one file instead, and
// it has to exist. Flags and environment variables win over them.
//
// A key is a variable without its ASP_ prefix, in lower case: control_plane_url, tenant, api_key.
const systemConfigFile = "/etc/asp/asp.yaml"

// Replaced by the tests, so that the files of whoever runs them are not read.
var (
	systemConfigPath = systemConfigFile
	userConfigPath   = func() string {
		if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
			return filepath.Join(d, "asp", "asp.yaml")
		}
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return filepath.Join(h, ".config", "asp", "asp.yaml")
		}
		return ""
	}
)

// configChoice is the --config before the command, if there was one.
type configChoice struct {
	path  string
	named bool
}

// leadingConfigFlag takes `--config FILE` off the front of args. It is a global option, so it
// comes before the command, as `git -C dir status` does: after the command the arguments are
// the command's own (the program a sandbox runs may have a --config of its own).
func leadingConfigFlag(args []string) ([]string, configChoice, error) {
	var c configChoice
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--config" || a == "-config":
			if len(args) < 2 {
				return nil, c, errors.New("--config needs a file")
			}
			c = configChoice{path: args[1], named: true}
			args = args[2:]
		case strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "-config="):
			c = configChoice{path: a[strings.Index(a, "=")+1:], named: true}
			args = args[1:]
		default:
			return args, c, nil
		}
	}
	return args, c, nil
}

// configKeys maps each key a file may set to its variable.
func configKeys() map[string]string {
	keys := make(map[string]string, len(settingsTable))
	for _, s := range settingsTable {
		if !s.Legacy {
			keys[envcfg.KeyOf(s.Env)] = s.Env
		}
	}
	return keys
}

func secretKeys() map[string]bool {
	secret := map[string]bool{}
	for _, s := range settingsTable {
		if s.Secret {
			secret[envcfg.KeyOf(s.Env)] = true
		}
	}
	return secret
}

// configPaths are the base files to read, in order of increasing precedence. A named file
// (--config, ASP_CONFIG) replaces them and must exist; an empty name reads none.
func configPaths(c configChoice) (paths []string, explicit bool) {
	if c.named {
		return []string{c.path}, true
	}
	if v, ok := envcfg.Getenv("ASP_CONFIG"); ok {
		return []string{strings.TrimSpace(v)}, true
	}
	paths = []string{systemConfigPath}
	if u := userConfigPath(); u != "" {
		paths = append(paths, u)
	}
	return paths, false
}

// loaded is what the config files gave this process.
var loaded struct {
	paths   []string
	applied map[string]string // variable → file
}

// loadConfigFiles reads the config files and sets the variables the environment does not
// have. The file that wins is read first: the user's before the system's, so a variable the
// user's file set is already there when the system's is applied.
func loadConfigFiles(c configChoice) error {
	paths, explicit := configPaths(c)
	known, secret := configKeys(), secretKeys()
	loaded.paths, loaded.applied = nil, map[string]string{}
	for i := len(paths) - 1; i >= 0; i-- {
		file, err := envcfg.LoadFile(paths[i], explicit)
		if err != nil {
			return err
		}
		applied, err := file.ApplyToEnv(known)
		if err != nil {
			return err
		}
		for name, from := range applied {
			loaded.applied[name] = from
		}
		loaded.paths = append(append([]string(nil), file.Paths...), loaded.paths...)
		warned := map[string]bool{}
		for key, p := range file.From {
			if secret[key] && file.Loose[p] && !warned[p] {
				warned[p] = true
				envcfg.Warn(p + " holds a credential and can be read by every user: chmod 600 it")
			}
		}
	}
	return nil
}

// cmdConfig is `asp config show`: where the settings come from.
// configShowHelp is what -h adds to the usage line.
const configShowHelp = `
Shows where each setting comes from: the files, the environment, the defaults.
  --effective         also list the settings nothing sets, with their defaults
  --component NAME    asp (the default) shows this command's own settings; server and agent show what the
                      merged files of the control plane (/etc/asp/server.yaml) or the node-agent
                      (/etc/asp/agent.yaml) say
  --config FILE       read this file instead of /etc/asp/asp.yaml and ~/.config/asp/asp.yaml`

func cmdConfig(args []string, c configChoice, stdout, stderr io.Writer) int {
	usage := "usage: asp config show [--effective] [--component asp|server|agent] [--config FILE]"
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		fmt.Fprintln(stdout, usage+configShowHelp)
		return 0
	}
	if len(args) == 0 || args[0] != "show" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	component, effective := "asp", false
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch a := rest[i]; {
		case a == "--effective" || a == "-effective":
			effective = true
		case (a == "--component" || a == "-component") && i+1 < len(rest):
			component = rest[i+1]
			i++
		case strings.HasPrefix(a, "--component="):
			component = strings.TrimPrefix(a, "--component=")
		case (a == "--config" || a == "-config") && i+1 < len(rest):
			c = configChoice{path: rest[i+1], named: true}
			i++
		case strings.HasPrefix(a, "--config="):
			c = configChoice{path: strings.TrimPrefix(a, "--config="), named: true}
		case a == "-h" || a == "--help" || a == "-help":
			fmt.Fprintln(stdout, usage+configShowHelp)
			return 0
		default:
			fmt.Fprintf(stderr, "asp config show: unknown argument %q\n%s\n", a, usage)
			return 2
		}
	}
	switch component {
	case "asp":
		return showOwn(c, effective, stdout, stderr)
	case "server", "agent":
		return showComponent(component, c, stdout, stderr)
	}
	fmt.Fprintf(stderr, "asp config show: --component takes asp, server or agent, not %q\n", component)
	return 2
}

// showOwn prints the CLI's own settings: those a file or the environment set, and with
// --effective the others too, with their defaults.
func showOwn(c configChoice, effective bool, stdout, stderr io.Writer) int {
	if err := loadConfigFiles(c); err != nil {
		fmt.Fprintf(stderr, "asp config show: %v\n", err)
		return 1
	}
	if len(loaded.paths) > 0 {
		fmt.Fprintf(stdout, "# files: %s\n", strings.Join(loaded.paths, ", "))
	} else if paths, _ := configPaths(c); len(paths) > 0 && paths[0] != "" {
		fmt.Fprintf(stdout, "# no config file (%s are read when they exist)\n", strings.Join(paths, " and "))
	} else {
		fmt.Fprintln(stdout, "# no config file")
	}
	fmt.Fprintln(stdout, "# a flag beats the environment, which beats a file, which beats the default (flags are not shown here)")
	list := append([]setting(nil), settingsTable...)
	sort.Slice(list, func(i, j int) bool { return list[i].Env < list[j].Env })
	for _, s := range list {
		key := envcfg.KeyOf(s.Env)
		v, set := envcfg.Getenv(s.Env)
		switch {
		case set:
			source := "environment"
			if from, ok := loaded.applied[s.Env]; ok {
				source = "file " + from
			}
			if s.Secret {
				v = "<redacted>"
			}
			fmt.Fprintf(stdout, "%s: %s  # %s\n", key, yamlScalar(v), source)
		case effective && !s.Legacy:
			def := s.Default
			if def == "" {
				def = "unset"
			}
			fmt.Fprintf(stdout, "# %s: (%s)\n", key, def)
		}
	}
	return 0
}

// showComponent prints what the merged files of the control plane or the node-agent say. The
// CLI does not know their settings, so a key that looks like a credential is hidden; the
// component itself prints the whole effective configuration, defaults and sources included.
func showComponent(component string, c configChoice, stdout, stderr io.Writer) int {
	base, named := "/etc/asp/"+component+".yaml", c.named
	if c.named {
		base = c.path
	}
	file, err := envcfg.LoadFile(base, named && base != "")
	if err != nil {
		fmt.Fprintf(stderr, "asp config show: %v\n", err)
		return 1
	}
	binary := map[string]string{"server": "asp-control-plane", "agent": "asp-node-agent"}[component]
	if len(file.Paths) == 0 {
		fmt.Fprintf(stdout, "# no config file (%s is read when it exists)\n", base)
		return 0
	}
	fmt.Fprintf(stdout, "# files: %s\n", strings.Join(file.Paths, ", "))
	keys := make([]string, 0, len(file.Values))
	for k := range file.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := file.Values[k]
		if looksSecret(k) {
			v = "<redacted>"
		}
		fmt.Fprintf(stdout, "%s: %s  # %s\n", k, yamlScalar(v), file.From[k])
	}
	fmt.Fprintf(stdout, "# the effective configuration, with defaults and the environment: %s --print-config\n", binary)
	return 0
}

// looksSecret is a guess by name, for the settings of a component this binary does not know.
func looksSecret(key string) bool {
	for _, w := range []string{"token", "secret", "password", "pass", "api_key", "database_url", "credential", "private"} {
		if strings.Contains(key, w) {
			return true
		}
	}
	return false
}

func yamlScalar(v string) string {
	if v == "" {
		return `""`
	}
	if v == "true" || v == "false" {
		return v
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return v
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return strconv.Quote(v)
	}
	return strings.TrimSpace(string(b))
}
