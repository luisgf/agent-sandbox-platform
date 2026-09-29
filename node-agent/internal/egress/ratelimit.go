package egress

import (
	"sync"
	"time"
)

// TokenBucket is a simple per-key token bucket rate limiter.
type TokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens map[string]float64
	last   map[string]time.Time
}

func NewTokenBucket(ratePerSec, burst float64) *TokenBucket {
	if ratePerSec <= 0 {
		ratePerSec = 20
	}
	if burst <= 0 {
		burst = ratePerSec
	}
	return &TokenBucket{
		rate:   ratePerSec,
		burst:  burst,
		tokens: make(map[string]float64),
		last:   make(map[string]time.Time),
	}
}

// Allow returns true if one token was consumed for key.
func (b *TokenBucket) Allow(key string) bool {
	if b == nil {
		return true
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	tok, ok := b.tokens[key]
	last := b.last[key]
	if !ok {
		tok = b.burst
		last = now
	} else {
		elapsed := now.Sub(last).Seconds()
		tok += elapsed * b.rate
		if tok > b.burst {
			tok = b.burst
		}
	}
	if tok < 1 {
		b.tokens[key] = tok
		b.last[key] = now
		return false
	}
	b.tokens[key] = tok - 1
	b.last[key] = now
	return true
}
