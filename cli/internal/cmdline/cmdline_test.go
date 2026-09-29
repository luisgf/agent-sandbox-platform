package cmdline

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"echo hello", []string{"echo", "hello"}},
		{`echo "hello world"`, []string{"echo", "hello world"}},
		{`sh -c 'echo hi'`, []string{"sh", "-c", "echo hi"}},
		{`printf 'a\nb'`, []string{"printf", `a\nb`}},
	}
	for _, tc := range cases {
		got, err := Split(tc.in)
		if err != nil {
			t.Fatalf("Split(%q): %v", tc.in, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("Split(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestSplitUnclosed(t *testing.T) {
	if _, err := Split(`echo "hi`); err == nil {
		t.Fatal("expected error")
	}
}

func TestFromFlagAndArgs(t *testing.T) {
	got, err := FromFlagAndArgs("", []string{"echo", "x"})
	if err != nil || !reflect.DeepEqual(got, []string{"echo", "x"}) {
		t.Fatalf("trailing: %v %v", got, err)
	}
	got, err = FromFlagAndArgs("echo y", nil)
	if err != nil || !reflect.DeepEqual(got, []string{"echo", "y"}) {
		t.Fatalf("flag: %v %v", got, err)
	}
	if _, err := FromFlagAndArgs("", nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestSplitDashDash(t *testing.T) {
	b, a := SplitDashDash([]string{"--cmd", "x", "--", "echo", "hi"})
	if !reflect.DeepEqual(b, []string{"--cmd", "x"}) || !reflect.DeepEqual(a, []string{"echo", "hi"}) {
		t.Fatalf("got before=%v after=%v", b, a)
	}
}
