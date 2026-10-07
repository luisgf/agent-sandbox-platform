package settings

import (
	"flag"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/envcfg"
)

// env is a fake environment.
func env(kv ...string) envcfg.Lookup {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok && strings.TrimSpace(v) != ""
	}
}

func newSet(look envcfg.Lookup) *Set {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return New(fs, look)
}

// warnings collects what envcfg says about deprecated names.
func warnings(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var msgs []string
	old := envcfg.Warn
	envcfg.Warn = func(m string) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() }
	t.Cleanup(func() { envcfg.Warn = old })
	return &msgs
}

func TestEnvNameFollowsTheFlagName(t *testing.T) {
	for flagName, want := range map[string]string{
		"control-plane-url": "ASP_CONTROL_PLANE_URL",
		"tap-auto":          "ASP_TAP_AUTO",
		"vm.slice":          "ASP_VM_SLICE",
	} {
		if got := EnvName(flagName); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", flagName, got, want)
		}
	}
}

// The flag beats the environment, which beats the default.
func TestTheFlagBeatsTheEnvironmentWhichBeatsTheDefault(t *testing.T) {
	var node string
	var n int
	var d time.Duration
	var on bool
	s := newSet(env("ASP_NODE_ID", "from-env", "ASP_COUNT", "7", "ASP_WAIT", "90s", "ASP_ON", "yes"))
	s.String(&node, "node-id", "default", "node")
	s.Int(&n, "count", 1, "count")
	s.Duration(&d, "wait", time.Second, "wait")
	s.Bool(&on, "on", false, "on")
	if err := s.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if node != "from-env" || n != 7 || d != 90*time.Second || !on {
		t.Fatalf("environment: %q %d %v %v", node, n, d, on)
	}

	s = newSet(env("ASP_NODE_ID", "from-env", "ASP_COUNT", "7", "ASP_WAIT", "90s", "ASP_ON", "yes"))
	s.String(&node, "node-id", "default", "node")
	s.Int(&n, "count", 1, "count")
	s.Duration(&d, "wait", time.Second, "wait")
	s.Bool(&on, "on", true, "on")
	if err := s.Parse([]string{"--node-id=from-flag", "--count=2", "--wait=5s", "--on=false"}); err != nil {
		t.Fatal(err)
	}
	if node != "from-flag" || n != 2 || d != 5*time.Second || on {
		t.Fatalf("flags: %q %d %v %v", node, n, d, on)
	}

	s = newSet(env())
	s.String(&node, "node-id", "default", "node")
	s.Int(&n, "count", 1, "count")
	if err := s.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if node != "default" || n != 1 {
		t.Fatalf("defaults: %q %d", node, n)
	}
}

// A boolean means the same on the command line and in the environment, and a value
// that is not one is an error, not a setting that does nothing.
func TestBooleansHaveOneSpelling(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		var on bool
		s := newSet(env("ASP_TAP_AUTO", v))
		s.Bool(&on, "tap-auto", false, "tap")
		if err := s.Parse(nil); err != nil || !on {
			t.Errorf("env %q: %v %v", v, on, err)
		}
		on = false
		s = newSet(env())
		s.Bool(&on, "tap-auto", false, "tap")
		if err := s.Parse([]string{"--tap-auto=" + v}); err != nil || !on {
			t.Errorf("flag %q: %v %v", v, on, err)
		}
	}
	for _, v := range []string{"0", "false", "no", "off"} {
		on := true
		s := newSet(env("ASP_TAP_AUTO", v))
		s.Bool(&on, "tap-auto", true, "tap")
		if err := s.Parse(nil); err != nil || on {
			t.Errorf("env %q: %v %v", v, on, err)
		}
	}
	var on bool
	s := newSet(env())
	s.Bool(&on, "tap-auto", false, "tap")
	if err := s.Parse([]string{"--tap-auto"}); err != nil || !on {
		t.Errorf("bare flag: %v %v", on, err)
	}
	// Not a boolean: the error names the variable.
	s = newSet(env("ASP_TAP_AUTO", "maybe"))
	s.Bool(&on, "tap-auto", false, "tap")
	if err := s.Parse(nil); err == nil || !strings.Contains(err.Error(), "ASP_TAP_AUTO") {
		t.Errorf("a value that is not a boolean: %v", err)
	}
	s = newSet(env())
	s.Bool(&on, "tap-auto", false, "tap")
	if err := s.Parse([]string{"--tap-auto=maybe"}); err == nil {
		t.Error("the flag took a value that is not a boolean")
	}
}

func TestInvalidValuesAreReportedTogether(t *testing.T) {
	var n int
	var d time.Duration
	var u uint
	s := newSet(env("ASP_COUNT", "many", "ASP_WAIT", "soon", "ASP_BASE", "-3"))
	s.Int(&n, "count", 1, "")
	s.Duration(&d, "wait", time.Second, "")
	s.Uint(&u, "base", 1, "")
	err := s.Parse(nil)
	if err == nil {
		t.Fatal("no error")
	}
	for _, name := range []string{"ASP_COUNT", "ASP_WAIT", "ASP_BASE"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
}

func TestAnExplicitEnvNameAndAFlagOnlySetting(t *testing.T) {
	var roots, once string
	s := newSet(env("ASP_WORKSPACE_ROOTS", "/srv/ws", "ASP_REAP_ONLY", "x"))
	s.String(&roots, "workspace-root", "", "roots", Env("ASP_WORKSPACE_ROOTS"))
	s.String(&once, "reap-only", "", "action", NoEnv())
	if err := s.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if roots != "/srv/ws" {
		t.Fatalf("roots = %q", roots)
	}
	if once != "" {
		t.Fatalf("a flag-only setting read the environment: %q", once)
	}
	for _, st := range s.Settings() {
		switch st.Flag {
		case "workspace-root":
			if st.Env != "ASP_WORKSPACE_ROOTS" || !strings.Contains(st.Usage, "(env ASP_WORKSPACE_ROOTS)") {
				t.Errorf("%+v", st)
			}
		case "reap-only":
			if st.Env != "" || strings.Contains(st.Usage, "env ") {
				t.Errorf("%+v", st)
			}
		}
	}
}

// A renamed variable and a renamed flag keep working, and say they are deprecated.
func TestRenamedNamesStillWorkAndWarn(t *testing.T) {
	msgs := warnings(t)
	var url string
	var on bool
	s := newSet(env("CONTROL_PLANE_URL", "http://old"))
	s.String(&url, "control-plane-url", "http://default", "cp", Legacy("CONTROL_PLANE_URL"), LegacyFlag("cp-url"))
	s.Bool(&on, "egress-nft-redirect", false, "redirect", LegacyFlag("nft-egress-redirect"))
	if err := s.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if url != "http://old" {
		t.Fatalf("old variable: %q", url)
	}
	if len(*msgs) != 1 || !strings.Contains((*msgs)[0], "CONTROL_PLANE_URL is deprecated: use ASP_CONTROL_PLANE_URL") {
		t.Fatalf("warnings: %v", *msgs)
	}

	// The old flag sets the new one, and the environment does not override it.
	*msgs = nil
	s = newSet(env("ASP_CONTROL_PLANE_URL", "http://env"))
	s.String(&url, "control-plane-url", "http://default", "cp", Legacy("CONTROL_PLANE_URL"), LegacyFlag("cp-url"))
	s.Bool(&on, "egress-nft-redirect", false, "redirect", LegacyFlag("nft-egress-redirect"))
	if err := s.Parse([]string{"--cp-url=http://flag", "--nft-egress-redirect"}); err != nil {
		t.Fatal(err)
	}
	if url != "http://flag" || !on {
		t.Fatalf("old flags: %q %v", url, on)
	}
	if len(*msgs) != 2 || !strings.Contains((*msgs)[0], "--cp-url is deprecated: use --control-plane-url") ||
		!strings.Contains((*msgs)[1], "--nft-egress-redirect is deprecated: use --egress-nft-redirect") {
		t.Fatalf("warnings: %v", *msgs)
	}
	// The usage of the old flag points to the new one.
	if f := s.fs.Lookup("cp-url"); f == nil || !strings.Contains(f.Usage, "use --control-plane-url") {
		t.Fatalf("usage of the old flag: %+v", f)
	}
}

// triState is a bool that remembers whether it was set.
type triState struct{ set, value bool }

func (b *triState) String() string {
	if b == nil || !b.set {
		return "auto"
	}
	return strconv.FormatBool(b.value)
}

func (b *triState) Set(v string) error {
	parsed, ok := envcfg.ParseBool(v)
	if !ok {
		return strconv.ErrSyntax
	}
	b.set, b.value = true, parsed
	return nil
}

func (b *triState) IsBoolFlag() bool { return true }

func TestTriStateTellsNotSetFromFalse(t *testing.T) {
	var v triState
	s := newSet(env())
	s.Tri(&v, "ssh-agent-confirm", "confirm")
	if err := s.Parse(nil); err != nil || v.set {
		t.Fatalf("unset: %+v %v", v, err)
	}
	v = triState{}
	s = newSet(env("ASP_SSH_AGENT_CONFIRM", "off"))
	s.Tri(&v, "ssh-agent-confirm", "confirm")
	if err := s.Parse(nil); err != nil || !v.set || v.value {
		t.Fatalf("env off: %+v %v", v, err)
	}
	// An explicit flag beats the environment: the one that used to lose.
	v = triState{}
	s = newSet(env("ASP_SSH_AGENT_CONFIRM", "off"))
	s.Tri(&v, "ssh-agent-confirm", "confirm")
	if err := s.Parse([]string{"--ssh-agent-confirm"}); err != nil || !v.set || !v.value {
		t.Fatalf("flag on, env off: %+v %v", v, err)
	}
}

// The usage text lists defaults, not what the environment holds: a token taken from
// the environment must not be printed by -h.
func TestUsageNeverShowsTheEnvironment(t *testing.T) {
	var token string
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	var out strings.Builder
	fs.SetOutput(&out)
	s := New(fs, env("ASP_NODE_TOKEN", "s3cr3t-value"))
	s.String(&token, "node-token", "", "token")
	if err := s.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if token != "s3cr3t-value" {
		t.Fatalf("token = %q", token)
	}
	fs.PrintDefaults()
	if strings.Contains(out.String(), "s3cr3t-value") {
		t.Fatalf("the usage prints the environment:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ASP_NODE_TOKEN") {
		t.Fatalf("the usage does not name the variable:\n%s", out.String())
	}
}

func TestDeprecatedSettingsAreAcceptedAndSayTheyDoNothing(t *testing.T) {
	msgs := warnings(t)
	s := newSet(env("ASP_OLD_KNOB", "1"))
	s.Deprecated("old-knob", "ASP_OLD_KNOB", "the guest image decides")
	if err := s.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if len(*msgs) != 1 || !strings.Contains((*msgs)[0], "ASP_OLD_KNOB has no effect: the guest image decides") {
		t.Fatalf("env: %v", *msgs)
	}
	*msgs = nil
	s = newSet(env())
	s.Deprecated("old-knob", "ASP_OLD_KNOB", "the guest image decides")
	if err := s.Parse([]string{"--old-knob", "--old-knob=0"}); err != nil {
		t.Fatal(err)
	}
	if len(*msgs) != 2 || !strings.Contains((*msgs)[0], "--old-knob has no effect") {
		t.Fatalf("flag: %v", *msgs)
	}
	if st := s.Settings(); len(st) != 1 || st[0].Kind != "deprecated" {
		t.Fatalf("settings: %+v", st)
	}
}
