package client

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

// Client represents an HTTP client simulator
type Client struct {
	id             string
	loadBalancer   types.LoadBalancer
	pool           *Pool
	mode           types.ConnectionMode
	modeMu         sync.RWMutex
	stats          clientStats
	headers        map[string]string
	maxRequestsPerConn int64
	closed         bool
	closeMu        sync.RWMutex
	transport      Transport
	sem            chan struct{} // Semaphore for concurrency limiting
}

type clientStats struct {
	totalRequests      int64
	successfulRequests int64
	rateLimitedRequests int64
	failedRequests     int64
	connectionsOpened  int64
	connectionsClosed  int64
	totalLatencyNs     int64
}

// ClientConfig holds client configuration
type ClientConfig struct {
	ID                 string
	LoadBalancer       types.LoadBalancer
	ConnectionMode     types.ConnectionMode
	MaxConnsPerServer  int
	MaxRequestsPerConn int64
	Headers            map[string]string
	Transport          Transport
	MaxConcurrency     int
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
		pool:               NewPool(config.MaxConnsPerServer, config.MaxRequestsPerConn, config.Transport),
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

	// Pick a server
	server, err := c.loadBalancer.PickServer()
	if err != nil {
		atomic.AddInt64(&c.stats.failedRequests, 1)
		return nil, fmt.Errorf("failed to pick server: %w", err)
	}

	mode := c.ConnectionMode()
	var conn types.Connection
	var usePool bool

	switch mode {
	case types.ConnectionModeReuse:
		usePool = true
	case types.ConnectionModePerRequest:
		usePool = false
	case types.ConnectionModeHybrid:
		// In hybrid mode, use connection reuse for most requests
		// but occasionally create new connections
		usePool = atomic.LoadInt64(&c.stats.totalRequests)%10 != 0
	}

	if usePool {
		conn, err = c.pool.Get(ctx, server)
		if err != nil {
			atomic.AddInt64(&c.stats.failedRequests, 1)
			return nil, fmt.Errorf("failed to get connection: %w", err)
		}
		atomic.AddInt64(&c.stats.connectionsOpened, 1)
	} else {
		// Create a new connection for this request
		connID := fmt.Sprintf("conn-%s-%d", server.ID, time.Now().UnixNano())
		conn = NewConnection(connID, server.ID, c.maxRequestsPerConn, c.transport)
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
		if usePool {
			c.pool.Put(conn) // Pool will handle the close
		} else {
			conn.Close()
		}
		atomic.AddInt64(&c.stats.connectionsClosed, 1)
	} else if usePool {
		c.pool.Put(conn)
	} else {
		conn.Close()
		atomic.AddInt64(&c.stats.connectionsClosed, 1)
	}

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
	return c.pool.Close()
}
