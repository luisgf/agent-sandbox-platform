package attest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dg(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func writeList(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func measured(kernel, rootfs byte, vmm string) BootStatement {
	return BootStatement{
		SandboxID: "sb", NodeID: "n1", TS: "2026-10-07T10:00:00Z",
		ImageDigest: dg(rootfs), KernelDigest: dg(kernel), VMMVersion: vmm, Boot: BootNew,
	}
}

func TestAllowlistCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.json")
	writeList(t, path, `{"images":[
		{"name":"debian-a","kernel":"`+dg('e')+`","rootfs":"`+dg('a')+`","vmm":"cloud-hypervisor v43.0"},
		{"name":"debian-b","kernel":"`+dg('e')+`","rootfs":"`+dg('b')+`"}]}`)
	al, err := LoadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		st   BootStatement
		want string // image name; "" = refused
		err  string
	}{
		{"exact", measured('e', 'a', "cloud-hypervisor v43.0"), "debian-a", ""},
		{"any vmm when the entry names none", measured('e', 'b', "cloud-hypervisor v99"), "debian-b", ""},
		{"wrong vmm", measured('e', 'a', "cloud-hypervisor v44.0"), "", "hypervisor version"},
		{"unknown kernel", measured('d', 'a', "cloud-hypervisor v43.0"), "", "kernel sha256:"},
		{"unknown image", measured('e', 'd', "cloud-hypervisor v43.0"), "", "base image sha256:"},
		{"known parts, never together", measured('e', 'a', "x"), "", "hypervisor version"},
		{"unmeasured", BootStatement{SandboxID: "sb", NodeID: "n1", TS: "2026-10-07T10:00:00Z", ImageDigest: "debian-asp"}, "", "no kernel and base image digests"},
		{"only the image measured", BootStatement{SandboxID: "sb", NodeID: "n1", TS: "2026-10-07T10:00:00Z", ImageDigest: dg('a')}, "", "no kernel and base image digests"},
	} {
		img, err := al.Check(tc.st)
		if tc.want == "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err=%v, want one containing %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || img.Name != tc.want {
			t.Errorf("%s: got %+v, %v; want %s", tc.name, img, err, tc.want)
		}
	}

	// A kernel and an image that are each listed, but never as a pair.
	writeList(t, path, `{"images":[
		{"name":"one","kernel":"`+dg('e')+`","rootfs":"`+dg('a')+`"},
		{"name":"two","kernel":"`+dg('f')+`","rootfs":"`+dg('b')+`"}]}`)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(path, future, future)
	if _, err := al.Check(measured('e', 'b', "")); err == nil || !strings.Contains(err.Error(), "combines") {
		t.Errorf("a mix of two images: %v", err)
	}
}

func TestAllowlistRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"not json":       `images:`,
		"empty list":     `{"images":[]}`,
		"no images key":  `{}`,
		"bad kernel":     `{"images":[{"name":"x","kernel":"sha256:abc","rootfs":"` + dg('a') + `"}]}`,
		"bad rootfs":     `{"images":[{"name":"x","kernel":"` + dg('e') + `","rootfs":"latest"}]}`,
		"upper-case hex": `{"images":[{"name":"x","kernel":"sha256:` + strings.Repeat("A", 64) + `","rootfs":"` + dg('a') + `"}]}`,
		"vmm too long":   `{"images":[{"name":"x","kernel":"` + dg('e') + `","rootfs":"` + dg('a') + `","vmm":"` + strings.Repeat("v", 200) + `"}]}`,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		writeList(t, path, body)
		if _, err := LoadAllowlist(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadAllowlist(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestAllowlistFollowsItsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.json")
	writeList(t, path, `{"images":[{"name":"old","kernel":"`+dg('e')+`","rootfs":"`+dg('a')+`"}]}`)
	al, err := LoadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := al.Check(measured('e', 'a', "")); err != nil {
		t.Fatal(err)
	}
	if _, err := al.Check(measured('e', 'c', "")); err == nil {
		t.Fatal("an image that is not listed yet was accepted")
	}

	// A rebuilt image is added without restarting anything.
	writeList(t, path, `{"images":[{"name":"old","kernel":"`+dg('e')+`","rootfs":"`+dg('a')+`"},{"name":"new","kernel":"`+dg('e')+`","rootfs":"`+dg('c')+`"}]}`)
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(path, future, future)
	if img, err := al.Check(measured('e', 'c', "")); err != nil || img.Name != "new" {
		t.Fatalf("after the edit: %+v, %v", img, err)
	}

	// A broken edit leaves the last good list in force.
	writeList(t, path, `{"images": [`)
	future = future.Add(time.Minute)
	_ = os.Chtimes(path, future, future)
	if img, err := al.Check(measured('e', 'c', "")); err != nil || img.Name != "new" {
		t.Fatalf("after a broken edit: %+v, %v", img, err)
	}

	// So does a file that is gone for a moment.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := al.Check(measured('e', 'a', "")); err != nil {
		t.Fatalf("with the file gone: %v", err)
	}
}

func TestValidDigestAndMeasured(t *testing.T) {
	for s, want := range map[string]bool{
		dg('a'): true, "": false, "sha256:": false, "sha256:" + strings.Repeat("a", 63): false,
		"sha256:" + strings.Repeat("G", 64): false, "sha1:" + strings.Repeat("a", 64): false, "debian-asp": false,
	} {
		if ValidDigest(s) != want {
			t.Errorf("ValidDigest(%q) = %v", s, !want)
		}
	}
	if !measured('e', 'a', "").Measured() {
		t.Error("a statement with both digests is not measured")
	}
	if (BootStatement{ImageDigest: "img", KernelDigest: dg('e')}).Measured() {
		t.Error("an image reference counts as a digest")
	}
}

func TestStatementValidation(t *testing.T) {
	ok := measured('e', 'a', "cloud-hypervisor v43.0")
	if err := validateStatement(ok); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.KernelDigest = "latest"
	if err := validateStatement(bad); err == nil {
		t.Error("a kernel digest that is not one was accepted")
	}
	bad = ok
	bad.Boot = "reboot"
	if err := validateStatement(bad); err == nil {
		t.Error("an unknown boot kind was accepted")
	}
	bad = ok
	bad.VMMVersion = strings.Repeat("v", MaxVMMVersionLen+1)
	if err := validateStatement(bad); err == nil {
		t.Error("an overlong vmm version was accepted")
	}
	// An older node's statement, with no measurement fields, still validates.
	old := BootStatement{SandboxID: "sb", NodeID: "n1", TS: "2026-10-07T10:00:00Z", ImageDigest: "debian-asp"}
	if err := validateStatement(old); err != nil {
		t.Errorf("a statement from a node that does not measure: %v", err)
	}
}
