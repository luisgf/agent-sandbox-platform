package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/envcfg"
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

// A setting without a description or a heading would show up in the reference as an empty row, or
// under a heading of its own.
func TestEverySettingIsDescribedAndGrouped(t *testing.T) {
	groups := map[string]bool{groupListen: true, groupDatabase: true, groupAuth: true, groupNodes: true,
		groupScheduling: true, groupKeys: true, groupRetention: true}
	for _, s := range settingsTable {
		if strings.TrimSpace(s.Help) == "" {
			t.Errorf("%s has no description", s.Env)
		}
		if !groups[s.Group] {
			t.Errorf("%s is under the heading %q, which the page does not know", s.Env, s.Group)
		}
	}
}

// The page is built from the table, so what the table holds is what the page says: a variable
// appears once, with its key, and a credential carries the lock.
func TestReferenceListsEveryVariableOnce(t *testing.T) {
	page := referenceMarkdown()
	for _, s := range settingsTable {
		row := "| `" + s.Env + "`"
		if n := strings.Count(page, "\n"+row); n != 1 {
			t.Errorf("%s has %d rows in the page, want 1", s.Env, n)
		}
		if s.Secret && !strings.Contains(page, row+" 🔒 |") {
			t.Errorf("%s is a credential and the page does not say so", s.Env)
		}
	}
	if strings.Contains(page, "](docs/") || strings.Contains(page, "](../docs/") {
		t.Error("a link in a description is not rewritten for the page's location")
	}
}

// referencePage is where referenceMarkdown is written, relative to the repository root.
const referencePage = "docs/reference/configuration/control-plane.md"

// referenceMarkdown is that page, written from settingsTable: it is as complete as the table, and
// a test holds the table to the source, so the page cannot miss a variable the way a hand-written
// list did. A test fails when the file in the repository differs from this (make docs rewrites it).
func referenceMarkdown() string {
	var b strings.Builder
	b.WriteString("<!-- Generado por `make docs` a partir de settingsTable (control-plane/cmd/api/settingstable.go). No lo edites a mano: un test falla si no coincide con el código. -->\n")
	b.WriteString("# Configuración del plano de control\n\n")
	b.WriteString("Cada ajuste es una variable de entorno `ASP_*`. Prioridad: **variable de entorno > fichero de configuración > valor por defecto**. ")
	b.WriteString("El fichero es `/etc/asp/server.yaml` (más los `server.yaml.d/*.yaml` que lo acompañan), o el que nombren `--config FILE` o `ASP_CONFIG`; ")
	b.WriteString("la clave de un ajuste es su variable sin `ASP_` y en minúsculas ([el fichero de configuración](../../how-to/config-file.md)). ")
	b.WriteString("`asp-control-plane --print-config` lista cada ajuste con su valor y de dónde sale, sin las credenciales; las marcadas con 🔒 son credenciales y nunca se imprimen.\n\n")
	b.WriteString("Los ajustes de los otros programas: [node-agent](node-agent.md), [`asp`](cli.md) y [`asp-server`](asp-server.md).\n")

	var groups []string
	by := map[string][]setting{}
	for _, s := range settingsTable {
		if _, seen := by[s.Group]; !seen {
			groups = append(groups, s.Group)
		}
		by[s.Group] = append(by[s.Group], s)
	}
	for _, g := range groups {
		fmt.Fprintf(&b, "\n## %s\n\n| Variable | Clave del fichero | Por defecto | Qué hace |\n|---|---|---|---|\n", g)
		for _, s := range by[g] {
			name := "`" + s.Env + "`"
			if s.Secret {
				name += " 🔒"
			}
			fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n", name, envcfg.KeyOf(s.Env), defaultCell(s.Default), helpCell(s.Help))
		}
	}
	return b.String()
}

// defaultCell shows a default: a value in code font, a description (it has spaces) as text.
func defaultCell(def string) string {
	switch {
	case def == "":
		return "—"
	case strings.ContainsAny(def, " |"):
		return mdCell(def)
	}
	return "`" + def + "`"
}

// helpCell makes a description safe inside a table cell. A link in it is written from the
// repository root (docs/…), and the page is three directories down.
func helpCell(help string) string {
	return mdCell(strings.ReplaceAll(help, "](docs/", "](../../"))
}

// mdCell makes text safe inside a Markdown table cell.
func mdCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\r\n", " ", "\n", " ").Replace(strings.TrimSpace(s))
}
