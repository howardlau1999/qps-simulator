# QPS Simulator

A high-performance distributed QPS (Queries Per Second) rate limiting simulator written in Go. Designed to test and verify rate limiting policies at scale (up to 1M+ QPS).

## Features

- **Multiple Rate Limiting Algorithms**: Token Bucket, Sliding Window, Fixed Window, Leaky Bucket
- **Pluggable Architecture**: Easy to add custom rate limiting algorithms
- **High Performance**: Sharded state for lock-free operation at high concurrency
- **Load Balancing**: Round-robin, Random, Weighted, Least-connections algorithms
- **Flexible Connection Modes**: Reuse, Per-request, or Hybrid (switchable at runtime)
- **Connection Lifecycle**: Configurable max requests per connection (default: 150)
- **Centralized Rate Limiting**: Optional gRPC-based remote rate limiter service
- **Real-time Metrics**: QPS, latency percentiles (p50, p95, p99), success/failure rates

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
  type: "token_bucket"         # token_bucket, sliding_window, fixed_window, leaky_bucket
  rate: 10000                  # Requests per second
  burst: 1000                  # Burst capacity
  shards: 256                  # Shards for high concurrency (1M+ QPS)

simulation:
  duration_seconds: 60
  warmup_seconds: 5
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
[1s] Requests: 10234 (10234/s) | Success: 8500 | RateLimited: 1734 | Latency: avg=1.2ms p50=1ms p95=2ms p99=5ms
```

## License

MIT
