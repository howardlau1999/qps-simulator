package client

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

// ContextKey is a type for context keys
type ContextKey string

// WorkerIDKey is the context key for the worker ID
const WorkerIDKey ContextKey = "worker_id"

// DedicatedConnKey is the context key for the dedicated connection
const DedicatedConnKey ContextKey = "dedicated_conn"

// Client represents an HTTP client simulator
type Client struct {
	id                 string
	loadBalancer       types.LoadBalancer
	pool               *Pool
	mode               types.ConnectionMode
	modeMu             sync.RWMutex
	stats              clientStats
	headers            map[string]string
	maxRequestsPerConn int64
	closed             bool
	closeMu            sync.RWMutex
	transport          Transport
	sem                chan struct{} // Semaphore for concurrency limiting
}

type clientStats struct {
	totalRequests       int64
	successfulRequests  int64
	rateLimitedRequests int64
	failedRequests      int64
	connectionsOpened   int64
	connectionsClosed   int64
	totalLatencyNs      int64
}

// ClientConfig holds client configuration
type ClientConfig struct {
	ID                     string
	LoadBalancer           types.LoadBalancer
	ConnectionMode         types.ConnectionMode
	ConnectionPoolStrategy string
	MaxConnsPerServer      int
	MaxRequestsPerConn     int64
	Headers                map[string]string
	Transport              Transport
	MaxConcurrency         int
	MaxConnections         int
}

// NewClient creates a new HTTP client simulator
func NewClient(config ClientConfig) *Client {
	if config.MaxRequestsPerConn <= 0 {
		config.MaxRequestsPerConn = 150
	}
	if config.MaxConnsPerServer <= 0 {
		config.MaxConnsPerServer = 100
	}

	var sem chan struct{}
	if config.MaxConcurrency > 0 {
		sem = make(chan struct{}, config.MaxConcurrency)
	}

	return &Client{
		id:                 config.ID,
		loadBalancer:       config.LoadBalancer,
		pool:               NewPool(config.ID, config.MaxRequestsPerConn, config.Transport, config.ConnectionPoolStrategy, config.MaxConnections),
		mode:               config.ConnectionMode,
		headers:            config.Headers,
		maxRequestsPerConn: config.MaxRequestsPerConn,
		transport:          config.Transport,
		sem:                sem,
	}
}

func (c *Client) ID() string {
	return c.id
}

// SetConnectionMode changes the connection mode dynamically
func (c *Client) SetConnectionMode(mode types.ConnectionMode) {
	c.modeMu.Lock()
	defer c.modeMu.Unlock()
	c.mode = mode
}

func (c *Client) ConnectionMode() types.ConnectionMode {
	c.modeMu.RLock()
	defer c.modeMu.RUnlock()
	return c.mode
}

// CreateDedicatedConnection creates a connection for a worker
// Returns the connection for the caller to manage
func (c *Client) CreateDedicatedConnection(ctx context.Context) (types.Connection, error) {
	// Pick a server and create connection
	server, err := c.loadBalancer.PickServer()
	if err != nil {
		return nil, fmt.Errorf("failed to pick server: %w", err)
	}

	conn, err := c.pool.Create(ctx, server.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection: %w", err)
	}
	atomic.AddInt64(&c.stats.connectionsOpened, 1)

	return conn, nil
}

// Send sends a request to a server selected by the load balancer
func (c *Client) Send(ctx context.Context, req *types.Request) (*types.Response, error) {
	c.closeMu.RLock()
	if c.closed {
		c.closeMu.RUnlock()
		return nil, fmt.Errorf("client is closed")
	}
	c.closeMu.RUnlock()

	// Acquire semaphore if configured
	if c.sem != nil {
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	start := time.Now()
	atomic.AddInt64(&c.stats.totalRequests, 1)

	// Merge client headers with request headers
	for k, v := range c.headers {
		if _, exists := req.Headers[k]; !exists {
			req.Headers[k] = v
		}
	}
	req.ClientID = c.id

	mode := c.ConnectionMode()
	var conn types.Connection
	var usePool bool
	var err error

	switch mode {
	case types.ConnectionModeReuse:
		usePool = true
	case types.ConnectionModePerRequest:
		usePool = false
	case types.ConnectionModeHybrid:
		// In hybrid mode, use connection reuse for most requests
		// but occasionally create new connections
		usePool = atomic.LoadInt64(&c.stats.totalRequests)%10 != 0
	case types.ConnectionModeDedicated:
		// Dedicated mode logic handling below
		usePool = false
	}

	if mode == types.ConnectionModeDedicated {
		// Get connection from context (worker manages its own connection)
		connVal := ctx.Value(DedicatedConnKey)
		if connVal == nil {
			// No connection yet, create one
			conn, err = c.CreateDedicatedConnection(ctx)
			if err != nil {
				atomic.AddInt64(&c.stats.failedRequests, 1)
				return nil, err
			}
		} else {
			conn = connVal.(types.Connection)
			// Check if connection is still valid
			if conn.ShouldClose() {
				conn.Close()
				atomic.AddInt64(&c.stats.connectionsClosed, 1)
				// Create replacement
				conn, err = c.CreateDedicatedConnection(ctx)
				if err != nil {
					atomic.AddInt64(&c.stats.failedRequests, 1)
					return nil, err
				}
			}
		}

		// Use the connection
		// Note: We don't set usePool = true, so we handle cleanup delicately below
	} else if usePool {
		// Define picker closure to lazy-load server
		picker := func() (string, error) {
			// Pick server first
			server, err := c.loadBalancer.PickServer()
			if err != nil {
				return "", err
			}
			return server.ID, nil
		}

		conn, err = c.pool.Get(ctx, picker)
		if err != nil {
			atomic.AddInt64(&c.stats.failedRequests, 1)
			return nil, fmt.Errorf("failed to get connection: %w", err)
		}
		atomic.AddInt64(&c.stats.connectionsOpened, 1)
	} else {
		// Create a new connection for this request
		// Must pick server explicitly here
		server, err := c.loadBalancer.PickServer()
		if err != nil {
			atomic.AddInt64(&c.stats.failedRequests, 1)
			return nil, fmt.Errorf("failed to pick server: %w", err)
		}

		conn, err = c.pool.Create(ctx, server.ID)
		if err != nil {
			atomic.AddInt64(&c.stats.failedRequests, 1)
			return nil, fmt.Errorf("failed to create connection: %w", err)
		}
		atomic.AddInt64(&c.stats.connectionsOpened, 1)
	}

	// Send request
	resp, err := conn.Send(ctx, req)

	latency := time.Since(start)
	atomic.AddInt64(&c.stats.totalLatencyNs, int64(latency))

	if err != nil {
		atomic.AddInt64(&c.stats.failedRequests, 1)
		if usePool {
			c.pool.Put(conn)
		} else {
			conn.Close()
			atomic.AddInt64(&c.stats.connectionsClosed, 1)
		}
		return nil, err
	}

	// Handle rate limiting response
	if resp.RateLimited {
		atomic.AddInt64(&c.stats.rateLimitedRequests, 1)
	} else {
		atomic.AddInt64(&c.stats.successfulRequests, 1)
	}

	// Handle connection close instruction from server
	if resp.ShouldClose || conn.ShouldClose() {
		if mode == types.ConnectionModeDedicated {
			// For dedicated mode, just close - worker will create new one on next Send
			// The new connection will be created because context won't have it, or ShouldClose check
			conn.Close()
			atomic.AddInt64(&c.stats.connectionsClosed, 1)
			// Set flag in response so worker knows to update its local connection
			resp.ShouldClose = true
		} else if usePool {
			c.pool.Put(conn) // Pool will handle the close
			atomic.AddInt64(&c.stats.connectionsClosed, 1)
		} else {
			conn.Close()
			atomic.AddInt64(&c.stats.connectionsClosed, 1)
		}
	} else if usePool {
		c.pool.Put(conn)
	} else if mode != types.ConnectionModeDedicated {
		// For non-dedicated, non-pool modes: close
		conn.Close()
		atomic.AddInt64(&c.stats.connectionsClosed, 1)
	}
	// For dedicated mode without close: connection stays valid for next request

	resp.ProcessingTime = latency
	return resp, nil
}

// Stats returns client statistics
func (c *Client) Stats() types.ClientStats {
	total := atomic.LoadInt64(&c.stats.totalRequests)
	var avgLatency time.Duration
	if total > 0 {
		avgLatency = time.Duration(atomic.LoadInt64(&c.stats.totalLatencyNs) / total)
	}

	return types.ClientStats{
		TotalRequests:       total,
		SuccessfulRequests:  atomic.LoadInt64(&c.stats.successfulRequests),
		RateLimitedRequests: atomic.LoadInt64(&c.stats.rateLimitedRequests),
		FailedRequests:      atomic.LoadInt64(&c.stats.failedRequests),
		ConnectionsOpened:   atomic.LoadInt64(&c.stats.connectionsOpened),
		ConnectionsClosed:   atomic.LoadInt64(&c.stats.connectionsClosed),
		AvgLatency:          avgLatency,
	}
}

// Close shuts down the client
func (c *Client) Close() error {
	c.closeMu.Lock()
	c.closed = true
	c.closeMu.Unlock()

	// Note: dedicated connections are managed by workers, they will be closed when workers exit
	return c.pool.Close()
}
