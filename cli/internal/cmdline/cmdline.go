// Package cmdline helpers for parsing guest command argv.
package cmdline

import (
	"fmt"
	"strings"
	"unicode"
)

// FromFlagAndArgs builds argv from --cmd string and/or trailing args after "--".
// Priority: if trailing is non-empty, use it; else split cmdFlag with shell-ish rules.
func FromFlagAndArgs(cmdFlag string, trailing []string) ([]string, error) {
	if len(trailing) > 0 {
		out := make([]string, 0, len(trailing))
		for _, a := range trailing {
			if a != "" {
				out = append(out, a)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("empty command after --")
		}
		return out, nil
	}
	cmdFlag = strings.TrimSpace(cmdFlag)
	if cmdFlag == "" {
		return nil, fmt.Errorf("command required: use --cmd '…' or -- argv…")
	}
	parts, err := Split(cmdFlag)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty --cmd")
	}
	return parts, nil
}

// Split tokenizes a command string with simple single/double quotes (no escapes beyond \\).
func Split(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	escaped := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		if escaped {
			cur.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if !inSingle && !inDouble && unicode.IsSpace(r) {
			flush()
			continue
		}
		cur.WriteRune(r)
	}
	if escaped {
		return nil, fmt.Errorf("trailing backslash in command")
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unclosed quote in command")
	}
	flush()
	return out, nil
}

// SplitDashDash splits args into before and after the first bare "--".
func SplitDashDash(args []string) (before, after []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}
