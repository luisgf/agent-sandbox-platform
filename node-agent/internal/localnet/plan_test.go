package localnet

import "strings"
import "testing"

func TestDecideDefaultIsPublic(t *testing.T) {
	p := Decide("sb-1", "user", false, "")
	if p.Kind != KindPublic || !p.UsePublicProxy {
		t.Fatalf("off plan=%+v", p)
	}
	if len(Commands(p)) != 0 {
		t.Fatalf("public plan must not add tunnel routes: %v", Commands(p))
	}
}

func TestDecideFlagOnIsNotPublic(t *testing.T) {
	pending := Decide("abcdef012345", "owner-a", true, "pending")
	if pending.Kind != KindBlackhole || pending.UsePublicProxy || !pending.BlockNodeProxy {
		t.Fatalf("pending=%+v", pending)
	}
	up := Decide("abcdef012345", "owner-a", true, "up")
	if up.Kind != KindTunnel || up.Iface != "wg-asp-abcdef01" || up.UsePublicProxy {
		t.Fatalf("up=%+v", up)
	}
	down := Decide("abcdef012345", "owner-a", true, "withdrawn")
	if down.Kind != KindBlackhole || down.UsePublicProxy {
		t.Fatalf("withdrawn=%+v", down)
	}
	for _, p := range []Plan{pending, up, down} {
		for _, c := range Commands(p) {
			if strings.Contains(c, "8888") || strings.Contains(c, "asp_egress") || strings.Contains(c, "proxy") {
				t.Fatalf("public fallback in %q for %+v", c, p)
			}
		}
		if p.UsesPublicProxy() {
			t.Fatalf("uses public %+v", p)
		}
	}
	// blackhole recipe is not the tunnel default, and not the proxy
	got := strings.Join(Commands(down), "\n")
	if !strings.Contains(got, "blackhole 0.0.0.0/0") {
		t.Fatalf("commands=%s", got)
	}
}

func TestMemoryApplyDisconnect(t *testing.T) {
	m := NewMemory()
	id := "sb"
	if err := m.Apply(Decide(id, "o", true, "pending")); err != nil {
		t.Fatal(err)
	}
	cur, ok := m.Current(id)
	if !ok || cur.Kind != KindBlackhole {
		t.Fatalf("pending current=%+v", cur)
	}
	if err := m.Apply(Decide(id, "o", true, "up")); err != nil {
		t.Fatal(err)
	}
	cur, _ = m.Current(id)
	if cur.Kind != KindTunnel {
		t.Fatalf("up=%+v", cur)
	}
	if err := m.Apply(Decide(id, "o", true, "withdrawn")); err != nil {
		t.Fatal(err)
	}
	cur, _ = m.Current(id)
	if cur.Kind != KindBlackhole || cur.UsePublicProxy {
		t.Fatalf("disconnect fell back: %+v", cur)
	}
	bad := Decide(id, "o", true, "withdrawn")
	bad.UsePublicProxy = true
	bad.Kind = KindBlackhole
	if err := m.Apply(bad); err == nil {
		t.Fatal("expected refusal of public fallback")
	}
}
