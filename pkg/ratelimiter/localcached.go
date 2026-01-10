package ratelimiter

import (
	"context"
	"sync"
	"time"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

// RemoteTokenServer simulates the centralized rate limit server
type RemoteTokenServer struct {
	mu         sync.Mutex
	tokens     float64
	rate       float64 // tokens per second
	lastRefill time.Time
}

// NewRemoteTokenServer creates a new remote server simulation
func NewRemoteTokenServer(rate int64, burst int64) *RemoteTokenServer {
	return &RemoteTokenServer{
		tokens:     float64(burst),
		rate:       float64(rate),
		lastRefill: time.Now(),
	}
}

// RequestTokens attempts to fetch tokens.
// If tokens are available or debt is allowed, returns granted count and 0 wait.
// If currently in debt from previous requests, returns 0 granted and a wait duration.
func (s *RemoteTokenServer) RequestTokens(n int64) (int64, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Refill tokens
	now := time.Now()
	elapsed := now.Sub(s.lastRefill).Seconds()
	s.lastRefill = now

	// Add new tokens
	s.tokens += elapsed * s.rate

	// Check if we are currently in debt (negative tokens)
	// If we are in debt, we reject this request until we are back to positive
	if s.tokens < 0 {
		// Calculate time to reach 0
		deficit := -s.tokens
		secondsToRecover := deficit / s.rate
		wait := time.Duration(secondsToRecover * float64(time.Second))
		return 0, wait
	}

	// Not in debt (or positive enough). We grant the request.
	// This might push us into negative (debt), which will block FUTURE requests.
	s.tokens -= float64(n)

	// If we went into debt after this request, return a wait time for the NEXT requestors (simulate rejection period)
	// The current request IS granted (we consumed the tokens), but we signal backpressure.
	if s.tokens < 0 {
		deficit := -s.tokens
		secondsToRecover := deficit / s.rate
		wait := time.Duration(secondsToRecover * float64(time.Second))
		return n, wait
	}

	return n, 0
}

// LocalCachedLimiter implements a rate limiter with local token caching
type LocalCachedLimiter struct {
	remote      *RemoteTokenServer
	localTokens int64
	rejectUntil time.Time

	prefetchCount int64
	checkDelay    time.Duration
	checkJitter   time.Duration
	metrics       types.MetricsRecorder

	mu        sync.Mutex
	fetching  bool
	fetchCond *sync.Cond

	config types.RateLimiterConfig
}

func NewLocalCachedLimiter(config types.RateLimiterConfig, remote *RemoteTokenServer, prefetch int, delayMs, jitterMs int, metrics types.MetricsRecorder) *LocalCachedLimiter {
	l := &LocalCachedLimiter{
		remote:        remote,
		prefetchCount: int64(prefetch),
		checkDelay:    time.Duration(delayMs) * time.Millisecond,
		checkJitter:   time.Duration(jitterMs) * time.Millisecond,
		metrics:       metrics,
		config:        config,
	}
	l.fetchCond = sync.NewCond(&l.mu)
	return l
}

func (l *LocalCachedLimiter) Allow(ctx context.Context, key string, headers map[string]string) (*types.RateLimitResult, error) {
	return l.AllowN(ctx, key, 1) // Simple wrapper
}

func (l *LocalCachedLimiter) AllowN(ctx context.Context, key string, n int64) (*types.RateLimitResult, error) {
	l.mu.Lock()

	// Loop to handle single-flight waiting
	for {
		now := time.Now()

		// 1. Check local tokens (PRIMARY: If we have tokens, use them!)
		if l.localTokens >= n {
			l.localTokens -= n
			l.mu.Unlock()
			return &types.RateLimitResult{
				Allowed:   true,
				Remaining: l.localTokens,
			}, nil
		}

		// 2. Not enough tokens. Check if we are in a rejection period.
		// If so, we cannot fetch more. Reject immediately.
		if now.Before(l.rejectUntil) {
			retryAfter := l.rejectUntil.Sub(now)
			l.mu.Unlock()
			return &types.RateLimitResult{
				Allowed:    false,
				RetryAfter: retryAfter,
				LimitedBy:  "local_cached_reject_period",
			}, nil
		}

		// 3. Not enough tokens. Need fetch.
		// If someone else is fetching, wait.
		if l.fetching {
			startWait := time.Now()
			l.fetchCond.Wait()
			if l.metrics != nil {
				l.metrics.RecordTokenFetchWait(time.Since(startWait))
			}
			// After wake up, re-check everything (continue loop)
			continue
		}

		// 4. We are the fetcher
		l.fetching = true
		l.mu.Unlock()

		// Perform Fetch (outside lock)
		granted, retryAfter, fetchLatency := l.performFetch()

		if l.metrics != nil {
			l.metrics.RecordRemoteTokenFetch(fetchLatency)
		}

		l.mu.Lock()
		l.fetching = false

		// Always add granted tokens (even if we are told to back off for *future* requests)
		l.localTokens += granted

		if retryAfter > 0 {
			// Remote told us to back off
			l.rejectUntil = time.Now().Add(retryAfter)
		}

		// Wake up waiters
		l.fetchCond.Broadcast()

		// Loop again to consume tokens or fail
	}
}

func (l *LocalCachedLimiter) performFetch() (int64, time.Duration, time.Duration) {
	start := time.Now()

	// Simulate Network Delay
	if l.checkDelay > 0 {
		delay := l.checkDelay
		if l.checkJitter > 0 {
			// Simple jitter
			jitter := time.Duration(time.Now().UnixNano()) % l.checkJitter
			delay += jitter
		}
		time.Sleep(delay)
	}

	granted, retryAfter := l.remote.RequestTokens(l.prefetchCount)
	latency := time.Since(start)

	return granted, retryAfter, latency
}

func (l *LocalCachedLimiter) Reset(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.localTokens = 0
	l.rejectUntil = time.Time{}
	return nil
}

func (l *LocalCachedLimiter) Config() types.RateLimiterConfig {
	return l.config
}

func (l *LocalCachedLimiter) Close() error {
	return nil
}
