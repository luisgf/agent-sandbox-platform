package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/api"
)

// bufferedExecTimeoutFromEnv reads ASP_BUFFERED_EXEC_TIMEOUT: a Go duration for
// how long a buffered exec (no ?stream=1) may run, "0" or "off" for no limit, and
// the default (10m) when unset.
func bufferedExecTimeoutFromEnv(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	switch raw {
	case "":
		return api.DefaultBufferedExecTimeout, nil
	case "0", "off", "false", "none", "disabled":
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s=%q is not a duration (10m, 90s), 0 or off", api.EnvBufferedExecTimeout, raw)
	}
	return d, nil
}
