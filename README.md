# QPS Simulator

A high-performance distributed QPS (Queries Per Second) rate limiting simulator written in Go. Designed to test and verify rate limiting policies at scale (up to 1M+ QPS).

## Features

- **Multiple Rate Limiting Algorithms**: Token Bucket, Sliding Window, Fixed Window, Leaky Bucket, Local-Cached Token Bucket
- **Pluggable Architecture**: Easy to add custom rate limiting algorithms
- **High Performance**: Sharded state for lock-free operation at high concurrency
- **Load Balancing**: Round-robin, Random, Weighted, Least-connections algorithms
- **Flexible Connection Modes**: Reuse, Per-request, or Hybrid (switchable at runtime)
- **Connection Lifecycle**: Configurable max requests per connection (default: 150)
- **Centralized Rate Limiting**: Local-Cached Token Bucket for high-throughput distributed scenarios
- **Real-time Metrics**: Detailed QPS, granular latency (check vs fetch), and HTML Dashboard visualization
- **Latency Simulation**: configurable delays for rate limit checks and remote fetches

## Quick Start

```bash
# Build
go build ./cmd/simulator

# Run with default config
./simulator

# Run with custom config
./simulator --config configs/sample.yaml
```

## Configuration

```yaml
clients:
  count: 10                    # Number of concurrent clients
  requests_per_client: 10000   # Requests per client
  connection_mode: "hybrid"    # reuse, per_request, or hybrid
  request_rate: 1000           # Target QPS per client

servers:
  count: 3                     # Number of backend servers
  max_requests_per_conn: 150   # Max requests before connection close

load_balancer:
  algorithm: "round_robin"     # round_robin, random, weighted, least_conn

rate_limiter:
  type: "local_cached"         # token_bucket, sliding_window, fixed_window, leaky_bucket, local_cached
  rate: 10000                  # Requests per second
  burst: 1000                  # Burst capacity
  shards: 256                  # Shards for high concurrency (1M+ QPS)
  prefetch_count: 50           # (For local_cached) Tokens to prefetch per batch

simulation:
  duration_seconds: 60
  warmup_seconds: 5

  # Latency Simulation
  remote_check_delay_ms: 10    # Simulated latency for remote token fetch
  remote_check_jitter_ms: 2    # Jitter for remote fetch
  rejected_delay_ms: 5         # Latency penalty for rejected requests
```

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    HTTP Clients (N)                          │
│         (Connection modes: reuse/per_request/hybrid)         │
└─────────────────────────┬───────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│                   TCP Load Balancer                          │
│     (round_robin, random, weighted, least_connections)       │
└─────────────────────────┬───────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│                   HTTP Servers (M)                           │
│          (Max 150 requests per connection)                   │
└─────────────────────────┬───────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│                    Rate Limiter                              │
│   (token_bucket, sliding_window, fixed_window, leaky_bucket) │
│              or Remote Rate Limiter (gRPC)                   │
└─────────────────────────────────────────────────────────────┘
```

## Adding Custom Rate Limiters

Implement the `RateLimiter` interface and register it:

```go
package custom

import (
    "github.com/howardlau1999/qps-simulator/pkg/ratelimiter"
    "github.com/howardlau1999/qps-simulator/pkg/types"
)

func init() {
    ratelimiter.Register("my_algorithm", NewMyRateLimiter)
}

func NewMyRateLimiter(config types.RateLimiterConfig) (types.RateLimiter, error) {
    // Your implementation
}
```

## Metrics Output

```
[4s] Requests: 103803 (27162/s) [Success: 5000/s, Rejected: 22162/s] | Check: 27096/s (4.8ms) | Fetch: 525/s (11.3ms) [Wait: 124.0] | Success: 46000 | RL: 57858 | Latency: avg=11.018ms p99=22.171ms | Conns: 3000 (new: 3000, closed: 0)

```

## License

MIT
