package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/envcfg"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/settings"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/version"
)

// defaultConfigFile is read when it exists; its drop-ins are in defaultConfigFile + ".d".
// --config (or ASP_CONFIG) names another, and then it has to exist.
const defaultConfigFile = "/etc/asp/agent.yaml"

// configSource is where the settings that did not come from flags or the environment came from.
type configSource struct {
	file  *envcfg.File
	known map[string]string // key → variable, for the settings a file may hold
}

// configFlagValue finds --config in args without parsing them: the file is read before the
// flags are applied, because the environment and the flags go over it.
func configFlagValue(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return ""
		}
		for _, p := range []string{"--config", "-config"} {
			if a == p && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(a, p+"=") {
				return strings.TrimPrefix(a, p+"=")
			}
		}
	}
	return ""
}

// settingKeys maps each key a file may hold to the variable of its setting. The settings that
// are actions (flag-only) and the file's own path are not in it.
func settingKeys(s *settings.Set) map[string]string {
	keys := map[string]string{}
	for _, st := range s.Settings() {
		if st.Env == "" || st.Flag == "config" || st.Kind == "deprecated" {
			continue
		}
		keys[envcfg.Key(st.Flag)] = st.Env
	}
	return keys
}

// useConfigFile reads the config file and its drop-ins and puts them under the
// environment in s: precedence is flags, environment, file, defaults.
func useConfigFile(s *settings.Set, args []string, env envcfg.Lookup) (*configSource, error) {
	if env == nil {
		env = envcfg.Getenv
	}
	path, explicit := configFlagValue(args), true
	if path == "" {
		if v, ok := env("ASP_CONFIG"); ok {
			path = v
		} else {
			path, explicit = defaultConfigFile, false
		}
	}
	file, err := envcfg.LoadFile(path, explicit)
	if err != nil {
		return nil, err
	}
	known := settingKeys(s)
	if err := file.Check(known); err != nil {
		return nil, err
	}
	s.Fallback(file.Lookup(known))
	return &configSource{file: file, known: known}, nil
}

// warnLooseSecrets says when a file other users can read holds a credential.
func (c *configSource) warnLooseSecrets(s *settings.Set, warn func(string)) {
	secrets := map[string]bool{}
	for _, st := range s.Settings() {
		if st.Secret {
			secrets[envcfg.Key(st.Flag)] = true
		}
	}
	for key, path := range c.file.From {
		if secrets[key] && c.file.Loose[path] {
			warn(fmt.Sprintf("%s holds %s and can be read by every user: chmod 600 it", path, key))
		}
	}
}

// printConfig writes the effective configuration: every setting, its value and where the
// value came from (flag, environment, file or default). Credentials are not shown.
func printConfig(w io.Writer, s *settings.Set, src *configSource) {
	fs := s.Flags()
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	list := s.Settings()
	sort.Slice(list, func(i, j int) bool { return list[i].Flag < list[j].Flag })
	fmt.Fprintf(w, "# effective configuration of node-agent %s\n", version.Short())
	if src != nil && len(src.file.Paths) > 0 {
		fmt.Fprintf(w, "# files: %s\n", strings.Join(src.file.Paths, ", "))
	} else {
		fmt.Fprintf(w, "# no config file (%s is read when it exists)\n", defaultConfigFile)
	}
	fmt.Fprintln(w, "# order: flag, environment, file, default")
	for _, st := range list {
		if st.Env == "" && !given[st.Flag] {
			continue // an action nobody asked for
		}
		fl := fs.Lookup(st.Flag)
		if fl == nil {
			continue
		}
		value := fl.Value.String()
		source := "default"
		switch {
		case given[st.Flag]:
			source = "flag"
		case st.Env != "" && envHas(st):
			source = "environment"
		case src != nil && fileHas(src, st):
			source = "file " + src.file.From[envcfg.Key(st.Flag)]
		}
		if st.Secret && value != "" {
			value = "<redacted>"
		}
		fmt.Fprintf(w, "%s: %s  # %s\n", envcfg.Key(st.Flag), yamlScalar(st.Kind, value), source)
	}
}

func envHas(st settings.Setting) bool {
	if _, ok := envcfg.Getenv(st.Env); ok {
		return true
	}
	for _, old := range st.LegacyEnv {
		if _, ok := envcfg.Getenv(old); ok {
			return true
		}
	}
	return false
}

func fileHas(src *configSource, st settings.Setting) bool {
	if st.Env == "" {
		return false
	}
	_, ok := src.file.Values[envcfg.Key(st.Flag)]
	return ok
}

// yamlScalar writes a value as the YAML a file would hold: text is quoted when a bare one
// would be read as something else; numbers, booleans and durations are left as they are.
func yamlScalar(kind, v string) string {
	switch kind {
	case "bool", "tri-state", "int", "uint":
		if v != "" {
			return v
		}
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%q", v)
	}
	return strings.TrimSpace(string(b))
}

// stderrWarn is how loadConfig reports a config file problem that is not fatal.
func stderrWarn(msg string) { fmt.Fprintln(os.Stderr, "node-agent: warning: "+msg) }
