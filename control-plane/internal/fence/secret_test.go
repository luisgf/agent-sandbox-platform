package fence

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSecret(t *testing.T) {
	t.Setenv("BMC_PW", "  from-env \n")
	file := filepath.Join(t.TempDir(), "bmc")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		{"literal-secret", "literal-secret", false},
		{"", "", false},
		{"env:BMC_PW", "from-env", false},
		{"file:" + file, "from-file", false},
		{"env:NOT_SET_ANYWHERE", "", true},
		{"file:" + file + ".missing", "", true},
	} {
		got, err := ResolveSecret(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ResolveSecret(%q) = %q, %v; want %q (error %v)", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestValidateSecretRef(t *testing.T) {
	for _, ok := range []string{"", "secret", "env:BMC_PW", "file:/etc/asp/bmc.pw"} {
		if err := ValidateSecretRef(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"env:", "env:   ", "file:relative/path", "file:", strings.Repeat("x", 5000)} {
		if err := ValidateSecretRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The BMC password must not be on the command line, where ps shows it.
func TestIPMIPasswordIsNotInTheArguments(t *testing.T) {
	var args []string
	var cmd *exec.Cmd
	p := &IPMI{
		LookPath: func(string) (string, error) { return "/usr/bin/ipmitool", nil },
		Exec: func(ctx context.Context, name string, a ...string) *exec.Cmd {
			args = a
			cmd = exec.CommandContext(ctx, "true") // stands in for ipmitool
			return cmd
		},
	}
	if err := p.Fence(context.Background(), Target{Endpoint: "10.0.0.1", User: "admin", Token: "s3cret-bmc-pw"}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "s3cret-bmc-pw") || strings.Contains(joined, "-P ") {
		t.Fatalf("the password is on the command line: %v", args)
	}
	if !strings.Contains(" "+joined+" ", " -E ") {
		t.Fatalf("-E (password from the environment) missing: %v", args)
	}
	found := false
	for _, e := range cmd.Env {
		if e == "IPMI_PASSWORD=s3cret-bmc-pw" {
			found = true
		}
	}
	if !found {
		t.Fatal("IPMI_PASSWORD is not in the command's environment")
	}
}
