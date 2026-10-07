package refdoc

import "testing"

func TestUsageMarkdown(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a workspace must be inside <root>/<tenant>/ (symbolic links resolved)", "a workspace must be inside `<root>/<tenant>/` (symbolic links resolved)"},
		{"on | report (log, remove nothing) | off; --dry-run only reports", "on | report (log, remove nothing) | off; `--dry-run` only reports"},
		{"default /var/lib/asp/disks.", "default `/var/lib/asp/disks`."},
		{"listen addr (e.g. 0.0.0.0:9443) when", "listen addr (e.g. `0.0.0.0:9443`) when"},
		{"host[:port] laptops dial", "`host[:port]` laptops dial"},
		{"set ASP_FOO_BAR=1 or a < b", "set `ASP_FOO_BAR=1` or a &lt; b"},
		{"where `asp auth login` keeps the token", "where `asp auth login` keeps the token"},
		{"default ~/.cache/asp/sessions", "default `~/.cache/asp/sessions`"},
		{"the legacy/debug mode, and/or more", "the legacy/debug mode, and/or more"},
		{"from cert-dir/ca.crt and 10.200.0.0/16 and a/b/c", "from `cert-dir/ca.crt` and `10.200.0.0/16` and `a/b/c`"},
	} {
		if got := UsageMarkdown(c.in); got != c.want {
			t.Errorf("UsageMarkdown(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestSentenceAndCell(t *testing.T) {
	for in, want := range map[string]string{
		"control plane base URL":           "Control plane base URL.",
		"comma-separated list":             "Comma-separated list.",
		"`1` forces it":                    "`1` forces it.",
		"Already a sentence.":              "Already a sentence.",
		"--flag does it":                   "--flag does it.",
		"":                                 "",
		"ends with a question mark?":       "Ends with a question mark?",
		"pipes | and\nnewlines are a cell": "Pipes | and\nnewlines are a cell.",
	} {
		if got := Sentence(in); got != want {
			t.Errorf("Sentence(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Cell("a | b\nc"); got != `a \| b c` {
		t.Errorf("Cell = %q", got)
	}
}
