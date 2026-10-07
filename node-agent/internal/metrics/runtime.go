package metrics

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RuntimeCollector reports the Go runtime and the process: goroutines, memory,
// garbage collections, CPU time, open file descriptors and start time. Names follow
// the usual go_* and process_* conventions, so existing dashboards read them.
func RuntimeCollector(start time.Time) Collector {
	return func() []Sample {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		g := func(name, help string, v float64) Sample {
			return Sample{Name: name, Help: help, Type: "gauge", Value: v}
		}
		c := func(name, help string, v float64) Sample {
			return Sample{Name: name, Help: help, Type: "counter", Value: v}
		}
		out := []Sample{
			g("go_goroutines", "Number of goroutines.", float64(runtime.NumGoroutine())),
			g("go_memstats_alloc_bytes", "Bytes of allocated heap objects.", float64(m.Alloc)),
			g("go_memstats_heap_objects", "Number of allocated heap objects.", float64(m.HeapObjects)),
			g("go_memstats_sys_bytes", "Bytes obtained from the system.", float64(m.Sys)),
			c("go_gc_cycles_total", "Completed garbage collection cycles.", float64(m.NumGC)),
			c("go_gc_pause_seconds_total", "Total time paused by garbage collection.", float64(m.PauseTotalNs)/1e9),
			g("process_start_time_seconds", "Start time of the process, in seconds since the Unix epoch.", float64(start.Unix())),
		}
		var ru syscall.Rusage
		if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
			cpu := float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
			out = append(out, c("process_cpu_seconds_total", "User and system CPU time of the process, in seconds.", cpu))
		}
		if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
			out = append(out, g("process_open_fds", "Number of open file descriptors.", float64(len(entries))))
		}
		if b, err := os.ReadFile("/proc/self/statm"); err == nil {
			if f := strings.Fields(string(b)); len(f) >= 2 {
				if pages, err := strconv.ParseFloat(f[1], 64); err == nil {
					out = append(out, g("process_resident_memory_bytes", "Resident memory of the process, in bytes.", pages*float64(os.Getpagesize())))
				}
			}
		}
		return out
	}
}
