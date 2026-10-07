package attest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// EnvAllowedImages names the file with the images a boot may be attested as.
const EnvAllowedImages = "ASP_ATTEST_ALLOWED_IMAGES"

// AllowedImage is one known-good combination: the kernel a node loads, the base
// image its sandbox disks are copied from, and optionally the hypervisor that
// boots them. node-agent --print-measurement prints one for the node it runs on.
type AllowedImage struct {
	// Name is for people: it is shown in the attestation and the OIDC claim.
	Name string `json:"name"`
	// Kernel and Rootfs are "sha256:<hex>".
	Kernel string `json:"kernel"`
	Rootfs string `json:"rootfs"`
	// VMM is the exact version string the node reports (for Cloud Hypervisor,
	// "cloud-hypervisor v43.0"). Empty accepts any version.
	VMM string `json:"vmm,omitempty"`
}

type allowlistFile struct {
	Images []AllowedImage `json:"images"`
}

// Allowlist is the set of images the control plane accepts boot evidence for. It
// follows its file: an edit is picked up at the next check, and an edit that
// does not parse leaves the last good list in force.
type Allowlist struct {
	path string

	mu     sync.Mutex
	mtime  time.Time
	size   int64
	images []AllowedImage
}

// LoadAllowlist reads the allowlist at path. It fails when the file is missing,
// is not valid, or lists no image: an empty list would refuse every boot.
func LoadAllowlist(path string) (*Allowlist, error) {
	a := &Allowlist{path: path}
	images, fi, err := readAllowlist(path)
	if err != nil {
		return nil, err
	}
	a.images, a.mtime, a.size = images, fi.ModTime(), fi.Size()
	return a, nil
}

func readAllowlist(path string) ([]AllowedImage, os.FileInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	var f allowlistFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(f.Images) == 0 {
		return nil, nil, fmt.Errorf("%s lists no image, so every boot would be refused", path)
	}
	for i, img := range f.Images {
		if !ValidDigest(img.Kernel) || !ValidDigest(img.Rootfs) {
			return nil, nil, fmt.Errorf("%s: image %d (%q): kernel and rootfs must be sha256:<64 lowercase hex digits>", path, i+1, img.Name)
		}
		if len(img.VMM) > MaxVMMVersionLen {
			return nil, nil, fmt.Errorf("%s: image %d (%q): vmm is longer than %d bytes", path, i+1, img.Name, MaxVMMVersionLen)
		}
	}
	return f.Images, fi, nil
}

// Path is the file the list was loaded from.
func (a *Allowlist) Path() string { return a.path }

func (a *Allowlist) current() []AllowedImage {
	a.mu.Lock()
	defer a.mu.Unlock()
	fi, err := os.Stat(a.path)
	if err != nil {
		return a.images // keep the last good list; the file may be mid-replace
	}
	if fi.ModTime().Equal(a.mtime) && fi.Size() == a.size {
		return a.images
	}
	images, fi2, err := readAllowlist(a.path)
	// Whatever happened, do not look at this version of the file again.
	a.mtime, a.size = fi.ModTime(), fi.Size()
	if err != nil {
		slog.Error("attestation image allowlist not reloaded; the previous list stays in force", "error", err)
		return a.images
	}
	a.images, a.mtime, a.size = images, fi2.ModTime(), fi2.Size()
	slog.Info("attestation image allowlist reloaded", "path", a.path, "images", len(images))
	return a.images
}

// Check returns the allowed image a statement matches. A statement that is not
// measured matches nothing.
func (a *Allowlist) Check(st BootStatement) (AllowedImage, error) {
	if !st.Measured() {
		return AllowedImage{}, errors.New("the statement carries no kernel and base image digests, and this control plane only accepts images it knows")
	}
	images := a.current()
	var kernelKnown, rootfsKnown, pairKnown bool
	for _, img := range images {
		k, r := img.Kernel == st.KernelDigest, img.Rootfs == st.ImageDigest
		kernelKnown = kernelKnown || k
		rootfsKnown = rootfsKnown || r
		if !k || !r {
			continue
		}
		pairKnown = true
		if img.VMM == "" || img.VMM == st.VMMVersion {
			return img, nil
		}
	}
	switch {
	case !kernelKnown:
		return AllowedImage{}, fmt.Errorf("kernel %s is not in the image allowlist", st.KernelDigest)
	case !rootfsKnown:
		return AllowedImage{}, fmt.Errorf("base image %s is not in the image allowlist", st.ImageDigest)
	case !pairKnown:
		return AllowedImage{}, errors.New("no allowed image combines this kernel and this base image")
	}
	return AllowedImage{}, fmt.Errorf("hypervisor version %q is not allowed with this kernel and base image", st.VMMVersion)
}
