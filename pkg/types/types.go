package types

import (
	"context"
	"io"
	"time"
)

// ConnectionMode defines how clients manage connections
type ConnectionMode int

const (
	// ConnectionModeReuse reuses connections for multiple requests
	ConnectionModeReuse ConnectionMode = iota
	// ConnectionModePerRequest creates a new connection per request
	ConnectionModePerRequest
	// ConnectionModeHybrid allows switching between modes dynamically
	ConnectionModeHybrid
	// ConnectionModeDedicated assigns a persistent connection to each worker goroutine
	ConnectionModeDedicated
)

// Request represents an HTTP request in the simulation
type Request struct {
	ID        string
	ClientID  string
	Headers   map[string]string
	Timestamp time.Time
	Body      []byte
}

// Response represents an HTTP response in the simulation
type Response struct {
	RequestID      string
	StatusCode     int
	Headers        map[string]string
	RateLimited    bool
	ShouldClose    bool // Server instructs client to close connection
	Timestamp      time.Time
	ProcessingTime time.Duration
}

// ServerInfo represents a backend server
type ServerInfo struct {
	ID      string
	Address string
	Port    int
	Weight  int // For weighted load balancing
	Healthy bool
}

// RateLimitResult contains detailed rate limiting decision info
type RateLimitResult struct {
	Allowed       bool
	Remaining     int64  // Remaining requests in current window
	ResetAfter    time.Duration
	RetryAfter    time.Duration
	LimitedBy     string // Which key/rule caused the limit
}

// RateLimiter defines the plugin interface for rate limiting algorithms
// Implementations must be thread-safe and optimized for high concurrency (1M+ QPS)
type RateLimiter interface {
	// Allow checks if a request should be allowed based on key and headers
	// Must be lock-free or use sharding for high performance
	Allow(ctx context.Context, key string, headers map[string]string) (*RateLimitResult, error)
	
	// AllowN checks if N requests should be allowed (batch operation)
	AllowN(ctx context.Context, key string, n int64) (*RateLimitResult, error)
	
	// Reset resets the rate limiter state for a key
	Reset(key string) error
	
	// Config returns the current configuration
	Config() RateLimiterConfig
	
	// Close cleans up resources
	Close() error
}

// RateLimiterConfig holds rate limiter configuration
type RateLimiterConfig struct {
	Type      string                 // Algorithm type: token_bucket, sliding_window, etc.
	Rate      int64                  // Requests per second
	Burst     int64                  // Maximum burst size
	Window    time.Duration          // Window size for window-based algorithms
	Shards    int                    // Number of shards for high concurrency
	ExtraOpts map[string]interface{} // Algorithm-specific options
}

// RateLimiterFactory creates rate limiters
type RateLimiterFactory func(config RateLimiterConfig) (RateLimiter, error)

// LoadBalancer defines the interface for load balancing strategies
type LoadBalancer interface {
	// PickServer selects a server for the client request
	PickServer() (*ServerInfo, error)
	
	// RegisterServer adds a server to the pool
	RegisterServer(server *ServerInfo) error
	
	// RemoveServer removes a server from the pool
	RemoveServer(serverID string) error
	
	// MarkUnhealthy marks a server as unhealthy
	MarkUnhealthy(serverID string)
	
	// MarkHealthy marks a server as healthy
	MarkHealthy(serverID string)
	
	// Servers returns all registered servers
	Servers() []*ServerInfo
}

// Connection represents a client-server connection
type Connection interface {
	ID() string
	ServerID() string
	RequestCount() int64
	MaxRequests() int64
	ShouldClose() bool
	Send(ctx context.Context, req *Request) (*Response, error)
	Close() error
}

// ConnectionPool manages a pool of connections
type ConnectionPool interface {
	// Get retrieves a connection from the pool (load balanced)
	Get(ctx context.Context) (Connection, error)
	
	// Create creates a new connection (load balanced, not pooled)
	Create(ctx context.Context) (Connection, error)

	// Put returns a connection to the pool
	Put(conn Connection) error
	
	// Close closes all connections in the pool
	Close() error
	
	// Stats returns pool statistics
	Stats() ConnectionPoolStats
}

// ConnectionPoolStats contains connection pool statistics
type ConnectionPoolStats struct {
	TotalConnections  int64
	ActiveConnections int64
	IdleConnections   int64
	TotalRequests     int64
}

// Client represents an HTTP client in the simulation
type Client interface {
	// ID returns the client identifier
	ID() string
	
	// Send sends a request and returns the response
	Send(ctx context.Context, req *Request) (*Response, error)
	
	// SetConnectionMode changes the connection mode
	SetConnectionMode(mode ConnectionMode)
	
	// ConnectionMode returns the current connection mode
	ConnectionMode() ConnectionMode
	
	// Stats returns client statistics
	Stats() ClientStats
	
	// Close shuts down the client
	Close() error
}

// ClientStats contains client statistics
type ClientStats struct {
	TotalRequests     int64
	SuccessfulRequests int64
	RateLimitedRequests int64
	FailedRequests    int64
	ConnectionsOpened int64
	ConnectionsClosed int64
	AvgLatency        time.Duration
}

// Server represents an HTTP server in the simulation
type Server interface {
	// ID returns the server identifier
	ID() string
	
	// Address returns the server address
	Address() string
	
	// Start starts the server
	Start() error
	
	// Stop stops the server gracefully
	Stop(ctx context.Context) error
	
	// Stats returns server statistics
	Stats() ServerStats
	
	// SetRateLimiter sets the rate limiter for this server
	SetRateLimiter(limiter RateLimiter)
}

// ServerStats contains server statistics
type ServerStats struct {
	TotalRequests      int64
	SuccessfulRequests int64
	RateLimitedRequests int64
	ActiveConnections  int64
	TotalConnections   int64  // New connections opened
	ClosedConnections  int64  // Connections closed
}

// Metrics collects and reports simulation metrics
type Metrics interface {
	// RecordRequest records a request metric
	RecordRequest(clientID, serverID string, latency time.Duration, rateLimited bool)
	
	// RecordConnection records a connection event
	RecordConnection(clientID, serverID string, opened bool)
	
	// Snapshot returns a point-in-time snapshot of metrics
	Snapshot() *MetricsSnapshot
	
	// Reset resets all metrics
	Reset()
	
	io.Closer
}

// MetricsSnapshot contains a point-in-time view of metrics
type MetricsSnapshot struct {
	Timestamp           time.Time
	TotalRequests       int64
	SuccessfulRequests  int64
	RateLimitedRequests int64
	FailedRequests      int64
	
	RequestsPerSecond   float64
	SuccessQPS          float64
	RejectedQPS         float64
	
	AvgLatency          time.Duration
	P50Latency          time.Duration
	P95Latency          time.Duration
	P99Latency          time.Duration
	
	AvgSuccessLatency   time.Duration
	P50SuccessLatency   time.Duration
	P95SuccessLatency   time.Duration
	P99SuccessLatency   time.Duration
	
	AvgRejectedLatency  time.Duration
	P50RejectedLatency  time.Duration
	P95RejectedLatency  time.Duration
	P99RejectedLatency  time.Duration
	
	ActiveConnections   int64
	TotalConnections    int64
	ClosedConnections   int64
	
	// Rate Limit Check Metrics
	RateLimitCheckQPS      float64
	AvgRateLimitCheckLatency time.Duration
	P50RateLimitCheckLatency time.Duration
	P95RateLimitCheckLatency time.Duration
	P99RateLimitCheckLatency time.Duration

	// Remote Token Fetch Metrics (for local-cached algorithm)
	RemoteFetchQPS           float64
	AvgRemoteFetchLatency    time.Duration
	P50RemoteFetchLatency    time.Duration
	P95RemoteFetchLatency    time.Duration
	P99RemoteFetchLatency    time.Duration
	
	AvgTokenFetchWaiters     float64
}

// MetricsRecorder defines the interface for recording specific metrics
type MetricsRecorder interface {
	RecordRateLimitCheck(latency time.Duration)
	RecordRemoteTokenFetch(latency time.Duration)
	RecordTokenFetchWait(duration time.Duration)
}
