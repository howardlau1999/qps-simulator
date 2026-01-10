package ratelimiter

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

func TestTokenBucket_Allow(t *testing.T) {
	config := types.RateLimiterConfig{
		Type:   "token_bucket",
		Rate:   10,
		Burst:  5,
		Shards: 16,
	}

	limiter, err := NewTokenBucket(config)
	if err != nil {
		t.Fatalf("failed to create token bucket: %v", err)
	}
	defer limiter.Close()

	ctx := context.Background()

	// First 5 requests should be allowed (burst)
	for i := 0; i < 5; i++ {
		result, err := limiter.Allow(ctx, "test-key", nil)
		if err != nil {
			t.Fatalf("Allow failed: %v", err)
		}
		if !result.Allowed {
			t.Errorf("Request %d should be allowed", i)
		}
	}

	// 6th request should be rate limited
	result, err := limiter.Allow(ctx, "test-key", nil)
	if err != nil {
		t.Fatalf("Allow failed: %v", err)
	}
	if result.Allowed {
		t.Error("6th request should be rate limited")
	}
}

func TestTokenBucket_RateRefill(t *testing.T) {
	config := types.RateLimiterConfig{
		Type:   "token_bucket",
		Rate:   100, // 100 tokens per second
		Burst:  1,
		Shards: 16,
	}

	limiter, err := NewTokenBucket(config)
	if err != nil {
		t.Fatalf("failed to create token bucket: %v", err)
	}
	defer limiter.Close()

	ctx := context.Background()

	// Use the burst token
	result, _ := limiter.Allow(ctx, "test-key", nil)
	if !result.Allowed {
		t.Error("First request should be allowed")
	}

	// Wait for refill (10ms = 1 token at 100/s rate)
	time.Sleep(15 * time.Millisecond)

	// Should be allowed now
	result, _ = limiter.Allow(ctx, "test-key", nil)
	if !result.Allowed {
		t.Error("Request after wait should be allowed")
	}
}

func TestSlidingWindow_Allow(t *testing.T) {
	config := types.RateLimiterConfig{
		Type:   "sliding_window",
		Rate:   10,
		Window: 100 * time.Millisecond,
		Shards: 16,
	}

	limiter, err := NewSlidingWindow(config)
	if err != nil {
		t.Fatalf("failed to create sliding window: %v", err)
	}
	defer limiter.Close()

	ctx := context.Background()

	// All 10 requests should be allowed
	for i := 0; i < 10; i++ {
		result, err := limiter.Allow(ctx, "test-key", nil)
		if err != nil {
			t.Fatalf("Allow failed: %v", err)
		}
		if !result.Allowed {
			t.Errorf("Request %d should be allowed", i)
		}
	}

	// 11th request should be rate limited
	result, _ := limiter.Allow(ctx, "test-key", nil)
	if result.Allowed {
		t.Error("11th request should be rate limited")
	}
}

func TestFixedWindow_Allow(t *testing.T) {
	config := types.RateLimiterConfig{
		Type:   "fixed_window",
		Rate:   5,
		Window: 100 * time.Millisecond,
		Shards: 16,
	}

	limiter, err := NewFixedWindow(config)
	if err != nil {
		t.Fatalf("failed to create fixed window: %v", err)
	}
	defer limiter.Close()

	ctx := context.Background()

	// All 5 requests should be allowed
	for i := 0; i < 5; i++ {
		result, err := limiter.Allow(ctx, "test-key", nil)
		if err != nil {
			t.Fatalf("Allow failed: %v", err)
		}
		if !result.Allowed {
			t.Errorf("Request %d should be allowed", i)
		}
	}

	// 6th request should be rate limited
	result, _ := limiter.Allow(ctx, "test-key", nil)
	if result.Allowed {
		t.Error("6th request should be rate limited")
	}

	// Wait for next window
	time.Sleep(150 * time.Millisecond)

	// Should be allowed in new window
	result, _ = limiter.Allow(ctx, "test-key", nil)
	if !result.Allowed {
		t.Error("Request in new window should be allowed")
	}
}

func TestLeakyBucket_Allow(t *testing.T) {
	config := types.RateLimiterConfig{
		Type:   "leaky_bucket",
		Rate:   10,
		Burst:  5, // capacity
		Shards: 16,
	}

	limiter, err := NewLeakyBucket(config)
	if err != nil {
		t.Fatalf("failed to create leaky bucket: %v", err)
	}
	defer limiter.Close()

	ctx := context.Background()

	// First 5 should be allowed (fill the bucket)
	for i := 0; i < 5; i++ {
		result, err := limiter.Allow(ctx, "test-key", nil)
		if err != nil {
			t.Fatalf("Allow failed: %v", err)
		}
		if !result.Allowed {
			t.Errorf("Request %d should be allowed", i)
		}
	}

	// 6th should be rate limited (bucket full)
	result, _ := limiter.Allow(ctx, "test-key", nil)
	if result.Allowed {
		t.Error("6th request should be rate limited (bucket full)")
	}
}

func TestRateLimiter_Concurrency(t *testing.T) {
	config := types.RateLimiterConfig{
		Type:   "token_bucket",
		Rate:   100000, // High rate
		Burst:  10000,
		Shards: 256,
	}

	limiter, err := NewTokenBucket(config)
	if err != nil {
		t.Fatalf("failed to create token bucket: %v", err)
	}
	defer limiter.Close()

	ctx := context.Background()

	// Run concurrent requests
	var wg sync.WaitGroup
	concurrency := 100
	requestsPerGoroutine := 100

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := "test-key" // Same key for contention
			for j := 0; j < requestsPerGoroutine; j++ {
				_, err := limiter.Allow(ctx, key, nil)
				if err != nil {
					t.Errorf("Allow failed: %v", err)
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestRegistry(t *testing.T) {
	registered := ListRegistered()
	
	expected := map[string]bool{
		"token_bucket":   true,
		"sliding_window": true,
		"fixed_window":   true,
		"leaky_bucket":   true,
	}

	for _, name := range registered {
		if !expected[name] {
			t.Errorf("Unexpected limiter registered: %s", name)
		}
		delete(expected, name)
	}

	for name := range expected {
		t.Errorf("Expected limiter not registered: %s", name)
	}
}

func TestCreate(t *testing.T) {
	tests := []struct {
		name       string
		limiterType string
		wantErr    bool
	}{
		{"token_bucket", "token_bucket", false},
		{"sliding_window", "sliding_window", false},
		{"fixed_window", "fixed_window", false},
		{"leaky_bucket", "leaky_bucket", false},
		{"unknown", "unknown_type", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := types.RateLimiterConfig{
				Type:   tt.limiterType,
				Rate:   100,
				Burst:  10,
				Shards: 16,
			}
			limiter, err := Create(config)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if limiter != nil {
				limiter.Close()
			}
		})
	}
}

func BenchmarkTokenBucket_Allow(b *testing.B) {
	config := types.RateLimiterConfig{
		Type:   "token_bucket",
		Rate:   1000000, // 1M QPS
		Burst:  100000,
		Shards: 256,
	}

	limiter, _ := NewTokenBucket(config)
	defer limiter.Close()

	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			limiter.Allow(ctx, "bench-key", nil)
		}
	})
}

func BenchmarkSlidingWindow_Allow(b *testing.B) {
	config := types.RateLimiterConfig{
		Type:   "sliding_window",
		Rate:   1000000,
		Window: time.Second,
		Shards: 256,
	}

	limiter, _ := NewSlidingWindow(config)
	defer limiter.Close()

	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			limiter.Allow(ctx, "bench-key", nil)
		}
	})
}
