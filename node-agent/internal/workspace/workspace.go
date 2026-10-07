// Package workspace decides which host directories a sandbox may export into
// its guest.
//
// The path comes from the sandbox spec, which the caller of the control plane
// writes. virtiofsd runs as root and shares whatever directory it is given
// read-write, so an unchecked path exports the node: "/" would put every other
// tenant's disks and the node's own keys in the guest. A workspace must live
// under <root>/<tenant>/ for one of the roots the operator gave the node, after
// symbolic links are resolved, so neither ".." nor a link inside the tenant's
// own directory can lead out of it.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRoot is the workspace root of a node that was given none. It does not
// exist until the operator creates it, so by default no workspace is allowed.
const DefaultRoot = "/srv/asp/workspaces"

// ErrNotAllowed means a path is outside every workspace root of the node.
var ErrNotAllowed = errors.New("workspace not allowed")

// Roots are the directories under which each tenant has its own.
type Roots []string

// ParseRoots parses a comma-separated list of absolute directories.
func ParseRoots(csv string) (Roots, error) {
	var out Roots
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !filepath.IsAbs(part) {
			return nil, fmt.Errorf("workspace root %q must be an absolute path", part)
		}
		clean := filepath.Clean(part)
		if clean == "/" {
			return nil, fmt.Errorf("workspace root / would export the whole node")
		}
		out = append(out, clean)
	}
	return out, nil
}

// ValidTenant reports whether a tenant id can be one directory name.
func ValidTenant(tenant string) bool {
	return tenant != "" && len(tenant) <= 128 && tenant != "." && tenant != ".." &&
		!strings.ContainsAny(tenant, "/\x00") && filepath.Base(tenant) == tenant
}

// Resolve returns the real path of a workspace the tenant may use: path, with
// symbolic links resolved, must be a directory inside <root>/<tenant> of one of
// the roots. The caller shares the returned path, not the one it was given.
func (r Roots) Resolve(tenant, path string) (string, error) {
	if !ValidTenant(tenant) {
		return "", fmt.Errorf("%w: tenant %q cannot name a directory", ErrNotAllowed, tenant)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("workspace host path %q must be absolute", path)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("workspace host path: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("workspace host path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace host path %s is not a directory", path)
	}
	if len(r) == 0 {
		return "", fmt.Errorf("%w: this node has no workspace root (--workspace-root); create %s/<tenant> or name another", ErrNotAllowed, DefaultRoot)
	}
	for _, root := range r {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue // a root that does not exist allows nothing
		}
		base, err := filepath.EvalSymlinks(filepath.Join(rr, tenant))
		if err != nil {
			continue
		}
		if real == base || strings.HasPrefix(real, base+string(filepath.Separator)) {
			return real, nil
		}
	}
	return "", fmt.Errorf("%w: %s is not under %s for tenant %s: put the project there, or ask the operator for another --workspace-root",
		ErrNotAllowed, path, filepath.Join(r[0], tenant), tenant)
}
