package ratelimiter

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

// Registry stores registered rate limiter factories
var (
	registryMu sync.RWMutex
	registry   = make(map[string]types.RateLimiterFactory)
)

// Register registers a rate limiter factory
func Register(name string, factory types.RateLimiterFactory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = factory
}

// Create creates a rate limiter from configuration
func Create(config types.RateLimiterConfig) (types.RateLimiter, error) {
	registryMu.RLock()
	factory, ok := registry[config.Type]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown rate limiter type: %s", config.Type)
	}

	return factory(config)
}

// ListRegistered returns all registered rate limiter types
func ListRegistered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}

func init() {
	// Register built-in rate limiters
	Register("token_bucket", NewTokenBucket)
	Register("sliding_window", NewSlidingWindow)
	Register("fixed_window", NewFixedWindow)
	Register("leaky_bucket", NewLeakyBucket)
}

// baseLimiter provides common functionality for rate limiters
type baseLimiter struct {
	config types.RateLimiterConfig
	shards []*shard
	closed bool
	mu     sync.RWMutex
}

// shard represents a single shard of the rate limiter state
type shard struct {
	mu    sync.Mutex
	state map[string]interface{}
}

func newBaseLimiter(config types.RateLimiterConfig) *baseLimiter {
	numShards := config.Shards
	if numShards <= 0 {
		numShards = 256 // Default for high concurrency
	}

	shards := make([]*shard, numShards)
	for i := 0; i < numShards; i++ {
		shards[i] = &shard{
			state: make(map[string]interface{}),
		}
	}

	return &baseLimiter{
		config: config,
		shards: shards,
	}
}

// getShard returns the shard for a given key using FNV-1a hash
func (b *baseLimiter) getShard(key string) *shard {
	h := fnv1a(key)
	return b.shards[h%uint64(len(b.shards))]
}

// fnv1a computes FNV-1a hash of a string (fast, good distribution)
func fnv1a(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

func (b *baseLimiter) Config() types.RateLimiterConfig {
	return b.config
}

func (b *baseLimiter) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

// ============================================================================
// Token Bucket Rate Limiter
// ============================================================================

// tokenBucketState holds state for a single key
type tokenBucketState struct {
	tokens     float64
	lastUpdate time.Time
}

// TokenBucket implements the token bucket algorithm
type TokenBucket struct {
	*baseLimiter
	rate  float64 // tokens per second
	burst float64 // max tokens
}

// NewTokenBucket creates a new token bucket rate limiter
func NewTokenBucket(config types.RateLimiterConfig) (types.RateLimiter, error) {
	if config.Rate <= 0 {
		return nil, fmt.Errorf("rate must be positive")
	}
	burst := config.Burst
	if burst <= 0 {
		burst = config.Rate
	}

	return &TokenBucket{
		baseLimiter: newBaseLimiter(config),
		rate:        float64(config.Rate),
		burst:       float64(burst),
	}, nil
}

func (t *TokenBucket) Allow(ctx context.Context, key string, headers map[string]string) (*types.RateLimitResult, error) {
	return t.AllowN(ctx, key, 1)
}

func (t *TokenBucket) AllowN(ctx context.Context, key string, n int64) (*types.RateLimitResult, error) {
	shard := t.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	now := time.Now()
	cost := float64(n)

	state, ok := shard.state[key].(*tokenBucketState)
	if !ok {
		state = &tokenBucketState{
			tokens:     t.burst,
			lastUpdate: now,
		}
		shard.state[key] = state
	}

	// Add tokens based on elapsed time
	elapsed := now.Sub(state.lastUpdate).Seconds()
	state.tokens += elapsed * t.rate
	if state.tokens > t.burst {
		state.tokens = t.burst
	}
	state.lastUpdate = now

	result := &types.RateLimitResult{
		Remaining: int64(state.tokens),
	}

	if state.tokens >= cost {
		state.tokens -= cost
		result.Allowed = true
		result.Remaining = int64(state.tokens)
	} else {
		result.Allowed = false
		result.LimitedBy = key
		// Calculate when enough tokens will be available
		needed := cost - state.tokens
		result.RetryAfter = time.Duration(needed/t.rate*1e9) * time.Nanosecond
	}

	return result, nil
}

func (t *TokenBucket) Reset(key string) error {
	shard := t.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	delete(shard.state, key)
	return nil
}

// ============================================================================
// Sliding Window Rate Limiter
// ============================================================================

type slidingWindowState struct {
	prevCount  int64
	currCount  int64
	windowStart time.Time
}

// SlidingWindow implements the sliding window log algorithm
type SlidingWindow struct {
	*baseLimiter
	rate   int64
	window time.Duration
}

// NewSlidingWindow creates a new sliding window rate limiter
func NewSlidingWindow(config types.RateLimiterConfig) (types.RateLimiter, error) {
	if config.Rate <= 0 {
		return nil, fmt.Errorf("rate must be positive")
	}
	window := config.Window
	if window <= 0 {
		window = time.Second
	}

	return &SlidingWindow{
		baseLimiter: newBaseLimiter(config),
		rate:        config.Rate,
		window:      window,
	}, nil
}

func (s *SlidingWindow) Allow(ctx context.Context, key string, headers map[string]string) (*types.RateLimitResult, error) {
	return s.AllowN(ctx, key, 1)
}

func (s *SlidingWindow) AllowN(ctx context.Context, key string, n int64) (*types.RateLimitResult, error) {
	shard := s.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	now := time.Now()

	state, ok := shard.state[key].(*slidingWindowState)
	if !ok {
		state = &slidingWindowState{
			windowStart: now.Truncate(s.window),
		}
		shard.state[key] = state
	}

	currentWindow := now.Truncate(s.window)
	
	// Handle window transition
	if currentWindow.After(state.windowStart) {
		if currentWindow.Sub(state.windowStart) >= s.window {
			// More than one window has passed
			state.prevCount = 0
			state.currCount = 0
		} else {
			// Move to next window
			state.prevCount = state.currCount
			state.currCount = 0
		}
		state.windowStart = currentWindow
	}

	// Calculate weighted count using sliding window
	elapsed := now.Sub(currentWindow)
	weight := float64(s.window-elapsed) / float64(s.window)
	weightedCount := float64(state.prevCount)*weight + float64(state.currCount)

	result := &types.RateLimitResult{
		Remaining:  s.rate - int64(weightedCount),
		ResetAfter: s.window - elapsed,
	}

	if int64(weightedCount)+n <= s.rate {
		state.currCount += n
		result.Allowed = true
		result.Remaining = s.rate - int64(weightedCount) - n
	} else {
		result.Allowed = false
		result.LimitedBy = key
		result.RetryAfter = s.window - elapsed
	}

	return result, nil
}

func (s *SlidingWindow) Reset(key string) error {
	shard := s.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	delete(shard.state, key)
	return nil
}

// ============================================================================
// Fixed Window Rate Limiter
// ============================================================================

type fixedWindowState struct {
	count       int64
	windowStart time.Time
}

// FixedWindow implements the fixed window counter algorithm
type FixedWindow struct {
	*baseLimiter
	rate   int64
	window time.Duration
}

// NewFixedWindow creates a new fixed window rate limiter
func NewFixedWindow(config types.RateLimiterConfig) (types.RateLimiter, error) {
	if config.Rate <= 0 {
		return nil, fmt.Errorf("rate must be positive")
	}
	window := config.Window
	if window <= 0 {
		window = time.Second
	}

	return &FixedWindow{
		baseLimiter: newBaseLimiter(config),
		rate:        config.Rate,
		window:      window,
	}, nil
}

func (f *FixedWindow) Allow(ctx context.Context, key string, headers map[string]string) (*types.RateLimitResult, error) {
	return f.AllowN(ctx, key, 1)
}

func (f *FixedWindow) AllowN(ctx context.Context, key string, n int64) (*types.RateLimitResult, error) {
	shard := f.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	now := time.Now()
	currentWindow := now.Truncate(f.window)

	state, ok := shard.state[key].(*fixedWindowState)
	if !ok || state.windowStart.Before(currentWindow) {
		state = &fixedWindowState{
			count:       0,
			windowStart: currentWindow,
		}
		shard.state[key] = state
	}

	elapsed := now.Sub(currentWindow)
	result := &types.RateLimitResult{
		Remaining:  f.rate - state.count,
		ResetAfter: f.window - elapsed,
	}

	if state.count+n <= f.rate {
		state.count += n
		result.Allowed = true
		result.Remaining = f.rate - state.count
	} else {
		result.Allowed = false
		result.LimitedBy = key
		result.RetryAfter = f.window - elapsed
	}

	return result, nil
}

func (f *FixedWindow) Reset(key string) error {
	shard := f.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	delete(shard.state, key)
	return nil
}

// ============================================================================
// Leaky Bucket Rate Limiter
// ============================================================================

type leakyBucketState struct {
	water      float64 // Current water level
	lastUpdate time.Time
}

// LeakyBucket implements the leaky bucket algorithm
type LeakyBucket struct {
	*baseLimiter
	rate     float64 // Leak rate (requests per second)
	capacity float64 // Bucket capacity
}

// NewLeakyBucket creates a new leaky bucket rate limiter
func NewLeakyBucket(config types.RateLimiterConfig) (types.RateLimiter, error) {
	if config.Rate <= 0 {
		return nil, fmt.Errorf("rate must be positive")
	}
	capacity := config.Burst
	if capacity <= 0 {
		capacity = config.Rate
	}

	return &LeakyBucket{
		baseLimiter: newBaseLimiter(config),
		rate:        float64(config.Rate),
		capacity:    float64(capacity),
	}, nil
}

func (l *LeakyBucket) Allow(ctx context.Context, key string, headers map[string]string) (*types.RateLimitResult, error) {
	return l.AllowN(ctx, key, 1)
}

func (l *LeakyBucket) AllowN(ctx context.Context, key string, n int64) (*types.RateLimitResult, error) {
	shard := l.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	now := time.Now()
	cost := float64(n)

	state, ok := shard.state[key].(*leakyBucketState)
	if !ok {
		state = &leakyBucketState{
			water:      0,
			lastUpdate: now,
		}
		shard.state[key] = state
	}

	// Drain water based on elapsed time
	elapsed := now.Sub(state.lastUpdate).Seconds()
	state.water -= elapsed * l.rate
	if state.water < 0 {
		state.water = 0
	}
	state.lastUpdate = now

	result := &types.RateLimitResult{
		Remaining: int64(l.capacity - state.water),
	}

	if state.water+cost <= l.capacity {
		state.water += cost
		result.Allowed = true
		result.Remaining = int64(l.capacity - state.water)
	} else {
		result.Allowed = false
		result.LimitedBy = key
		// Calculate when enough capacity will be available
		overflow := state.water + cost - l.capacity
		result.RetryAfter = time.Duration(overflow/l.rate*1e9) * time.Nanosecond
	}

	return result, nil
}

func (l *LeakyBucket) Reset(key string) error {
	shard := l.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	delete(shard.state, key)
	return nil
}
