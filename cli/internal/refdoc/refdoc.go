// Package refdoc has what the generators of the reference pages (docs/reference) share: they turn
// the plain text of a flag's help into Markdown that survives a table cell. Only tests import it.
package refdoc

import (
	"regexp"
	"strings"
)

// Cell makes text safe inside a Markdown table cell.
func Cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\r\n", " ", "\n", " ").Replace(strings.TrimSpace(s))
}

// Sentence starts a help text with a capital and ends it with a full stop, so that a column of
// them reads as text: they are written as flag help, which has neither.
func Sentence(s string) string {
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

// UsageMarkdown turns the plain text of a help into Markdown: what looks like code (a flag, a
// variable, a path, an address, a placeholder) goes in code font, and the rest is escaped, so a
// <placeholder> is not taken for an HTML tag.
func UsageMarkdown(s string) string {
	words := strings.Split(s, " ")
	for i, w := range words {
		words[i] = word(w)
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

func word(w string) string {
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
		strings.HasPrefix(core, "ASP_"), strings.HasPrefix(core, "$"), strings.HasPrefix(core, "~"),
		strings.HasPrefix(core, "=") && len(core) > 1,
		strings.ContainsAny(core, "{}[]"),
		strings.HasPrefix(core, "<") && strings.Contains(core, ">"),
		pathish(core), addrLike.MatchString(core):
		return pre + "`" + core + "`" + post
	}
	return pre + strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(core) + post
}
