package store

// AutoProvisionEnabled is true when ASP_AUTO_PROVISION=1/true.
// Default is false: Create leaves sandboxes in requested for the node reconciler.
// Set ASP_AUTO_PROVISION=1 to keep the sync stub provisioner (requested→running).
func AutoProvisionEnabled() bool {
	return envTruthy("ASP_AUTO_PROVISION")
}

// ValidAgentStatus reports whether state is allowed on POST .../status.
func ValidAgentStatus(state SandboxState) bool {
	switch state {
	case SandboxStarting, SandboxRunning, SandboxFailed, SandboxStopped:
		return true
	default:
		return false
	}
}

// IsActiveLifecycle is true for states that Destroy should move to stopping.
func IsActiveLifecycle(state SandboxState) bool {
	switch state {
	case SandboxRequested, SandboxScheduled, SandboxStarting, SandboxRunning, SandboxPaused:
		return true
	default:
		return false
	}
}

// PickReadyNodeID returns the first ready node id (stable by map iteration order is fine for MVP),
// or empty if none. Prefer nodes with a non-empty AgentEndpoint.
func PickReadyNodeID(nodes []Node) string {
	var fallback string
	for _, n := range nodes {
		if n.State != "ready" {
			continue
		}
		if n.AgentEndpoint != "" && n.AgentEndpoint != "local://stub" {
			return n.ID
		}
		if fallback == "" && n.Endpoint != "local://stub" {
			fallback = n.ID
		}
	}
	if fallback != "" {
		return fallback
	}
	for _, n := range nodes {
		if n.State == "ready" {
			return n.ID
		}
	}
	return ""
}
