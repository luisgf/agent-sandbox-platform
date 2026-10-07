package version

import (
	"strings"
	"testing"
)

func TestShortForReleaseAndDevBuilds(t *testing.T) {
	for _, c := range []struct {
		in   Info
		want string
	}{
		{Info{Version: "0.1.0", Commit: "0123456789abcdef"}, "0.1.0"},
		{Info{Version: "dev", Commit: "0123456789abcdef"}, "dev+01234567"},
		{Info{Version: "dev", Commit: "0123456789abcdef", Modified: true}, "dev+01234567-dirty"},
		{Info{Version: "dev"}, "dev"},
		{Info{Version: "dev", Modified: true}, "dev-dirty"},
	} {
		if got := c.in.Short(); got != c.want {
			t.Errorf("%+v: Short() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStringSaysWhatItWasBuiltFrom(t *testing.T) {
	got := Info{Version: "1.2.3", Commit: "0123456789abcdef", Date: "2026-10-07T12:00:00Z", GoVersion: "go1.27"}.String()
	for _, want := range []string{"1.2.3", "commit 01234567", "built 2026-10-07T12:00:00Z", "go1.27"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not mention %q", got, want)
		}
	}
	if strings.Contains(got, "0123456789abcdef") {
		t.Errorf("%q has the whole commit; eight characters are enough", got)
	}
}

func TestGetNeverReturnsAnEmptyVersion(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = ""
	if got := Get(); got.Version != "dev" || got.GoVersion == "" {
		t.Fatalf("Get() = %+v", got)
	}
}

func TestLinkerValuesWinOverTheBuildInfo(t *testing.T) {
	oldC, oldD := Commit, Date
	defer func() { Commit, Date = oldC, oldD }()
	Commit, Date = "feedfacefeedface", "2026-01-01T00:00:00Z"
	got := Get()
	if got.Commit != "feedfacefeedface" || got.Date != "2026-01-01T00:00:00Z" {
		t.Fatalf("Get() = %+v", got)
	}
}
