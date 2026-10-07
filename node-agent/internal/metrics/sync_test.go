package metrics

import (
	"os"
	"testing"
)

// The control plane and the node-agent are separate modules, so each has its own
// copy of this package: they must be the same file.
func TestSameAsTheControlPlanes(t *testing.T) {
	for _, f := range []string{"metrics.go", "runtime.go", "serve.go", "metrics_test.go"} {
		mine, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := os.ReadFile("../../../control-plane/internal/metrics/" + f)
		if err != nil {
			t.Skipf("the control plane's copy is not here (a checkout of the node-agent alone): %v", err)
		}
		if string(mine) != string(theirs) {
			t.Errorf("%s differs from control-plane/internal/metrics/%s: change both", f, f)
		}
	}
}
