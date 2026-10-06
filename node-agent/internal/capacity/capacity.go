// Package capacity detects what this host can offer to sandboxes, for the
// control plane's scheduler (ADR-0011).
package capacity

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Host is what the machine has.
type Host struct {
	CPUs        int
	MemTotalMiB int // 0 when unknown (no /proc/meminfo, e.g. macOS dry-run)
}

// Detect reads the CPU count and /proc/meminfo.
func Detect() Host {
	h := Host{CPUs: runtime.NumCPU()}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		h.MemTotalMiB = parseMemTotal(f)
	}
	return h
}

// parseMemTotal returns MemTotal from /proc/meminfo in MiB, or 0.
func parseMemTotal(r io.Reader) int {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0
			}
			return kb / 1024
		}
	}
	return 0
}

// The --capacity-* flags use -1 for "detect" and 0 for "not enforced".
const Detected = -1

// CPU resolves --capacity-cpu: detected cores, 0 (not enforced) or the given count.
func CPU(flag int, h Host) int {
	if flag == Detected {
		return h.CPUs
	}
	return flag
}

// MemMiB resolves --capacity-mem-mib. Detected memory keeps max(1 GiB, 10%) for
// the host and the agent. Unknown memory is 0 (not enforced); a host too small to
// keep that reserve reports 1 MiB, so the scheduler places nothing there.
func MemMiB(flag int, h Host) int {
	if flag != Detected {
		return flag
	}
	if h.MemTotalMiB <= 0 {
		return 0
	}
	reserve := max(1024, h.MemTotalMiB/10)
	if h.MemTotalMiB-reserve < 1 {
		return 1
	}
	return h.MemTotalMiB - reserve
}
