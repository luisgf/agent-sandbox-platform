package envcfg

import (
	"strings"
	"sync"
	"testing"
)

func TestParseBool(t *testing.T) {
	for in, want := range map[string]bool{
		"1": true, "true": true, "TRUE": true, " Yes ": true, "on": true,
		"0": false, "false": false, "False": false, "no": false, "OFF": false,
	} {
		got, ok := ParseBool(in)
		if !ok || got != want {
			t.Errorf("ParseBool(%q) = %v %v, want %v true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", " ", "2", "maybe", "y", "enable", "tru"} {
		if _, ok := ParseBool(in); ok {
			t.Errorf("ParseBool(%q) understood it", in)
		}
	}
}

func TestTruthy(t *testing.T) {
	t.Setenv("ASP_TEST_TRUTHY", "yes")
	if !Truthy("ASP_TEST_TRUTHY") {
		t.Error("yes is not true")
	}
	for _, v := range []string{"0", "false", "", "banana"} {
		t.Setenv("ASP_TEST_TRUTHY", v)
		if Truthy("ASP_TEST_TRUTHY") {
			t.Errorf("%q is true", v)
		}
	}
}

func capture(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var msgs []string
	old := Warn
	Warn = func(m string) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() }
	ResetWarnings()
	t.Cleanup(func() { Warn = old })
	return &msgs
}

func mapLookup(m map[string]string) Lookup {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok && strings.TrimSpace(v) != ""
	}
}

func TestGetPrefersTheNameAndWarnsForTheOldOne(t *testing.T) {
	msgs := capture(t)
	v, from, ok := Get(mapLookup(map[string]string{"ASP_NEW": "n"}), "ASP_NEW", "OLD")
	if !ok || v != "n" || from != "ASP_NEW" || len(*msgs) != 0 {
		t.Fatalf("new name: %q %q %v %v", v, from, ok, *msgs)
	}

	v, from, ok = Get(mapLookup(map[string]string{"OLD": "o"}), "ASP_NEW", "OLD")
	if !ok || v != "o" || from != "OLD" {
		t.Fatalf("old name: %q %q %v", v, from, ok)
	}
	if len(*msgs) != 1 || !strings.Contains((*msgs)[0], "OLD is deprecated: use ASP_NEW") {
		t.Fatalf("warnings: %v", *msgs)
	}
	// Once per process, not once per read.
	Get(mapLookup(map[string]string{"OLD": "o"}), "ASP_NEW", "OLD")
	if len(*msgs) != 1 {
		t.Fatalf("warned again: %v", *msgs)
	}

	// Both set: the new name wins, and the old one is said to be ignored when it disagrees.
	v, from, _ = Get(mapLookup(map[string]string{"ASP_NEW": "n", "OLD": "x"}), "ASP_NEW", "OLD")
	if v != "n" || from != "ASP_NEW" {
		t.Fatalf("both: %q %q", v, from)
	}
	if len(*msgs) != 2 || !strings.Contains((*msgs)[1], "OLD is ignored") {
		t.Fatalf("warnings: %v", *msgs)
	}

	if _, _, ok := Get(mapLookup(nil), "ASP_NEW", "OLD"); ok {
		t.Fatal("nothing set, something found")
	}
}

func TestGetenvTreatsEmptyAsUnset(t *testing.T) {
	t.Setenv("ASP_TEST_EMPTY", "  ")
	if _, ok := Getenv("ASP_TEST_EMPTY"); ok {
		t.Fatal("blank is set")
	}
	t.Setenv("ASP_TEST_EMPTY", "x")
	if v, ok := Getenv("ASP_TEST_EMPTY"); !ok || v != "x" {
		t.Fatalf("%q %v", v, ok)
	}
}
