package main

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/envcfg"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/settings"
)

// parse declares the settings on a private flag set and parses args with env as the
// environment, as loadConfig does without exiting.
func parse(t *testing.T, env map[string]string, args ...string) (config, error) {
	t.Helper()
	var cfg config
	fs := flag.NewFlagSet("node-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	s := settings.New(fs, func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok && strings.TrimSpace(v) != ""
	})
	declareSettings(s, &cfg)
	return cfg, s.Parse(args)
}

func declared(t *testing.T) []settings.Setting {
	t.Helper()
	var cfg config
	fs := flag.NewFlagSet("node-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	s := settings.New(fs, func(string) (string, bool) { return "", false })
	declareSettings(s, &cfg)
	return s.Settings()
}

// Names that are not the flag's name in capitals, and why. Each is already in use
// and documented, or shared with the control plane; a new setting follows the rule.
var explicitEnv = map[string]string{
	"workspace-root":     "ASP_WORKSPACE_ROOTS",      // the control plane reads the same list
	"bootstrap-token":    "ASP_NODE_BOOTSTRAP_TOKEN", // the control plane's name for it
	"enroll-token":       "ASP_NODE_ENROLL_TOKEN",
	"egress-allow-cidr":  "ASP_EGRESS_ALLOW_CIDRS",
	"api-key-file":       "ASP_NODE_API_KEY_FILE",
	"default-sandbox-id": "ASP_SANDBOX_ID",
}

// One name per setting, ASP_ everywhere: no duplicates, no setting without the
// prefix, no renamed name that is also somebody's name.
func TestEverySettingHasOneNameWithThePrefix(t *testing.T) {
	flagNames := map[string]string{}
	envNames := map[string]string{}
	claim := func(into map[string]string, name, owner string) {
		if prev, dup := into[name]; dup {
			t.Errorf("%s names both %s and %s", name, prev, owner)
		}
		into[name] = owner
	}
	valid := regexp.MustCompile(`^ASP_[A-Z][A-Z0-9_]*$`)
	flagRe := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	list := declared(t)
	if len(list) < 60 {
		t.Fatalf("only %d settings declared", len(list))
	}
	for _, st := range list {
		if !flagRe.MatchString(st.Flag) {
			t.Errorf("flag %q is not lower-case words with dashes", st.Flag)
		}
		claim(flagNames, st.Flag, st.Flag)
		for _, old := range st.LegacyFlags {
			claim(flagNames, old, st.Flag+" (renamed)")
		}
		if st.Env != "" {
			if !valid.MatchString(st.Env) {
				t.Errorf("--%s: %q is not an ASP_ variable", st.Flag, st.Env)
			}
			want := settings.EnvName(st.Flag)
			if st.Env != want {
				if explicitEnv[st.Flag] != st.Env {
					t.Errorf("--%s sets %s, not %s: name it by its flag, or list the exception with its reason", st.Flag, st.Env, want)
				}
			} else if _, listed := explicitEnv[st.Flag]; listed {
				t.Errorf("--%s is listed as an exception but follows the rule", st.Flag)
			}
			claim(envNames, st.Env, st.Flag)
		} else if len(st.LegacyEnv) > 0 && st.Kind != "deprecated" {
			t.Errorf("--%s has renamed variables but no variable", st.Flag)
		}
		for _, old := range st.LegacyEnv {
			if old == st.Env {
				t.Errorf("--%s lists its own variable as renamed", st.Flag)
			}
			claim(envNames, old, st.Flag+" (renamed)")
		}
	}
	// A renamed flag is not also a setting of its own.
	for _, st := range list {
		for _, old := range st.LegacyFlags {
			if flagNames[old] != st.Flag+" (renamed)" {
				t.Errorf("--%s is the old name of --%s and something else", old, st.Flag)
			}
		}
	}
	// Every setting is documented where the operator looks: the usage names its variable.
	for _, st := range list {
		if st.Env != "" && !strings.Contains(st.Usage, st.Env) {
			t.Errorf("--%s: the usage does not name %s", st.Flag, st.Env)
		}
	}
}

// The environment names the code reads outside the settings follow the same rule: a
// new os.Getenv of a name without the prefix fails here. The names that are not
// ours are listed.
func TestNoVariableIsReadWithoutThePrefix(t *testing.T) {
	external := map[string]bool{
		"SSH_AUTH_SOCK": true, // the user's own agent, which the node bridges to
	}
	root := filepath.Join("..", "..")
	found := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			var fn string
			switch c := call.Fun.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := c.X.(*ast.Ident); ok {
					fn = pkg.Name + "." + c.Sel.Name
				}
			case *ast.Ident:
				fn = c.Name
			}
			switch fn {
			case "os.Getenv", "os.LookupEnv", "os.Setenv", "envcfg.Getenv", "envcfg.Truthy", "envcfg.Get":
			default:
				return true
			}
			// envcfg.Get takes the lookup first.
			arg := call.Args[0]
			if fn == "envcfg.Get" && len(call.Args) > 1 {
				arg = call.Args[1]
			}
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, _ := strconv.Unquote(lit.Value)
			found[name] = append(found[name], filepath.ToSlash(path))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < 5 {
		t.Fatalf("only %d variables found: is the scan reading the sources?", len(found))
	}
	var bad []string
	for name, files := range found {
		if !strings.HasPrefix(name, "ASP_") && !external[name] {
			bad = append(bad, name+" in "+strings.Join(files, ", "))
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("variables read without the ASP_ prefix (use a setting, with Legacy for an old name):\n  %s", strings.Join(bad, "\n  "))
	}
}

// The names that existed before the cleanup still configure the agent.
func TestOldNamesStillWork(t *testing.T) {
	var mu sync.Mutex
	var warned []string
	old := envcfg.Warn
	envcfg.Warn = func(m string) { mu.Lock(); warned = append(warned, m); mu.Unlock() }
	envcfg.ResetWarnings()
	t.Cleanup(func() { envcfg.Warn = old })

	cfg, err := parse(t, map[string]string{
		"CONTROL_PLANE_URL": "https://cp.old", "NODE_ID": "old-node", "NODE_ENDPOINT": "https://old:9443",
		"CH_SOCKET_DIR": "/run/old", "CH_API_SOCKET": "/run/old.sock", "CLOUD_HYPERVISOR_BIN": "/opt/ch",
		"VIRTIOFSD_BIN": "/opt/vfsd", "DRY_RUN": "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlPlaneURL != "https://cp.old" || cfg.NodeID != "old-node" || cfg.Endpoint != "https://old:9443" ||
		cfg.CHSocketDir != "/run/old" || cfg.CHAPISocket != "/run/old.sock" || cfg.VMMBinary != "/opt/ch" ||
		cfg.VirtiofsdBin != "/opt/vfsd" || !cfg.DryRun {
		t.Fatalf("old variables not honoured: %+v", cfg)
	}
	if len(warned) != 8 {
		t.Fatalf("one warning per old variable, got %d: %v", len(warned), warned)
	}

	// The new names win, and the old flags map to the new.
	cfg, err = parse(t, map[string]string{"ASP_CONTROL_PLANE_URL": "https://cp.new", "CONTROL_PLANE_URL": "https://cp.old"},
		"--nft-egress-redirect=false")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlPlaneURL != "https://cp.new" || !cfg.egressNFTRedirect.set || cfg.egressNFTRedirect.value {
		t.Fatalf("%+v", cfg)
	}
	cfg, err = parse(t, map[string]string{"ASP_NFT_EGRESS_REDIRECT": "1"})
	if err != nil || !cfg.egressNFTRedirect.set || !cfg.egressNFTRedirect.value {
		t.Fatalf("old nft variable: %+v %v", cfg.egressNFTRedirect, err)
	}
	// ASP_IDP_REQUIRED meant "multi-user profile" on a node.
	cfg, err = parse(t, map[string]string{"ASP_IDP_REQUIRED": "1"})
	if err != nil || !cfg.MultiUser {
		t.Fatalf("ASP_IDP_REQUIRED: %+v %v", cfg.MultiUser, err)
	}
}

// Booleans were read as "1" and nothing else: ASP_TAP_AUTO=true did nothing.
func TestBooleanVariablesTakeEverySpelling(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		cfg, err := parse(t, map[string]string{"ASP_TAP_AUTO": v, "ASP_RECONCILE": v, "ASP_HOST_VSOCK": v, "ASP_MTLS": v})
		if err != nil || !cfg.TapAuto || !cfg.Reconcile || !cfg.HostVsock || !cfg.MTLS {
			t.Errorf("%q: %+v %v", v, cfg, err)
		}
	}
	if _, err := parse(t, map[string]string{"ASP_TAP_AUTO": "ture"}); err == nil || !strings.Contains(err.Error(), "ASP_TAP_AUTO") {
		t.Errorf("a typo in a boolean was not reported: %v", err)
	}
	cfg, err := parse(t, map[string]string{"ASP_VM_SURVIVE_RESTART": "off"})
	if err != nil || cfg.VMSurviveRestart {
		t.Errorf("ASP_VM_SURVIVE_RESTART=off: %v %v", cfg.VMSurviveRestart, err)
	}
	if cfg, _ = parse(t, nil); !cfg.VMSurviveRestart {
		t.Error("a VM survives the agent by default")
	}
}

// --ssh-agent-confirm given on the command line was overridden by the environment.
func TestSSHAgentConfirmFlagBeatsTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		args []string
		want bool
	}{
		{"default off", nil, nil, false},
		{"multi-user turns it on", map[string]string{"ASP_MULTI_USER": "1"}, nil, true},
		{"a socket template turns it on", map[string]string{"ASP_SSH_AGENT_SOCK_TEMPLATE": "/run/{owner_sub}.sock"}, nil, true},
		{"the variable turns it off in multi-user", map[string]string{"ASP_MULTI_USER": "1", "ASP_SSH_AGENT_CONFIRM": "0"}, nil, false},
		{"the variable turns it on", map[string]string{"ASP_SSH_AGENT_CONFIRM": "yes"}, nil, true},
		{"the flag beats the variable", map[string]string{"ASP_SSH_AGENT_CONFIRM": "0"}, []string{"--ssh-agent-confirm"}, true},
		{"the flag off beats multi-user", map[string]string{"ASP_MULTI_USER": "1"}, []string{"--ssh-agent-confirm=false"}, false},
	} {
		cfg, err := parse(t, c.env, c.args...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := sshAgentConfirm(cfg); got != c.want {
			t.Errorf("%s: confirm = %v, want %v", c.name, got, c.want)
		}
	}
}

// --guest-ssh-agent-auto only ever logged: it is still accepted and says so.
func TestDeadKnobsAreAcceptedAndDoNothing(t *testing.T) {
	var mu sync.Mutex
	var warned []string
	old := envcfg.Warn
	envcfg.Warn = func(m string) { mu.Lock(); warned = append(warned, m); mu.Unlock() }
	t.Cleanup(func() { envcfg.Warn = old })
	if _, err := parse(t, map[string]string{"ASP_GUEST_SSH_AGENT_AUTO": "1"}, "--guest-ssh-agent-auto=0"); err != nil {
		t.Fatal(err)
	}
	// Given on the command line it is the flag that speaks; the variable is ignored.
	if len(warned) == 0 || !strings.Contains(warned[0], "--guest-ssh-agent-auto has no effect") {
		t.Fatalf("warnings: %v", warned)
	}
}

// No setting reads the environment of a flag that is an action.
func TestActionFlagsHaveNoVariable(t *testing.T) {
	for _, st := range declared(t) {
		if (st.Flag == "reap-only" || st.Flag == "print-measurement") && st.Env != "" {
			t.Errorf("--%s has the variable %s", st.Flag, st.Env)
		}
	}
	cfg, err := parse(t, map[string]string{"ASP_REAP_ONLY": "1", "ASP_PRINT_MEASUREMENT": "1"})
	if err != nil || cfg.ReapOnly || cfg.PrintMeasurement {
		t.Fatalf("%+v %v", cfg, err)
	}
	_ = os.Getenv // keep the import honest in case the scan above is edited
}
