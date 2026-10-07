package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvWorkspaceRoots lists the directories a sandbox's workspace may live under
// (comma-separated, absolute): <root>/<tenant>/… . The node-agent enforces the
// same rule on its own roots, resolving symbolic links; this check only answers
// at create time with a 400 instead of a sandbox that fails to start, and it
// cannot see the filesystem. Unset: no early check, the node decides.
const EnvWorkspaceRoots = "ASP_WORKSPACE_ROOTS"

// workspaceRoots returns the configured roots, cleaned.
func workspaceRoots() []string {
	var out []string
	for _, part := range strings.Split(os.Getenv(EnvWorkspaceRoots), ",") {
		if part = strings.TrimSpace(part); part != "" && filepath.IsAbs(part) && filepath.Clean(part) != "/" {
			out = append(out, filepath.Clean(part))
		}
	}
	return out
}

// checkWorkspacePath reports why a tenant may not export path: it must be
// inside <root>/<tenant> for one of the roots. Lexical only: ".." is resolved
// by Clean, links are the node's business.
func checkWorkspacePath(roots []string, tenant, path string) error {
	if len(roots) == 0 || path == "" {
		return nil
	}
	if tenant == "" || tenant == "." || tenant == ".." || strings.ContainsAny(tenant, "/\x00") {
		return fmt.Errorf("workspace_host_path: tenant %q cannot name a directory", tenant)
	}
	clean := filepath.Clean(path)
	for _, root := range roots {
		base := filepath.Join(root, tenant)
		if clean == base || strings.HasPrefix(clean, base+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("workspace_host_path %s is not allowed: a workspace must be inside %s (the tenant's directory under a workspace root of this deployment)",
		path, filepath.Join(roots[0], tenant))
}
