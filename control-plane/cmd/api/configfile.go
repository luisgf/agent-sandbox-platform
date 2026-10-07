package main

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/envcfg"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/version"
)

// defaultConfigFile is read when it exists; its drop-ins are in defaultConfigFile + ".d".
// --config (or ASP_CONFIG) names another, and then it has to exist.
const defaultConfigFile = "/etc/asp/server.yaml"

// fileSettings is what the config file gave the process: the variables it set (the
// environment wins over the file, so only those the environment did not have), with the
// file each came from. --print-config shows it.
var fileSettings struct {
	paths   []string
	applied map[string]string
}

// configKeys maps each key a file may hold to its variable.
func configKeys() map[string]string {
	keys := make(map[string]string, len(settingsTable))
	for _, s := range settingsTable {
		keys[envcfg.KeyOf(s.Env)] = s.Env
	}
	return keys
}

// configFlag finds --config in the arguments (the control plane has no flag parser: its
// settings are variables) and ASP_CONFIG after it.
func configFlag(args []string) (path string, explicit bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		for _, p := range []string{"--config", "-config"} {
			if a == p && i+1 < len(args) {
				return args[i+1], true
			}
			if strings.HasPrefix(a, p+"=") {
				return strings.TrimPrefix(a, p+"="), true
			}
		}
	}
	if v, ok := envcfg.Getenv("ASP_CONFIG"); ok {
		return v, true
	}
	return defaultConfigFile, false
}

// loadConfigFile reads the config file and its drop-ins and sets the variables the
// environment does not have, before anything reads them. It fails on a key that is no setting.
func loadConfigFile(args []string) error {
	path, explicit := configFlag(args)
	file, err := envcfg.LoadFile(path, explicit)
	if err != nil {
		return err
	}
	applied, err := file.ApplyToEnv(configKeys())
	if err != nil {
		return err
	}
	fileSettings.paths, fileSettings.applied = file.Paths, applied
	secret := map[string]bool{}
	for _, s := range settingsTable {
		if s.Secret {
			secret[envcfg.KeyOf(s.Env)] = true
		}
	}
	for key, p := range file.From {
		if secret[key] && file.Loose[p] {
			slog.Warn(p + " holds " + key + " and can be read by every user: chmod 600 it")
		}
	}
	return nil
}

func wantsPrintConfig(args []string) bool {
	for _, a := range args {
		if a == "--print-config" || a == "-print-config" {
			return true
		}
	}
	return false
}

// printConfig writes every setting, its value and where it came from: the environment, the
// config file, or nowhere (then what the control plane does by default, in words). Credentials
// are not shown.
func printConfig(w io.Writer) {
	fmt.Fprintf(w, "# effective configuration of asp-control-plane %s\n", version.Short())
	if len(fileSettings.paths) > 0 {
		fmt.Fprintf(w, "# files: %s\n", strings.Join(fileSettings.paths, ", "))
	} else {
		fmt.Fprintf(w, "# no config file (%s is read when it exists)\n", defaultConfigFile)
	}
	fmt.Fprintln(w, "# order: environment, file, default")
	list := append([]setting(nil), settingsTable...)
	sort.Slice(list, func(i, j int) bool { return list[i].Env < list[j].Env })
	for _, s := range list {
		key := envcfg.KeyOf(s.Env)
		v, set := envcfg.Getenv(s.Env)
		switch {
		case !set:
			def := s.Default
			if def == "" {
				def = "unset"
			}
			fmt.Fprintf(w, "# %s: (%s)\n", key, def)
		default:
			source := "environment"
			if p, fromFile := fileSettings.applied[s.Env]; fromFile {
				source = "file " + p
			}
			if s.Secret {
				v = "<redacted>"
			}
			fmt.Fprintf(w, "%s: %s  # %s\n", key, yamlScalar(v), source)
		}
	}
}

// yamlScalar writes a value as the YAML a file would hold: numbers and booleans as they
// are, text quoted only when a bare one would be read as something else.
func yamlScalar(v string) string {
	if v == "" {
		return `""`
	}
	if _, ok := envcfg.ParseBool(v); ok && (v == "1" || v == "0" || strings.EqualFold(v, "true") || strings.EqualFold(v, "false")) {
		return v
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return v
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%q", v)
	}
	return strings.TrimSpace(string(b))
}
