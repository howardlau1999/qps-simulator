package client

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

// Connection represents a simulated client-server connection
type Connection struct {
	id           string
	serverID     string
	maxRequests  int64
	requestCount int64
	shouldClose  bool
	closed       bool
	mu           sync.Mutex
	createdAt    time.Time
}

// NewConnection creates a new connection
func NewConnection(id, serverID string, maxRequests int64) *Connection {
	return &Connection{
		id:          id,
		serverID:    serverID,
		maxRequests: maxRequests,
		createdAt:   time.Now(),
	}
}

func (c *Connection) ID() string {
	return c.id
}

func (c *Connection) ServerID() string {
	return c.serverID
}

func (c *Connection) RequestCount() int64 {
	return atomic.LoadInt64(&c.requestCount)
}

func (c *Connection) MaxRequests() int64 {
	return c.maxRequests
}

func (c *Connection) ShouldClose() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shouldClose || atomic.LoadInt64(&c.requestCount) >= c.maxRequests
}

// MarkShouldClose marks the connection for closing (server instruction)
func (c *Connection) MarkShouldClose() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shouldClose = true
}

// IncrementRequests increments the request count and returns true if connection should close
func (c *Connection) IncrementRequests() bool {
	count := atomic.AddInt64(&c.requestCount, 1)
	return count >= c.maxRequests
}

func (c *Connection) Send(ctx context.Context, req *types.Request) (*types.Response, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("connection closed")
	}
	c.mu.Unlock()

	shouldClose := c.IncrementRequests()

	// Simulate request processing (this will be handled by server in real simulation)
	resp := &types.Response{
		RequestID:   req.ID,
		StatusCode:  200,
		Headers:     make(map[string]string),
		ShouldClose: shouldClose,
		Timestamp:   time.Now(),
	}

	return resp, nil
}

func (c *Connection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// ============================================================================
// Connection Pool
// ============================================================================

// Pool manages a pool of connections per server
type Pool struct {
	maxConnsPerServer int
	maxRequestsPerConn int64
	pools             map[string]*serverPool
	mu                sync.RWMutex
	stats             types.ConnectionPoolStats
	closed            bool
}

type serverPool struct {
	serverID    string
	connections chan *Connection
	active      int64
	total       int64
	mu          sync.Mutex
}

// NewPool creates a new connection pool
func NewPool(maxConnsPerServer int, maxRequestsPerConn int64) *Pool {
	if maxConnsPerServer <= 0 {
		maxConnsPerServer = 100
	}
	if maxRequestsPerConn <= 0 {
		maxRequestsPerConn = 150
	}

	return &Pool{
		maxConnsPerServer:  maxConnsPerServer,
		maxRequestsPerConn: maxRequestsPerConn,
		pools:              make(map[string]*serverPool),
	}
}

func (p *Pool) getServerPool(serverID string) *serverPool {
	p.mu.RLock()
	sp, ok := p.pools[serverID]
	p.mu.RUnlock()

	if ok {
		return sp
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock
	if sp, ok = p.pools[serverID]; ok {
		return sp
	}

	sp = &serverPool{
		serverID:    serverID,
		connections: make(chan *Connection, p.maxConnsPerServer),
	}
	p.pools[serverID] = sp
	return sp
}

// Get retrieves or creates a connection to the specified server
func (p *Pool) Get(ctx context.Context, server *types.ServerInfo) (types.Connection, error) {
	if p.closed {
		return nil, fmt.Errorf("pool is closed")
	}

	sp := p.getServerPool(server.ID)

	// Try to get an existing connection
	select {
	case conn := <-sp.connections:
		if !conn.ShouldClose() {
			atomic.AddInt64(&sp.active, 1)
			return conn, nil
		}
		// Connection is at max requests, close it
		conn.Close()
		atomic.AddInt64(&p.stats.TotalConnections, -1)
	default:
	}

	// Create new connection
	connID := fmt.Sprintf("conn-%s-%d", server.ID, atomic.AddInt64(&sp.total, 1))
	conn := NewConnection(connID, server.ID, p.maxRequestsPerConn)
	atomic.AddInt64(&p.stats.TotalConnections, 1)
	atomic.AddInt64(&sp.active, 1)
	return conn, nil
}

// Put returns a connection to the pool
func (p *Pool) Put(conn types.Connection) error {
	if p.closed {
		return conn.Close()
	}

	c, ok := conn.(*Connection)
	if !ok {
		return conn.Close()
	}

	sp := p.getServerPool(c.ServerID())
	atomic.AddInt64(&sp.active, -1)

	if c.ShouldClose() {
		atomic.AddInt64(&p.stats.TotalConnections, -1)
		return c.Close()
	}

	// Try to return to pool
	select {
	case sp.connections <- c:
		return nil
	default:
		// Pool is full, close connection
		atomic.AddInt64(&p.stats.TotalConnections, -1)
		return c.Close()
	}
}

// Close closes all connections in the pool
func (p *Pool) Close() error {
	p.mu.Lock()
	p.closed = true
	pools := p.pools
	p.pools = make(map[string]*serverPool)
	p.mu.Unlock()

	for _, sp := range pools {
		close(sp.connections)
		for conn := range sp.connections {
			conn.Close()
		}
	}
	return nil
}

// Stats returns pool statistics
func (p *Pool) Stats() types.ConnectionPoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var active, idle int64
	for _, sp := range p.pools {
		active += atomic.LoadInt64(&sp.active)
		idle += int64(len(sp.connections))
	}

	return types.ConnectionPoolStats{
		TotalConnections:  atomic.LoadInt64(&p.stats.TotalConnections),
		ActiveConnections: active,
		IdleConnections:   idle,
		TotalRequests:     atomic.LoadInt64(&p.stats.TotalRequests),
	}
}
