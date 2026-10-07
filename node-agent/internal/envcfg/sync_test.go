package envcfg

import (
	"os"
	"testing"
)

// The node-agent, the control plane and the CLI are separate modules, so each has its
// own copy of this package: they must be the same files.
func TestSameAsTheOtherBinaries(t *testing.T) {
	for _, f := range []string{"envcfg.go", "envcfg_test.go", "file.go", "file_test.go"} {
		mine, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, other := range []string{"control-plane", "cli"} {
			theirs, err := os.ReadFile("../../../" + other + "/internal/envcfg/" + f)
			if err != nil {
				t.Skipf("the copy of %s is not here (a checkout of the node-agent alone): %v", other, err)
			}
			if string(mine) != string(theirs) {
				t.Errorf("%s differs from %s/internal/envcfg/%s: change all three", f, other, f)
			}
		}
	}
}
