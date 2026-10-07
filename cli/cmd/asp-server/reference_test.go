package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/envcfg"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/refdoc"
)

var update = flag.Bool("update", false, "rewrite the generated reference pages (make docs)")

// referencePage is where referenceMarkdown is written, relative to the repository root.
const referencePage = "docs/reference/configuration/asp-server.md"

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

// referenceMarkdown is docs/reference/configuration/asp-server.md, written from the flags of
// asp-server: they are all its settings (the environment is not a layer of this program).
func referenceMarkdown() string {
	fs, _, _ := newFlags(nil)
	var b strings.Builder
	b.WriteString("<!-- Generado por `make docs` a partir de las opciones de asp-server (cli/cmd/asp-server/main.go). No lo edites a mano: un test falla si no coincide con el código. -->\n")
	b.WriteString("# Configuración de `asp-server`\n\n")
	b.WriteString("`asp-server` ([un solo host](../../how-to/single-host.md)) se configura con opciones y con un fichero: **opción > fichero > valor por defecto**. ")
	b.WriteString("El fichero es `" + defaultConfigFile + "` (más los `standalone.yaml.d/*.yaml` que lo acompañan), o el que nombre `--config FILE`; ")
	b.WriteString("la clave de un ajuste es su opción con guiones bajos (`data_dir`) y una clave que no es un ajuste es un error ([el fichero de configuración](../../how-to/config-file.md)). ")
	b.WriteString("El entorno no es una capa: los programas que arranca, `asp-control-plane` y `asp-node-agent`, leen el suyo, y una variable pensada para uno no debe cambiar a este.\n\n")
	b.WriteString("Los ajustes de esos programas: [plano de control](control-plane.md) y [node-agent](node-agent.md); el cliente, [`asp`](cli.md).\n\n")
	b.WriteString("| Opción | Clave del fichero | Por defecto | Qué hace |\n|---|---|---|---|\n")
	fs.VisitAll(func(f *flag.Flag) {
		key := "—"
		if f.Name != "config" && f.Name != "version" {
			key = "`" + envcfg.Key(f.Name) + "`"
		}
		_, usage := flag.UnquoteUsage(f)
		def := "—"
		if f.DefValue != "" && f.DefValue != "false" {
			def = "`" + f.DefValue + "`"
		}
		fmt.Fprintf(&b, "| `--%s` | %s | %s | %s |\n", f.Name, key, def, refdoc.Cell(refdoc.Sentence(refdoc.UsageMarkdown(usage))))
	})
	return b.String()
}

// Every flag has a description and is a row of the page.
func TestReferenceListsEveryFlag(t *testing.T) {
	fs, _, _ := newFlags(nil)
	page := referenceMarkdown()
	n := 0
	fs.VisitAll(func(f *flag.Flag) {
		n++
		if strings.TrimSpace(f.Usage) == "" {
			t.Errorf("--%s has no description", f.Name)
		}
		if c := strings.Count(page, "\n| `--"+f.Name+"` |"); c != 1 {
			t.Errorf("--%s has %d rows in the page, want 1", f.Name, c)
		}
	})
	if n < 10 {
		t.Fatalf("only %d flags", n)
	}
}
