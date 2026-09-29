package store

import "time"

// DefaultLeaseTTL is how long a claim/renew keeps exclusive ownership.
// Soft fencing only — no STONITH; see docs for split-brain limits.
const DefaultLeaseTTL = 30 * time.Second

func leaseUntil(from time.Time) time.Time {
	return from.Add(DefaultLeaseTTL)
}

func leaseExpired(until *time.Time, now time.Time) bool {
	if until == nil {
		// No lease set: treat sticky assignments without lease as expired for reclaim
		// of starting/running (legacy rows). Requested soft-assign without lease is OK.
		return true
	}
	return !until.After(now)
}
