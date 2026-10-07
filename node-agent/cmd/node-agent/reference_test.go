package main

import (
	"flag"
	"fmt"
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
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/settings"
)

var update = flag.Bool("update", false, "rewrite the generated reference pages (make docs)")

// goldenFile checks a generated page against the one in the repository, or rewrites it with
// -update (make docs does).
func goldenFile(t *testing.T, rel, want string) {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	if _, err := os.Stat(filepath.Join(root, "docs")); err != nil {
		t.Skipf("no docs/ next to the module (it is built outside the repository): %v", err)
	}
	path := filepath.Join(root, rel)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (make docs writes it)", rel, err)
	}
	if string(got) != want {
		t.Errorf("%s is out of date: run `make docs` and commit the result", rel)
	}
}

func TestReferenceIsUpToDate(t *testing.T) {
	goldenFile(t, referencePage, referenceMarkdown())
}

// Every flag is under exactly one heading of the page, and a heading names only flags that exist:
// a new setting has to be put somewhere, so it cannot be missing from the reference.
func TestEveryFlagIsInOneGroup(t *testing.T) {
	declared := map[string]bool{}
	for _, st := range declaredSettings() {
		declared[st.Flag] = true
	}
	in := map[string]string{}
	for _, g := range referenceGroups {
		for _, name := range g.Flags {
			if !declared[name] {
				t.Errorf("%q lists --%s, which is not a flag", g.Title, name)
			}
			if prev, dup := in[name]; dup {
				t.Errorf("--%s is under both %q and %q", name, prev, g.Title)
			}
			in[name] = g.Title
		}
	}
	for name := range declared {
		if _, ok := in[name]; !ok {
			t.Errorf("--%s is in no group of the reference (referenceGroups in reference_test.go)", name)
		}
	}
}

// The ASP_ names the source mentions as whole strings are the variables it reads. Each one is a
// setting, a renamed name of one, or listed in envOnlySettings; and what envOnlySettings lists is
// still in the source. A variable added to the code without a line of documentation fails here.
func TestEveryVariableTheSourceReadsIsDocumented(t *testing.T) {
	known := map[string]string{}
	for _, st := range declaredSettings() {
		if st.Env != "" {
			known[st.Env] = "--" + st.Flag
		}
		for _, old := range st.LegacyEnv {
			known[old] = "--" + st.Flag + " (renamed)"
		}
	}
	for _, e := range envOnlySettings {
		if prev, dup := known[e.Env]; dup {
			t.Errorf("%s is listed as having no flag, and it is %s", e.Env, prev)
		}
		known[e.Env] = "reference.go"
	}

	re := regexp.MustCompile(`^ASP_[A-Z][A-Z0-9_]*$`)
	mentioned := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
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
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && re.MatchString(s) {
					mentioned[s] = append(mentioned[s], filepath.ToSlash(path))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mentioned) < 20 {
		t.Fatalf("only %d variables found: is the scan reading the sources?", len(mentioned))
	}
	var missing []string
	for name, files := range mentioned {
		if _, ok := known[name]; !ok {
			missing = append(missing, name+" in "+strings.Join(files, ", "))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("variables the source mentions that the reference does not document (declare a setting, or add them to envOnlySettings):\n  %s", strings.Join(missing, "\n  "))
	}
	for _, e := range envOnlySettings {
		if _, ok := mentioned[e.Env]; !ok {
			t.Errorf("%s is documented as a variable of the node-agent and the source no longer mentions it", e.Env)
		}
	}
}

func TestEverySettingHasAUsage(t *testing.T) {
	for _, st := range declaredSettings() {
		if strings.TrimSpace(st.Usage) == "" {
			t.Errorf("--%s has no usage text", st.Flag)
		}
	}
}

func TestUsageMarkdown(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a workspace must be inside <root>/<tenant>/ (symbolic links resolved)", "a workspace must be inside `<root>/<tenant>/` (symbolic links resolved)"},
		{"on | report (log, remove nothing) | off; --dry-run only reports", "on | report (log, remove nothing) | off; `--dry-run` only reports"},
		{"default /var/lib/asp/disks.", "default `/var/lib/asp/disks`."},
		{"listen addr (e.g. 0.0.0.0:9443) when", "listen addr (e.g. `0.0.0.0:9443`) when"},
		{"host[:port] laptops dial", "`host[:port]` laptops dial"},
		{"the legacy/debug mode, and/or more", "the legacy/debug mode, and/or more"},
		{"from cert-dir/ca.crt and 10.200.0.0/16", "from `cert-dir/ca.crt` and `10.200.0.0/16`"},
		{"set ASP_FOO_BAR=1 or a < b", "set `ASP_FOO_BAR=1` or a &lt; b"},
	} {
		if got := usageMarkdown(c.in); got != c.want {
			t.Errorf("usageMarkdown(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

// referencePage is where referenceMarkdown is written, relative to the repository root.
const referencePage = "docs/reference/configuration/node-agent.md"

// referenceGroup is one heading of the page and the flags under it. Every flag is in exactly one
// group (a test), so a new setting cannot be left out of the reference.
type referenceGroup struct {
	Title string
	Flags []string
}

var referenceGroups = []referenceGroup{
	{"Plano de control, identidad del nodo y enrollment", []string{
		"control-plane-url", "control-plane-ca", "enroll-url", "node-id", "endpoint", "enroll",
		"bootstrap-token", "enroll-token", "cert-dir", "mtls", "api-key-file"}},
	{"Reconciliación y capacidad", []string{
		"reconcile", "reconcile-interval", "reconcile-workers", "heartbeat-interval",
		"capacity-cpu", "capacity-mem-mib", "max-sandboxes"}},
	{"API local del agente (exec)", []string{
		"agent-listen", "agent-tls-listen", "insecure-agent-listen", "agent-token-file"}},
	{"Hipervisor, discos y workspaces", []string{
		"ch-binary", "ch-socket-dir", "ch-api-socket", "guest-kernel", "guest-rootfs", "guest-verify",
		"guest-ready-timeout", "disk-dir", "disk-min-free-mib", "stop-grace", "workspace-root",
		"virtiofsd-bin", "virtiofsd-sandbox", "pod-daemon-sock", "pod-daemon-port", "reap-leftovers"}},
	{"Confinamiento de las microVMs", []string{
		"vm-confine", "vm-survive-restart", "vm-unprivileged", "vm-uid-base", "vm-run-dir", "vm-slice",
		"vm-memory-overhead-mib", "vm-cpu-overhead-percent", "vm-tasks-max"}},
	{"Red de las VMs y local-net", []string{
		"guest-subnet", "tap-auto", "local-net-key-dir", "local-net-dial"}},
	{"Salida a Internet (egress)", []string{
		"egress-enforce", "egress-proxy-listen", "egress-dns-sink", "egress-allow-cidr", "egress-mitm",
		"egress-mitm-ca", "egress-nft-redirect", "nft-egress-mode", "nft-dns-action", "nft-http-ports"}},
	{"Identidad de las sandboxes y agente SSH", []string{
		"host-vsock", "host-vsock-dir", "identity-listen", "default-sandbox-id",
		"insecure-identity-sandbox-header", "ssh-agent-bridge", "ssh-agent-confirm",
		"insecure-ssh-agent-global-approvals", "ssh-agent-sock-template", "multi-user"}},
	{"Observabilidad", []string{"metrics-listen", "pprof-listen", "insecure-obs-listen"}},
	{"Laboratorio, diagnóstico y acciones", []string{
		"dry-run", "config", "print-config", "doctor", "doctor-json", "print-measurement", "reap-only", "version"}},
	{"Obsoletos", []string{"guest-ssh-agent-auto"}},
}

// envOnlySetting is something the node-agent reads from the environment that has no flag: a
// knob of a package deep inside, or a variable it only warns about. A test holds this list to the
// source: every ASP_ name the code mentions is a setting, is here, or the test says why not.
type envOnlySetting struct {
	Env     string
	Default string // in words where the code decides
	Help    string
	Secret  bool
}

var envOnlySettings = []envOnlySetting{
	{Env: "ASP_ATTEST_KEY", Default: "`$TMPDIR/asp-attest-key.pem`, solo en un lab",
		Help: "Clave ECDSA P-256 (PEM) con la que el nodo firma sus atestaciones de arranque cuando no usa la de su certificado; se crea si no existe. El plano de control tiene que fiarse de su clave pública (el mismo fichero en un lab de un host, o `ASP_ATTEST_TRUSTED_PUBS`). Un nodo de producción sin esta variable firma solo con la clave de su certificado y no crea ninguna clave temporal"},
	{Env: "ASP_ALLOW_TMP_KEYS",
		Help: "`1`: un nodo de producción arranca aunque `--cert-dir`, `ASP_ATTEST_KEY` o `--egress-mitm-ca` (con `--egress-mitm`) estén en un directorio temporal (`/tmp`, `/var/tmp`, `/dev/shm`, `$TMPDIR`), que un reinicio vacía. Es de producción un nodo sin `--dry-run` con `--agent-tls-listen`, `--mtls` o un certificado en `--cert-dir`: ahí, sin esta variable, no arranca (código 2). En un lab solo avisa"},
	{Env: "ASP_NODE_API_KEY", Secret: true,
		Help: "La API key (ámbito `platform`) que el nodo manda al plano de control, para quien no quiere un fichero; `--api-key-file` gana. Hace falta con un plano de control por HTTP plano, donde no hay certificado de cliente"},
	{Env: "ASP_EGRESS_ALLOWLIST_JSON", Default: "ninguna: deny-default",
		Help: "Política de egress de todo origen que no es una sandbox conocida, en JSON (`{\"mode\":\"deny-default\",\"rules\":[{\"host_pattern\":\"*.example.com\",\"port\":443,\"enabled\":true}]}`). Una sandbox conocida usa la política de su tenant, que llega con el trabajo; un JSON que no se entiende se ignora"},
	{Env: "ASP_EGRESS_MAX_BODY", Default: "`8388608` (8 MiB)",
		Help: "Bytes máximos del cuerpo de una petición HTTP por el proxy de egress; más es un 413. Las respuestas pasan enteras"},
	{Env: "ASP_NFT_SCRIPT", Default: "el script que lleva el binario",
		Help: "Ruta de un script de nftables propio que sustituye al embebido. Si no existe, el nodo falla en vez de aplicar otro"},
	{Env: "ASP_NFT_TABLE", Default: "`asp_egress`", Help: "Nombre de la tabla de nftables del redirect de egress"},
	{Env: "ASP_EGRESS_PROXY_IP", Default: "`10.200.0.1`", Help: "Informativa: sale en la nota que imprime el script de nftables. Las reglas redirigen a un puerto local y no usan la IP"},
	{Env: "ASP_EGRESS_PROXY_PORT", Default: "`8888`", Help: "El node-agent toma el puerto de `--egress-proxy-listen` (8888 si no lo da): esta variable solo vale para el script de nftables ejecutado a mano"},
	{Env: "ASP_EGRESS_DNS_SINK_IP", Default: "el valor de `ASP_EGRESS_PROXY_IP`", Help: "Informativa, como `ASP_EGRESS_PROXY_IP`"},
	{Env: "ASP_EGRESS_DNS_SINK_PORT", Default: "`5353`", Help: "El node-agent toma el puerto de `--egress-dns-sink` (5353 si no lo da): esta variable solo vale para el script de nftables ejecutado a mano"},
	{Env: "ASP_FENCE_ENDPOINT", Help: "Se ignora, con un aviso: un nodo no elige cómo se le apaga. El destino de fencing lo fija un operador en el plano de control (`asp node fence set`)"},
	{Env: "ASP_FENCE_TOKEN", Help: "Se ignora, con un aviso, por lo mismo que `ASP_FENCE_ENDPOINT`"},
}

// externalEnv is what the node-agent reads that is not ours.
var externalEnv = []struct{ Env, Help string }{
	{"SSH_AUTH_SOCK", "El agente SSH del usuario al que se conecta el puente (`--ssh-agent-bridge`) cuando la sandbox no tiene un socket propio"},
}

// declaredSettings is every setting of the node-agent, as the command line declares them.
func declaredSettings() []settings.Setting {
	var cfg config
	fs := flag.NewFlagSet("node-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	s := settings.New(fs, func(string) (string, bool) { return "", false })
	declareSettings(s, &cfg)
	return s.Settings()
}

// referenceMarkdown is docs/reference/configuration/node-agent.md, written from the settings the
// command line declares and from envOnlySettings. A test fails when the file in the repository
// differs from this (make docs rewrites it), so the page cannot drift from the flags.
func referenceMarkdown() string {
	byFlag := map[string]settings.Setting{}
	for _, st := range declaredSettings() {
		byFlag[st.Flag] = st
	}
	var b strings.Builder
	b.WriteString("<!-- Generado por `make docs` a partir de los ajustes de node-agent/cmd/node-agent/main.go y de reference.go. No lo edites a mano: un test falla si no coincide con el código. -->\n")
	b.WriteString("# Configuración del node-agent\n\n")
	b.WriteString("Cada ajuste es una opción de la línea de órdenes y una variable de entorno `ASP_*`. Prioridad: **opción > variable de entorno > fichero de configuración > valor por defecto**. ")
	b.WriteString("El fichero es `/etc/asp/agent.yaml` (más los `agent.yaml.d/*.yaml` que lo acompañan), o el que nombren `--config FILE` o `ASP_CONFIG`; ")
	b.WriteString("la clave de un ajuste es su opción con guiones bajos (`control_plane_url`) y una clave que no es un ajuste es un error ([el fichero de configuración](../../how-to/config-file.md)). ")
	b.WriteString("`node-agent --print-config` lista cada ajuste con su valor y de dónde sale, sin las credenciales; las marcadas con 🔒 son credenciales. ")
	b.WriteString("Los booleanos se escriben igual en la opción, la variable y el fichero: `1`, `true`, `yes`, `on` / `0`, `false`, `no`, `off`.\n\n")
	b.WriteString("Los ajustes de los otros programas: [plano de control](control-plane.md), [`asp`](cli.md) y [`asp-server`](asp-server.md).\n")
	for _, g := range referenceGroups {
		fmt.Fprintf(&b, "\n## %s\n\n| Opción | Variable | Por defecto | Qué hace |\n|---|---|---|---|\n", g.Title)
		for _, name := range g.Flags {
			st, ok := byFlag[name]
			if !ok {
				continue // a test reports a group that names a flag that does not exist
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", optionCell(st), variableCell(st), nodeDefaultCell(st), usageCell(st))
		}
	}
	b.WriteString("\n## Variables sin opción\n\nSe leen del entorno y no tienen opción ni clave en el fichero.\n\n| Variable | Por defecto | Qué hace |\n|---|---|---|\n")
	for _, e := range envOnlySettings {
		name := "`" + e.Env + "`"
		if e.Secret {
			name += " 🔒"
		}
		def := e.Default
		if def == "" {
			def = "—"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", name, mdCell(def), mdCell(e.Help))
	}
	for _, e := range externalEnv {
		fmt.Fprintf(&b, "| `%s` | — | %s. No es de ASP. |\n", e.Env, mdCell(e.Help))
	}
	return b.String()
}

func optionCell(st settings.Setting) string {
	return "`--" + st.Flag + "`"
}

func variableCell(st settings.Setting) string {
	if st.Env == "" {
		return "—"
	}
	s := "`" + st.Env + "`"
	if st.Secret {
		s += " 🔒"
	}
	return s
}

// nodeDefaultCell shows the default of a setting: nothing for an action or an empty string.
func nodeDefaultCell(st settings.Setting) string {
	switch {
	case st.Kind == "deprecated", st.Default == "":
		return "—"
	case strings.ContainsAny(st.Default, " |"):
		return mdCell(st.Default)
	}
	return "`" + st.Default + "`"
}

// usageCell is the usage text of the flag as Markdown, with the names it used to have.
func usageCell(st settings.Setting) string {
	usage := st.Usage
	if st.Env != "" {
		usage = strings.TrimSuffix(usage, " (env "+st.Env+")")
	}
	out := mdCell(sentence(usageMarkdown(usage)))
	var old []string
	for _, f := range st.LegacyFlags {
		old = append(old, "`--"+f+"`")
	}
	for _, e := range st.LegacyEnv {
		old = append(old, "`"+e+"`")
	}
	if len(old) > 0 && st.Kind != "deprecated" {
		out += " Nombres antiguos, que siguen valiendo con un aviso: " + strings.Join(old, ", ") + "."
	}
	return out
}

// sentence starts a usage with a capital and ends it with a full stop, so that a column of them
// reads as text: the usages are written as flag help, which has neither.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if first := s[0]; first >= 'a' && first <= 'z' {
		word := s
		if i := strings.IndexAny(s, " ,;:"); i > 0 {
			word = s[:i]
		}
		if strings.Trim(word, "abcdefghijklmnopqrstuvwxyz-") == "" {
			s = strings.ToUpper(s[:1]) + s[1:]
		}
	}
	if last := s[len(s)-1]; last != '.' && last != '!' && last != '?' {
		s += "."
	}
	return s
}

// usageMarkdown turns the plain text of a usage into Markdown: what looks like code (a flag, a
// variable, a path, an address, a placeholder) goes in code font, and the rest is escaped, so a
// <placeholder> is not taken for an HTML tag.
func usageMarkdown(s string) string {
	words := strings.Split(s, " ")
	for i, w := range words {
		words[i] = wordMarkdown(w)
	}
	return strings.Join(words, " ")
}

var (
	pathLike   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_./{}<>*-]*$`)
	cidrLike   = regexp.MustCompile(`^[0-9A-Fa-f:.]+/[0-9]+$`)
	addrLike   = regexp.MustCompile(`^[0-9A-Za-z.\[\]-]*:[0-9]+$`)
	punctAfter = ".,;:)"
)

// pathish says whether a word with a slash is a path or an address range, and not two alternatives
// (and/or): it has a dot (cert-dir/ca.crt), more than one slash, or is a CIDR (10.200.0.0/16).
func pathish(w string) bool {
	if !pathLike.MatchString(w) {
		return false
	}
	return strings.Contains(w, ".") || strings.Count(w, "/") >= 2 || cidrLike.MatchString(w)
}

func wordMarkdown(w string) string {
	pre := w[:len(w)-len(strings.TrimLeft(w, "("))]
	core := w[len(pre):]
	trimmed := strings.TrimRight(core, punctAfter)
	post := core[len(trimmed):]
	core = trimmed
	switch {
	case core == "":
		return w
	case strings.Contains(core, "`"):
		return w
	case strings.HasPrefix(core, "--") && len(core) > 2,
		strings.HasPrefix(core, "/") && len(core) > 1,
		strings.HasPrefix(core, "ASP_"), strings.HasPrefix(core, "$"), strings.HasPrefix(core, "=") && len(core) > 1,
		strings.ContainsAny(core, "{}[]"),
		strings.HasPrefix(core, "<") && strings.Contains(core, ">"),
		pathish(core), addrLike.MatchString(core):
		return pre + "`" + core + "`" + post
	}
	return pre + strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(core) + post
}

// mdCell makes text safe inside a Markdown table cell.
func mdCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\r\n", " ", "\n", " ").Replace(strings.TrimSpace(s))
}
