package reconciler

import "fmt"

// GuestVerify modes (--guest-verify).
const (
	GuestVerifyAuto = "auto"
	GuestVerifyOn   = "on"
	GuestVerifyOff  = "off"
)

// checkGuestFile refuses a kernel or base image that is not the one that was released: the
// digest of the file, which the measure cache keeps while the file does not change, must be
// the one a SHA256SUMS next to it lists. Every sandbox boots from these files, so one
// replaced (or damaged) on the node would otherwise be booted without a word.
func (r *Reconciler) checkGuestFile(what, path string) error {
	if r.Measure == nil || r.Expected == nil || r.GuestVerify == GuestVerifyOff || r.GuestVerify == "" {
		return nil
	}
	want, listed := r.Expected(path)
	if !listed {
		if r.GuestVerify == GuestVerifyOn {
			return fmt.Errorf("guest %s %s is not listed in a SHA256SUMS next to it, and --guest-verify=on needs it: asp image pull installs them with one", what, path)
		}
		return nil
	}
	got, err := r.Measure(path)
	if err != nil {
		return fmt.Errorf("guest %s %s cannot be read to check it against its SHA256SUMS: %w", what, path, err)
	}
	if got != want {
		return fmt.Errorf("guest %s %s is %s but its SHA256SUMS says %s: not booting from it (asp image pull installs a good one, or --guest-verify=off)", what, path, got, want)
	}
	return nil
}
