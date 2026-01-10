package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Collector collects and aggregates simulation metrics
type Collector struct {
	startTime time.Time

	// Atomic counters for high-throughput (1M+ QPS) - CUMULATIVE
	totalRequests       int64
	successfulRequests  int64
	rateLimitedRequests int64
	failedRequests      int64
	activeConnections   int64

	// Rate Limit Check Metrics
	rateLimitChecks int64
	// Remote Token Fetch Metrics
	remoteFetches          int64
	tokenFetchWaitDuration int64 // Cumulative nanoseconds

	// Current instantaneous bucket (atomic pointer to *Bucket)
	currentBucket unsafe.Pointer

	// History of completed buckets (timestamp -> *Bucket)
	// Used by Snapshot() to retrieve the last full second's stats.
	// Map is protected by mutex, but writes only happen once per second.
	historyMu sync.RWMutex
	history   map[int64]*Bucket

	// Latency tracking with sharding for high concurrency (Current Window Only)
	numShards int
	closed    bool
	mu        sync.RWMutex
}

// Bucket represents metrics for a specific 1-second window
type Bucket struct {
	Timestamp int64 // Unix timestamp

	Total    int64
	Success  int64
	Rejected int64

	RateLimitChecks        int64
	RemoteFetches          int64
	TokenFetchWaitDuration int64

	SuccessLatencies        []*latencyShard
	RejectedLatencies       []*latencyShard
	RateLimitCheckLatencies []*latencyShard
	RemoteFetchLatencies    []*latencyShard
}

type latencyShard struct {
	mu        sync.Mutex
	latencies []time.Duration
}

// NewCollector creates a new metrics collector
func NewCollector(numShards int) *Collector {
	if numShards <= 0 {
		numShards = 64 // Default for high concurrency
	}

	c := &Collector{
		startTime: time.Now(),
		numShards: numShards,
		history:   make(map[int64]*Bucket),
	}

	// Initialize first bucket
	now := time.Now().Unix()
	firstBucket := newBucket(now, numShards)
	atomic.StorePointer(&c.currentBucket, unsafe.Pointer(firstBucket))

	return c
}

func newBucket(timestamp int64, numShards int) *Bucket {
	b := &Bucket{
		Timestamp:               timestamp,
		SuccessLatencies:        make([]*latencyShard, numShards),
		RejectedLatencies:       make([]*latencyShard, numShards),
		RateLimitCheckLatencies: make([]*latencyShard, numShards),
		RemoteFetchLatencies:    make([]*latencyShard, numShards),
	}
	for i := 0; i < numShards; i++ {
		b.SuccessLatencies[i] = &latencyShard{latencies: make([]time.Duration, 0, 1000)}
		b.RejectedLatencies[i] = &latencyShard{latencies: make([]time.Duration, 0, 1000)}
		b.RateLimitCheckLatencies[i] = &latencyShard{latencies: make([]time.Duration, 0, 1000)}
		b.RemoteFetchLatencies[i] = &latencyShard{latencies: make([]time.Duration, 0, 1000)}
	}
	return b
}

// RecordRequest records a request metric based on its completion time
func (c *Collector) RecordRequest(clientID, serverID string, latency time.Duration, rateLimited bool) {
	atomic.AddInt64(&c.totalRequests, 1)

	// Determine the timestamp bucket for this request
	// Note: We use completion time as the timestamp
	now := time.Now().Unix()

	// Optimistic retrieval of current bucket
	ptr := atomic.LoadPointer(&c.currentBucket)
	bucket := (*Bucket)(ptr)

	if bucket.Timestamp != now {
		// Time has advanced (or receded). We need to rotate or find the correct bucket.
		if now > bucket.Timestamp {
			// Rotation needed
			c.rotateBucket(now)
			// Reload bucket after potential rotation
			ptr = atomic.LoadPointer(&c.currentBucket)
			bucket = (*Bucket)(ptr)
		}
	}

	// Record stats in the bucket
	atomic.AddInt64(&bucket.Total, 1)

	var shard *latencyShard
	shardIdx := fnv1a(clientID) % uint64(c.numShards)

	if rateLimited {
		atomic.AddInt64(&c.rateLimitedRequests, 1)
		atomic.AddInt64(&bucket.Rejected, 1)
		shard = bucket.RejectedLatencies[shardIdx]
	} else {
		atomic.AddInt64(&c.successfulRequests, 1)
		atomic.AddInt64(&bucket.Success, 1)
		shard = bucket.SuccessLatencies[shardIdx]
	}

	// Record latency
	shard.mu.Lock()
	shard.latencies = append(shard.latencies, latency)
	shard.mu.Unlock()
}

// RecordRateLimitCheck records latency for a rate limit check
func (c *Collector) RecordRateLimitCheck(latency time.Duration) {
	atomic.AddInt64(&c.rateLimitChecks, 1)

	now := time.Now().Unix()
	ptr := atomic.LoadPointer(&c.currentBucket)
	bucket := (*Bucket)(ptr)

	if bucket.Timestamp != now {
		if now > bucket.Timestamp {
			c.rotateBucket(now)
			ptr = atomic.LoadPointer(&c.currentBucket)
			bucket = (*Bucket)(ptr)
		}
	}

	atomic.AddInt64(&bucket.RateLimitChecks, 1)

	// Random shard for check latency to avoid contention
	// Since we don't have a client ID, we can pick randomly or round-robin,
	// but random is cheaper if we just use time or similar.
	// Valid approach: use time nanoseconds
	shardIdx := uint64(time.Now().Nanosecond()) % uint64(c.numShards)
	shard := bucket.RateLimitCheckLatencies[shardIdx]

	shard.mu.Lock()
	shard.latencies = append(shard.latencies, latency)
	shard.mu.Unlock()
}

// RecordRemoteTokenFetch records latency for a remote token fetch
func (c *Collector) RecordRemoteTokenFetch(latency time.Duration) {
	atomic.AddInt64(&c.remoteFetches, 1)

	now := time.Now().Unix()
	ptr := atomic.LoadPointer(&c.currentBucket)
	bucket := (*Bucket)(ptr)

	if bucket.Timestamp != now {
		if now > bucket.Timestamp {
			c.rotateBucket(now)
			ptr = atomic.LoadPointer(&c.currentBucket)
			bucket = (*Bucket)(ptr)
		}
	}

	atomic.AddInt64(&bucket.RemoteFetches, 1)

	// Random shard
	shardIdx := uint64(time.Now().Nanosecond()) % uint64(c.numShards)
	shard := bucket.RemoteFetchLatencies[shardIdx]

	shard.mu.Lock()
	shard.latencies = append(shard.latencies, latency)
	shard.mu.Unlock()
}

// RecordTokenFetchWait records time spent waiting for a remote token fetch
func (c *Collector) RecordTokenFetchWait(duration time.Duration) {
	atomic.AddInt64(&c.tokenFetchWaitDuration, int64(duration))

	now := time.Now().Unix()
	ptr := atomic.LoadPointer(&c.currentBucket)
	bucket := (*Bucket)(ptr)

	if bucket.Timestamp != now {
		if now > bucket.Timestamp {
			c.rotateBucket(now)
			ptr = atomic.LoadPointer(&c.currentBucket)
			bucket = (*Bucket)(ptr)
		}
	}

	atomic.AddInt64(&bucket.TokenFetchWaitDuration, int64(duration))
}

// rotateBucket handles shifting to a new time bucket
func (c *Collector) rotateBucket(newTimestamp int64) {
	// Acquire lock to manage history map
	c.historyMu.Lock()
	defer c.historyMu.Unlock()

	// Re-check current bucket under lock to avoid race
	ptr := atomic.LoadPointer(&c.currentBucket)
	current := (*Bucket)(ptr)

	if current.Timestamp >= newTimestamp {
		// Someone else rotated
		return
	}

	// Archive current bucket
	c.history[current.Timestamp] = current

	// Cleanup old history (keep last 10 seconds)
	for ts := range c.history {
		if ts < newTimestamp-10 {
			delete(c.history, ts)
		}
	}

	// Create and set new bucket
	newBucket := newBucket(newTimestamp, c.numShards)
	atomic.StorePointer(&c.currentBucket, unsafe.Pointer(newBucket))
}

// RecordFailedRequest records a failed request
func (c *Collector) RecordFailedRequest() {
	atomic.AddInt64(&c.totalRequests, 1)
	atomic.AddInt64(&c.failedRequests, 1)
}

// RecordConnection records a connection event
func (c *Collector) RecordConnection(clientID, serverID string, opened bool) {
	if opened {
		atomic.AddInt64(&c.activeConnections, 1)
	} else {
		atomic.AddInt64(&c.activeConnections, -1)
	}
}

// Snapshot returns a point-in-time snapshot of metrics
func (c *Collector) Snapshot() *MetricsSnapshot {
	// Logic: Get the LAST COMPLETED bucket (now - 1).
	now := time.Now().Unix()
	return c.getSnapshotForTimestamp(now - 1)
}

// CaptureFinalSnapshot flushes the current partial bucket and returns its snapshot
func (c *Collector) CaptureFinalSnapshot() *MetricsSnapshot {
	// Force rotation to current timestamp (if needed) to ensure current bucket is fresh
	// But actually we want to snapshot the *current* pending bucket
	// Just read the current bucket directly

	ptr := atomic.LoadPointer(&c.currentBucket)
	bucket := (*Bucket)(ptr)

	// The current bucket is for `now`.
	// Simulation is ending, so we snapshot `now`.
	// We pass the bucket directly to normalization.

	return c.createSnapshotFromBucket(bucket)
}

func (c *Collector) getSnapshotForTimestamp(ts int64) *MetricsSnapshot {
	c.historyMu.RLock()
	bucket := c.history[ts]
	c.historyMu.RUnlock()
	return c.createSnapshotFromBucket(bucket)
}

func (c *Collector) createSnapshotFromBucket(bucket *Bucket) *MetricsSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	elapsed := time.Since(c.startTime)
	total := atomic.LoadInt64(&c.totalRequests)

	var qps, successQPS, rejectedQPS, checkQPS, fetchQPS float64
	var avg, p50, p95, p99 time.Duration
	var avgSuccess, p50Success, p95Success, p99Success time.Duration
	var avgRejected, p50Rejected, p95Rejected, p99Rejected time.Duration
	var avgCheck, p50Check, p95Check, p99Check time.Duration
	var avgFetch, p50Fetch, p95Fetch, p99Fetch time.Duration
	var avgWaiters float64

	if bucket != nil {
		// Calculate Effective Duration for Normalization
		// Bucket spans [bucket.Timestamp, bucket.Timestamp+1]
		bucketStart := time.Unix(bucket.Timestamp, 0)
		bucketEnd := bucketStart.Add(time.Second)

		// Intersect with Simulation Start Time
		effectiveStart := bucketStart
		if c.startTime.After(bucketStart) {
			effectiveStart = c.startTime
		}

		// Intersect with Simulation End Time (Now)
		effectiveEnd := bucketEnd
		now := time.Now()
		if now.Before(bucketEnd) {
			effectiveEnd = now
		}

		duration := effectiveEnd.Sub(effectiveStart).Seconds()
		if duration < 0.001 {
			duration = 1.0 // Prevent division by zero or weirdness
		}

		// Count requests
		countTotal := float64(atomic.LoadInt64(&bucket.Total))
		countSuccess := float64(atomic.LoadInt64(&bucket.Success))
		countRejected := float64(atomic.LoadInt64(&bucket.Rejected))
		countChecks := float64(atomic.LoadInt64(&bucket.RateLimitChecks))
		countFetches := float64(atomic.LoadInt64(&bucket.RemoteFetches))

		// Normalize QPS
		qps = countTotal / duration
		successQPS = countSuccess / duration
		rejectedQPS = countRejected / duration
		checkQPS = countChecks / duration
		fetchQPS = countFetches / duration

		// Avg Waiters = TotalWaitDuration / Duration
		waitDuration := atomic.LoadInt64(&bucket.TokenFetchWaitDuration)
		// duration is in seconds. waitDuration is in ns.
		// Avg Waiters = (waitDuration_ns / 1e9) / duration_s
		avgWaiters = (float64(waitDuration) / 1e9) / duration

		// Latencies
		var successLatencies, rejectedLatencies, checkLatencies, fetchLatencies []time.Duration

		for _, shard := range bucket.SuccessLatencies {
			shard.mu.Lock()
			successLatencies = append(successLatencies, shard.latencies...)
			shard.mu.Unlock()
		}

		for _, shard := range bucket.RejectedLatencies {
			shard.mu.Lock()
			rejectedLatencies = append(rejectedLatencies, shard.latencies...)
			shard.mu.Unlock()
		}

		for _, shard := range bucket.RateLimitCheckLatencies {
			shard.mu.Lock()
			checkLatencies = append(checkLatencies, shard.latencies...)
			shard.mu.Unlock()
		}

		for _, shard := range bucket.RemoteFetchLatencies {
			shard.mu.Lock()
			fetchLatencies = append(fetchLatencies, shard.latencies...)
			shard.mu.Unlock()
		}

		allLatencies := make([]time.Duration, 0, len(successLatencies)+len(rejectedLatencies))
		allLatencies = append(allLatencies, successLatencies...)
		allLatencies = append(allLatencies, rejectedLatencies...)

		avg, p50, p95, p99 = calculateStats(allLatencies)
		avgSuccess, p50Success, p95Success, p99Success = calculateStats(successLatencies)
		avgRejected, p50Rejected, p95Rejected, p99Rejected = calculateStats(rejectedLatencies)
		avgCheck, p50Check, p95Check, p99Check = calculateStats(checkLatencies)
		avgFetch, p50Fetch, p95Fetch, p99Fetch = calculateStats(fetchLatencies)
	}

	return &MetricsSnapshot{
		Timestamp:           time.Now(),
		Duration:            elapsed,
		TotalRequests:       total,
		SuccessfulRequests:  atomic.LoadInt64(&c.successfulRequests),
		RateLimitedRequests: atomic.LoadInt64(&c.rateLimitedRequests),
		FailedRequests:      atomic.LoadInt64(&c.failedRequests),

		RequestsPerSecond: qps,
		SuccessQPS:        successQPS,
		RejectedQPS:       rejectedQPS,

		AvgLatency: avg,
		P50Latency: p50,
		P95Latency: p95,
		P99Latency: p99,

		AvgSuccessLatency: avgSuccess,
		P50SuccessLatency: p50Success,
		P95SuccessLatency: p95Success,
		P99SuccessLatency: p99Success,

		AvgRejectedLatency: avgRejected,
		P50RejectedLatency: p50Rejected,
		P95RejectedLatency: p95Rejected,
		P99RejectedLatency: p99Rejected,

		RateLimitCheckQPS:        checkQPS,
		AvgRateLimitCheckLatency: avgCheck,
		P50RateLimitCheckLatency: p50Check,
		P95RateLimitCheckLatency: p95Check,
		P99RateLimitCheckLatency: p99Check,

		RemoteFetchQPS:        fetchQPS,
		AvgRemoteFetchLatency: avgFetch,
		P50RemoteFetchLatency: p50Fetch,
		P95RemoteFetchLatency: p95Fetch,
		P99RemoteFetchLatency: p99Fetch,

		AvgTokenFetchWaiters: avgWaiters,

		ActiveConnections: atomic.LoadInt64(&c.activeConnections),
	}
}

// calculateStats helper to calculate average and percentiles
func calculateStats(latencies []time.Duration) (avg, p50, p95, p99 time.Duration) {
	if len(latencies) == 0 {
		return 0, 0, 0, 0
	}

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	var sum time.Duration
	for _, l := range latencies {
		sum += l
	}
	avg = sum / time.Duration(len(latencies))

	p50 = latencies[len(latencies)*50/100]
	p95 = latencies[len(latencies)*95/100]
	p99 = latencies[len(latencies)*99/100]

	return
}

// Reset resets all metrics
func (c *Collector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.startTime = time.Now()
	atomic.StoreInt64(&c.totalRequests, 0)
	atomic.StoreInt64(&c.successfulRequests, 0)
	atomic.StoreInt64(&c.rateLimitedRequests, 0)
	atomic.StoreInt64(&c.failedRequests, 0)
	atomic.StoreInt64(&c.activeConnections, 0)
	atomic.StoreInt64(&c.rateLimitChecks, 0)
	atomic.StoreInt64(&c.remoteFetches, 0)
	atomic.StoreInt64(&c.tokenFetchWaitDuration, 0)

	c.historyMu.Lock()
	c.history = make(map[int64]*Bucket)
	c.historyMu.Unlock()

	now := time.Now().Unix()
	firstBucket := newBucket(now, c.numShards)
	atomic.StorePointer(&c.currentBucket, unsafe.Pointer(firstBucket))
}

// Close closes the metrics collector
func (c *Collector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// MetricsSnapshot contains a point-in-time view of metrics
type MetricsSnapshot struct {
	Timestamp           time.Time     `json:"timestamp"`
	Duration            time.Duration `json:"duration_ns"`
	TotalRequests       int64         `json:"total_requests"`
	SuccessfulRequests  int64         `json:"successful_requests"`
	RateLimitedRequests int64         `json:"rate_limited_requests"`
	FailedRequests      int64         `json:"failed_requests"`

	RequestsPerSecond float64 `json:"requests_per_second"`
	SuccessQPS        float64 `json:"success_qps"`
	RejectedQPS       float64 `json:"rejected_qps"`

	AvgLatency time.Duration `json:"avg_latency_ns"`
	P50Latency time.Duration `json:"p50_latency_ns"`
	P95Latency time.Duration `json:"p95_latency_ns"`
	P99Latency time.Duration `json:"p99_latency_ns"`

	AvgSuccessLatency time.Duration `json:"avg_success_latency_ns"`
	P50SuccessLatency time.Duration `json:"p50_success_latency_ns"`
	P95SuccessLatency time.Duration `json:"p95_success_latency_ns"`
	P99SuccessLatency time.Duration `json:"p99_success_latency_ns"`

	AvgRejectedLatency time.Duration `json:"avg_rejected_latency_ns"`
	P50RejectedLatency time.Duration `json:"p50_rejected_latency_ns"`
	P95RejectedLatency time.Duration `json:"p95_rejected_latency_ns"`
	P99RejectedLatency time.Duration `json:"p99_rejected_latency_ns"`

	RateLimitCheckQPS        float64       `json:"rate_limit_check_qps"`
	AvgRateLimitCheckLatency time.Duration `json:"avg_rate_limit_check_latency_ns"`
	P50RateLimitCheckLatency time.Duration `json:"p50_rate_limit_check_latency_ns"`
	P95RateLimitCheckLatency time.Duration `json:"p95_rate_limit_check_latency_ns"`
	P99RateLimitCheckLatency time.Duration `json:"p99_rate_limit_check_latency_ns"`

	// Remote Token Fetch Metrics
	RemoteFetchQPS        float64       `json:"remote_fetch_qps"`
	AvgRemoteFetchLatency time.Duration `json:"avg_remote_fetch_latency_ns"`
	P50RemoteFetchLatency time.Duration `json:"p50_remote_fetch_latency_ns"`
	P95RemoteFetchLatency time.Duration `json:"p95_remote_fetch_latency_ns"`
	P99RemoteFetchLatency time.Duration `json:"p99_remote_fetch_latency_ns"`

	AvgTokenFetchWaiters float64 `json:"avg_token_fetch_waiters"`

	ActiveConnections int64 `json:"active_connections"`
	TotalConnections  int64 `json:"total_connections"`  // New connections opened
	ClosedConnections int64 `json:"closed_connections"` // Connections closed
}

// String returns a human-readable string representation
func (s *MetricsSnapshot) String() string {
	return fmt.Sprintf(
		"Requests: %d (%.0f/s) [Success: %.0f/s, Rejected: %.0f/s] | Check: %.0f/s (%.1fms) | Fetch: %.0f/s (%.1fms) [Wait: %.1f] | Success: %d | RL: %d | Latency: avg=%v p99=%v | Conns: %d (new: %d, closed: %d)",
		s.TotalRequests,
		s.RequestsPerSecond,
		s.SuccessQPS,
		s.RejectedQPS,
		s.RateLimitCheckQPS,
		float64(s.AvgRateLimitCheckLatency.Nanoseconds())/1000000.0,
		s.RemoteFetchQPS,
		float64(s.AvgRemoteFetchLatency.Nanoseconds())/1000000.0,
		s.AvgTokenFetchWaiters,
		s.SuccessfulRequests,
		s.RateLimitedRequests,
		s.AvgLatency.Round(time.Microsecond),
		s.P99Latency.Round(time.Microsecond),
		s.ActiveConnections,
		s.TotalConnections,
		s.ClosedConnections,
	)
}

// SaveJSON saves the snapshot to a JSON file
func (s *MetricsSnapshot) SaveJSON(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// fnv1a computes FNV-1a hash (same as in ratelimiter for consistency)
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
