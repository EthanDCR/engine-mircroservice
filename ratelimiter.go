package main

import "time"

// rateLimiter serializes calls to at most one per interval, shared across
// goroutines, regardless of how many CSV rows are processed at once.
type rateLimiter struct {
	ticker *time.Ticker
}

func newRateLimiter(interval time.Duration) *rateLimiter {
	return &rateLimiter{ticker: time.NewTicker(interval)}
}

func (r *rateLimiter) wait() {
	<-r.ticker.C
}
