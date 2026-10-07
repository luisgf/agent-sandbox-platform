package main

import "testing"

func TestVirtiofsSandboxDefault(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		euid    int
		want    string
		wantErr bool
	}{
		{"", 0, "chroot", false},
		{"", 1000, "none", false},
		{"namespace", 0, "namespace", false},
		{" CHROOT ", 1000, "chroot", false},
		{"none", 0, "none", false},
		{"pivot", 0, "", true},
	} {
		got, err := virtiofsSandbox(tc.mode, tc.euid)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("virtiofsSandbox(%q, %d) = %q, %v; want %q (error %v)", tc.mode, tc.euid, got, err, tc.want, tc.wantErr)
		}
	}
}
