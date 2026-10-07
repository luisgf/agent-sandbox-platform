package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/envcfg"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/refdoc"
)

var update = flag.Bool("update", false, "rewrite the generated reference pages (make docs)")

// The pages this package generates, relative to the repository root.
const (
	settingsPage = "docs/reference/configuration/cli.md"
	commandsPage = "docs/reference/cli.md"
)

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

func TestSettingsReferenceIsUpToDate(t *testing.T) {
	goldenFile(t, settingsPage, settingsMarkdown())
}

func TestCommandsReferenceIsUpToDate(t *testing.T) {
	goldenFile(t, commandsPage, commandsMarkdown(t))
}

// settingsMarkdown is docs/reference/configuration/cli.md, written from settingsTable: the table
// is held to the source by a test, so the page cannot miss a variable.
func settingsMarkdown() string {
	var b strings.Builder
	b.WriteString("<!-- Generado por `make docs` a partir de settingsTable (cli/cmd/asp/settingstable.go). No lo edites a mano: un test falla si no coincide con el código. -->\n")
	b.WriteString("# Configuración de `asp`\n\n")
	b.WriteString("Cada ajuste es una variable de entorno `ASP_*`; muchos tienen también una opción en la orden que la usa (`--tenant`, `--api-key`…, ver [las órdenes](../cli.md)). ")
	b.WriteString("Prioridad: **opción > variable de entorno > fichero de configuración > valor por defecto**. ")
	b.WriteString("Los ficheros son `/etc/asp/asp.yaml` y, encima, `~/.config/asp/asp.yaml`, cada uno con los `asp.yaml.d/*.yaml` que lo acompañan; `asp --config FILE <orden>` o `ASP_CONFIG` leen ese fichero en su lugar (`--config` solo se admite antes de la orden). ")
	b.WriteString("La clave de un ajuste es su variable sin `ASP_` y en minúsculas ([el fichero de configuración](../../how-to/config-file.md)). ")
	b.WriteString("`asp config show [--effective]` lista cada ajuste con su valor y de dónde sale; las credenciales (🔒) salen como `<redacted>`.\n\n")
	b.WriteString("Los ajustes de los otros programas: [plano de control](control-plane.md), [node-agent](node-agent.md) y [`asp-server`](asp-server.md).\n\n")
	b.WriteString("| Variable | Clave del fichero | Por defecto | Qué hace |\n|---|---|---|---|\n")
	for _, s := range settingsTable {
		name := "`" + s.Env + "`"
		if s.Secret {
			name += " 🔒"
		}
		key := "`" + envcfg.KeyOf(s.Env) + "`"
		if s.Legacy {
			key = "— (un fichero no puede fijarla)"
		}
		def := "—"
		switch {
		case s.Default == "":
		case strings.ContainsAny(s.Default, " |"):
			def = refdoc.Cell(s.Default)
		default:
			def = "`" + s.Default + "`"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", name, key, def, refdoc.Cell(refdoc.Sentence(refdoc.UsageMarkdown(s.Help))))
	}
	return b.String()
}

// cliCommand is one command of `asp`, as the usage of the root command lists it.
type cliCommand struct {
	Words    []string // sandbox create
	Synopsis string   // asp sandbox create [flags]
	Summary  string   // the words after the synopsis, when the usage has any
}

func (c cliCommand) name() string { return strings.Join(c.Words, " ") }

var (
	wordRe = regexp.MustCompile(`^[a-z][a-z0-9-]*(\|[a-z][a-z0-9-]*)*$`)
	gapRe  = regexp.MustCompile(`^(.*?\S) {2,}(\S.*)$`)
)

// commandsOfUsage reads the commands out of the usage of the root command: a line under "Usage:"
// that starts with asp and a lower-case word. A word with | is several commands.
func commandsOfUsage(usage string) []cliCommand {
	var out []cliCommand
	in := false
	for _, line := range strings.Split(usage, "\n") {
		switch {
		case strings.TrimSpace(line) == "Usage:":
			in = true
			continue
		case !in:
			continue
		case strings.TrimSpace(line) == "":
			return out
		}
		rest := strings.TrimPrefix(strings.TrimSpace(line), "asp ")
		summary := ""
		if m := gapRe.FindStringSubmatch(rest); m != nil {
			rest, summary = m[1], m[2]
		}
		tokens := strings.Split(rest, " ")
		n := 0
		for n < len(tokens) && wordRe.MatchString(tokens[n]) {
			n++
		}
		if n == 0 {
			continue // asp [--config FILE] <command> ...
		}
		tail := strings.Join(tokens[n:], " ")
		// The alternatives of a word are commands of their own.
		combos := [][]string{nil}
		for _, tok := range tokens[:n] {
			var next [][]string
			for _, alt := range strings.Split(tok, "|") {
				for _, c := range combos {
					next = append(next, append(append([]string(nil), c...), alt))
				}
			}
			combos = next
		}
		for _, words := range combos {
			syn := "asp " + strings.Join(words, " ")
			if tail != "" {
				syn += " " + tail
			}
			out = append(out, cliCommand{Words: words, Synopsis: syn, Summary: summary})
		}
	}
	return out
}

// cliFlag is one line of the usage a command prints for -h.
type cliFlag struct{ Name, Type, Usage string }

// cliHelp is what `asp <command> -h` prints, split into the text before the flags and the flags.
type cliHelp struct {
	Preamble []string
	Flags    []cliFlag
}

var flagHeaderRe = regexp.MustCompile(`^  -(\S+)(?: (\S+))?$`)

func parseHelp(text string) cliHelp {
	var h cliHelp
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := flagHeaderRe.FindStringSubmatch(line); m != nil {
			f := cliFlag{Name: m[1], Type: m[2]}
			if f.Type == "" {
				f.Type = "bool"
			}
			var usage []string
			for i+1 < len(lines) && strings.HasPrefix(lines[i+1], "    \t") {
				usage = append(usage, strings.TrimPrefix(lines[i+1], "    \t"))
				i++
			}
			f.Usage = strings.Join(usage, " ")
			h.Flags = append(h.Flags, f)
			continue
		}
		if strings.HasPrefix(line, "Usage of ") && len(h.Flags) == 0 && len(h.Preamble) == 0 {
			continue
		}
		h.Preamble = append(h.Preamble, line)
	}
	return h
}

// helpOf runs `asp <words> -h` as a user would, on a machine with no configuration, and returns
// what it prints: the defaults of the flags then do not depend on the machine of whoever runs it.
func helpOf(t *testing.T, words []string) string {
	t.Helper()
	t.Setenv("HOME", "/home/user")
	t.Setenv("ASP_CONFIG", "/dev/null")
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, s := range settingsTable {
		t.Setenv(s.Env, "")
		os.Unsetenv(s.Env)
	}
	var stdout, stderr bytes.Buffer
	run(append(append([]string(nil), words...), "-h"), &stdout, &stderr)
	text := stderr.String()
	if text == "" {
		text = stdout.String()
	}
	if strings.HasPrefix(text, "unknown command") || strings.Contains(text, "unknown argument") || strings.Contains(text, "unknown subcommand") {
		t.Fatalf("asp %s -h: %s", strings.Join(words, " "), strings.SplitN(text, "\n", 2)[0])
	}
	if strings.TrimSpace(text) == "" {
		t.Fatalf("asp %s -h prints nothing", strings.Join(words, " "))
	}
	return text
}

// commandsMarkdown is docs/reference/cli.md: one section per command, from the text the command
// prints for -h and the usage of the root command. The flags every command that calls the API
// takes are said once.
func commandsMarkdown(t *testing.T) string {
	t.Helper()
	var root bytes.Buffer
	printRootUsage(&root)
	commands := commandsOfUsage(root.String())
	if len(commands) < 30 {
		t.Fatalf("only %d commands found in the usage of asp: is the parser reading it?", len(commands))
	}
	helps := make([]cliHelp, len(commands))
	for i, c := range commands {
		helps[i] = parseHelp(helpOf(t, c.Words))
	}

	// What every command that talks to the control plane takes, with the same meaning.
	key := func(f cliFlag) string { return f.Name + "\x00" + f.Type + "\x00" + f.Usage }
	var common []cliFlag
	var commonSet map[string]bool
	for _, h := range helps {
		has := false
		for _, f := range h.Flags {
			has = has || f.Name == "control-plane-url"
		}
		if !has {
			continue
		}
		if commonSet == nil {
			commonSet = map[string]bool{}
			for _, f := range h.Flags {
				commonSet[key(f)] = true
			}
			continue
		}
		here := map[string]bool{}
		for _, f := range h.Flags {
			here[key(f)] = true
		}
		for k := range commonSet {
			if !here[k] {
				delete(commonSet, k)
			}
		}
	}
	for _, f := range helps[0].Flags { // any command that has them lists them in the same order
		if commonSet[key(f)] {
			common = append(common, f)
		}
	}
	for _, need := range []string{"control-plane-url", "api-key", "id-token", "tenant"} {
		found := false
		for _, f := range common {
			found = found || f.Name == need
		}
		if !found {
			t.Fatalf("--%s is not among the flags every API command shares: %v", need, common)
		}
	}

	var b strings.Builder
	b.WriteString("<!-- Generado por `make docs` a partir de `asp <orden> -h` y del uso de `asp` (cli/cmd/asp). No lo edites a mano: un test falla si no coincide con el código. -->\n")
	b.WriteString("# Referencia de `asp`\n\n")
	b.WriteString("`asp` es la línea de órdenes de la API del plano de control. Uso: `asp [--config FILE] <orden> …`; `asp <orden> -h` imprime la ayuda de una orden, y esta página sale de ese texto. ")
	b.WriteString("Las opciones se escriben con uno o dos guiones (`-tenant` o `--tenant`). ")
	b.WriteString("`asp sandbox exec`, `asp sandbox run` y `asp session exec` terminan con el código de salida del comando que ejecutaron; `1` es un fallo de `asp` o de la API, y `2`, un uso incorrecto.\n\n")
	b.WriteString("Las variables de entorno y el fichero de configuración del cliente (la URL del plano de control, las credenciales, el directorio de sesiones) están en [configuración de `asp`](configuration/cli.md).\n\n")
	b.WriteString("## Opciones comunes\n\nLas órdenes que hablan con el plano de control (todas las de `sandbox`, `session`, `node` y `apikey`) aceptan estas opciones; cada orden de más abajo lista solo las suyas.\n\n")
	writeFlagTable(&b, common)

	// The commands of a noun together, in the order the usage first mentions the noun.
	var groups []string
	byGroup := map[string][]int{}
	for i, c := range commands {
		if _, seen := byGroup[c.Words[0]]; !seen {
			groups = append(groups, c.Words[0])
		}
		byGroup[c.Words[0]] = append(byGroup[c.Words[0]], i)
	}
	for _, group := range groups {
		fmt.Fprintf(&b, "\n## %s\n", group)
		for _, i := range byGroup[group] {
			c, h := commands[i], helps[i]
			fmt.Fprintf(&b, "\n### `asp %s`\n", c.name())
			// A command that prints a usage of its own (doctor, session local-net, config show) is
			// described by it; the others by their line in the usage of asp.
			preamble := strings.TrimSpace(strings.Join(h.Preamble, "\n"))
			own := strings.HasPrefix(preamble, "usage:") || strings.Contains(preamble, "asp "+c.name())
			if !own {
				fmt.Fprintf(&b, "\n```\n%s\n```\n", c.Synopsis)
			}
			if c.Summary != "" {
				fmt.Fprintf(&b, "\n%s\n", refdoc.Sentence(refdoc.UsageMarkdown(c.Summary)))
			}
			if preamble != "" {
				fmt.Fprintf(&b, "\n```\n%s\n```\n", preamble)
			}
			var flags []cliFlag
			takesCommon := false
			for _, f := range h.Flags {
				if commonSet[key(f)] {
					takesCommon = true
					continue
				}
				flags = append(flags, f)
			}
			if len(flags) > 0 {
				b.WriteString("\n")
				writeFlagTable(&b, flags)
			}
			if takesCommon {
				b.WriteString("\nAcepta además las [opciones comunes](#opciones-comunes).\n")
			}
		}
	}
	return b.String()
}

func writeFlagTable(b *strings.Builder, flags []cliFlag) {
	b.WriteString("| Opción | Tipo | Qué hace |\n|---|---|---|\n")
	for _, f := range flags {
		fmt.Fprintf(b, "| `--%s` | %s | %s |\n", f.Name, f.Type, refdoc.Cell(refdoc.Sentence(refdoc.UsageMarkdown(f.Usage))))
	}
}

// Every variable of the table is a row of the page, and a credential carries the lock.
func TestSettingsReferenceListsEveryVariable(t *testing.T) {
	page := settingsMarkdown()
	for _, s := range settingsTable {
		row := "| `" + s.Env + "`"
		if n := strings.Count(page, "\n"+row); n != 1 {
			t.Errorf("%s has %d rows in the page, want 1", s.Env, n)
		}
		if s.Secret && !strings.Contains(page, row+" 🔒 |") {
			t.Errorf("%s is a credential and the page does not say so", s.Env)
		}
	}
}

func TestCommandsOfUsage(t *testing.T) {
	usage := `asp

Usage:
  asp [--config FILE] <command> ...
  asp sandbox create [flags]
  asp sandbox stop <id>        power off, keep the disk
  asp session start|exec [--name] [flags]
  asp node fence set <id> --endpoint URL [--token-env NAME | --token-stdin]
  asp version [--json]

Config files: ...
`
	var got []string
	for _, c := range commandsOfUsage(usage) {
		got = append(got, c.Synopsis+"|"+c.Summary)
	}
	want := []string{
		"asp sandbox create [flags]|",
		"asp sandbox stop <id>|power off, keep the disk",
		"asp session start [--name] [flags]|",
		"asp session exec [--name] [flags]|",
		"asp node fence set <id> --endpoint URL [--token-env NAME | --token-stdin]|",
		"asp version [--json]|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestParseHelp(t *testing.T) {
	h := parseHelp("Usage of sandbox create:\n  -json\n    \tprint raw JSON to stdout\n  -memory-mib int\n    \tmemory_mib (default 512)\n  -x value\n    \tfirst line\n    \tsecond line\n")
	want := []cliFlag{{"json", "bool", "print raw JSON to stdout"}, {"memory-mib", "int", "memory_mib (default 512)"}, {"x", "value", "first line second line"}}
	if len(h.Flags) != len(want) || len(h.Preamble) != 0 {
		t.Fatalf("%+v", h)
	}
	for i := range want {
		if h.Flags[i] != want[i] {
			t.Errorf("flag %d = %+v, want %+v", i, h.Flags[i], want[i])
		}
	}
}
