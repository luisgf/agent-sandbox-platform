package poddaemon

import (
	"context"
	"testing"
	"time"
)

// A buffered exec's deadline here is longer than the time the guest gives the
// command, so the guest's own timeout (exit 124, the output so far) answers first.
func TestBufferedForCommandsThatMayRunLong(t *testing.T) {
	c := &Client{BufferedTimeout: 60 * time.Second}
	for _, tc := range []struct {
		name    string
		command time.Duration
		want    time.Duration
	}{
		{"no timeout asked: the client's own", 0, 60 * time.Second},
		{"a short command keeps the client's", 10 * time.Second, 60 * time.Second},
		{"a long one gets the margin on top", 10 * time.Minute, 10*time.Minute + bufferedMargin},
		{"just under the client's limit but within the margin", 50 * time.Second, 50*time.Second + bufferedMargin},
	} {
		ctx, cancel := c.bufferedFor(context.Background(), tc.command)
		dl, ok := ctx.Deadline()
		cancel()
		if !ok {
			t.Fatalf("%s: no deadline", tc.name)
		}
		got := time.Until(dl)
		if got > tc.want || got < tc.want-2*time.Second {
			t.Errorf("%s: deadline in %v, want about %v", tc.name, got, tc.want)
		}
	}
	// A client with no limit stays without one.
	none := &Client{}
	ctx, cancel := none.bufferedFor(context.Background(), time.Hour)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("a client with no buffered timeout got one")
	}
}
