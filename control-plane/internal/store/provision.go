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

// ValidAgentTransition reports whether a node may move a sandbox from one state to
// another. A report that arrives late (after a destroy, a failover or a stop) must
// not bring the sandbox back: stopped is final, failed only goes to stopped, and
// stopping only finishes.
func ValidAgentTransition(from, to SandboxState) bool {
	switch from {
	case SandboxStopped:
		return to == SandboxStopped
	case SandboxFailed:
		return to == SandboxFailed || to == SandboxStopped
	case SandboxStopping:
		return to == SandboxStopped || to == SandboxFailed
	}
	return true
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
