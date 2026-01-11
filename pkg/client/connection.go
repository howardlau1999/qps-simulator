package client

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

// Transport defines the interface for communicating with the server
type Transport interface {
	Send(ctx context.Context, serverID, connID string, req *types.Request) (*types.Response, error)
	CloseConnection(serverID, connID string)
}

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
	transport    Transport
}

// NewConnection creates a new connection
func NewConnection(id, serverID string, maxRequests int64, transport Transport) *Connection {
	return &Connection{
		id:          id,
		serverID:    serverID,
		maxRequests: maxRequests,
		createdAt:   time.Now(),
		transport:   transport,
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

	if c.transport == nil {
		return nil, fmt.Errorf("no transport configured")
	}

	resp, err := c.transport.Send(ctx, c.serverID, c.id, req)
	if err != nil {
		return nil, err
	}

	if shouldClose {
		resp.ShouldClose = true
	}

	return resp, nil
}

func (c *Connection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	
	if c.transport != nil {
		c.transport.CloseConnection(c.serverID, c.id)
	}
	
	return nil
}

// ============================================================================
// Connection Pool
// ============================================================================

// Pool manages a pool of connections
type Pool struct {
	maxConnsPerServer  int // Not strictly enforced by Pool anymore if we pass limit responsibility? 
    // Wait, the user said "connection pool should also be unaware of which server it connects to".
    // Does it still need to enforce "max conns per server"?
    // If it's unaware, maybe it doesn't enforce per-server limit?
    // "Support setting max connections count for per client connection pool" (Global) was the previous goal.
    // If I just enforce global, I don't need to track per-server counts in map.
    // But `maxConnsPerServer` config still exists. If the pool is unaware, the client must enforce it?
    // Or maybe we treat `maxConnsPerServer` as irrelevant now that we have global `MaxConnections`.
    // Let's assume for now we only care about Global Limit in the Pool, OR we just scan to count?
    // Scanning to count per-server is O(N) but N is small (100).
    // Let's keep `maxConnsPerServer` for backward compatibility if needed, but primarily use Global.
    // Actually, if we don't have a map, checking per-server count requires iteration.
	maxRequestsPerConn int64
	maxTotalConnections int
	
    idleConnections    []*Connection
	mu                 sync.RWMutex
	stats              types.ConnectionPoolStats
	closed             bool
	transport          Transport
	strategy           string
    // Removed LoadBalancer and clientID (maybe clientID is useful for logging? kept it)
	clientID           string
	globalCond         *sync.Cond
}

// NewPool creates a new connection pool
func NewPool(clientID string, maxRequestsPerConn int64, transport Transport, strategy string, maxTotalConnections int) *Pool {
	if maxRequestsPerConn <= 0 {
		maxRequestsPerConn = 150
	}
	if strategy == "" {
		strategy = "fifo"
	}

	p := &Pool{
		maxRequestsPerConn:  maxRequestsPerConn,
		maxTotalConnections: maxTotalConnections,
		idleConnections:     make([]*Connection, 0),
		transport:           transport,
		strategy:            strategy,
		clientID:            clientID,
	}
	p.globalCond = sync.NewCond(&p.mu)
	return p
}

// Get retrieves a reusable connection or creates a new one using the picker
func (p *Pool) Get(ctx context.Context, picker func() (string, error)) (types.Connection, error) {
	if p.closed {
		return nil, fmt.Errorf("pool is closed")
	}

	p.mu.Lock()
	
	// 1. Try to reuse ANY idle connection
	if len(p.idleConnections) > 0 {
		// Pop the last one (LIFO) or first one (FIFO)
		// Strategy check
		var conn *Connection
		idx := 0
		if p.strategy == "lifo" {
			idx = len(p.idleConnections) - 1
		}
		
		conn = p.idleConnections[idx]
		
		// Remove from slice
		if p.strategy == "lifo" {
			p.idleConnections = p.idleConnections[:idx]
		} else {
			p.idleConnections = p.idleConnections[1:]
		}
		
		p.mu.Unlock()
		
		// Just reuse it, assuming it's healthy.
		// If it needs to stay open, TotalConnections remains same.
		return conn, nil
	}
	
	// 2. No idle connection. Must create new.
	// Check global limit
	if p.maxTotalConnections > 0 {
		for {
			total := atomic.LoadInt64(&p.stats.TotalConnections)
			if total < int64(p.maxTotalConnections) {
				break
			}
			
			// Try to evict an idle connection to make room
			// Wait... if we are here, len(idleConnections) was 0 just a moment ago.
			// But maybe someone returned one while we were checking atomic?
			// Re-check idleConnections under lock?
			// But we are under lock!
			// So if len(p.idleConnections) == 0, we can't evict anything!
			// We MUST wait.
			
			// Wait, another thread might have put one back?
			// So we loop.
			
			// Ah, if len > 0, we should have just taken it in step 1?
			// No, because we entered step 2.
			
			// So the logic inside loop:
			// If idle > 0, TAKE IT (reuse). Don't Create.
			if len(p.idleConnections) > 0 {
				// We found one while waiting! Use it!
				// Same logic as step 1
				var conn *Connection
				idx := 0
				if p.strategy == "lifo" {
					idx = len(p.idleConnections) - 1
				}
				conn = p.idleConnections[idx]
				if p.strategy == "lifo" {
					p.idleConnections = p.idleConnections[:idx]
				} else {
					p.idleConnections = p.idleConnections[1:]
				}
				p.mu.Unlock()
				return conn, nil
			}
			
			// Limit reached and no idle connections to evict?
			// We block.
			p.globalCond.Wait()
			if p.closed {
				p.mu.Unlock()
				return nil, fmt.Errorf("pool is closed")
			}
		}
	}
	
	// We are allowed to create a new connection.
	// We are holding the lock.
	// Reserve the slot.
	atomic.AddInt64(&p.stats.TotalConnections, 1)
	p.mu.Unlock()

	// 3. Pick server (Load Balancing) - outside lock
	serverID, err := picker()
	if err != nil {
		// Failed to pick, revert reservation
		atomic.AddInt64(&p.stats.TotalConnections, -1)
		// Signal? Maybe someone else can use it (unlikely if we failed to pick, but safe)
		// Actually if pick failed, we just decrement.
		p.globalCond.Signal()
		return nil, err
	}

	connID := fmt.Sprintf("conn-%s-%s-%d", p.clientID, serverID, time.Now().UnixNano())
	return NewConnection(connID, serverID, p.maxRequestsPerConn, p.transport), nil
}

// Create creates a new connection directly
func (p *Pool) Create(ctx context.Context, serverID string) (types.Connection, error) {
	if p.closed {
		return nil, fmt.Errorf("pool is closed")
	}
	connID := fmt.Sprintf("conn-%s-%s-%d", p.clientID, serverID, time.Now().UnixNano())
	return NewConnection(connID, serverID, p.maxRequestsPerConn, p.transport), nil
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

	p.mu.Lock()
	defer p.mu.Unlock()
	
	if p.closed {
		return c.Close()
	}

	if c.ShouldClose() {
		// Connection should effectively be removed from pool counting
		atomic.AddInt64(&p.stats.TotalConnections, -1)
		
		// Signal potential waiter that a slot might be free
		p.globalCond.Signal()

		return c.Close()
	}

	// Return to pool list
	p.idleConnections = append(p.idleConnections, c)
	
	// Signal waiters:
	// 1. Waiters blocked on Global Limit might need to wake up to evict this new idle connection if they need a slot.
	// 2. Waiters potentially cycling (though we don't have explicit server-waiters anymore).
	// Since Get scans idle list, we should wake them up.
	p.globalCond.Broadcast()

	return nil
}

// Close closes all connections in the pool
func (p *Pool) Close() error {
	p.mu.Lock()
	p.closed = true
	conns := p.idleConnections
	p.idleConnections = nil // Clear
	p.mu.Unlock()

	for _, conn := range conns {
		conn.Close()
	}
	
	p.globalCond.Broadcast()
	return nil
}

// evictOneIdleConnection tries to close one idle connection
// Returns true if a connection was evicted, false otherwise.
func (p *Pool) evictOneIdleConnection() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	
	if len(p.idleConnections) > 0 {
		conn := p.idleConnections[0]
		p.idleConnections = p.idleConnections[1:]
		
		// Remove from stats? 
		// Caller of evict (Get) usually expects slot to be freed.
		// "evict" implies removing from pool to make space.
		// So yes, decrement total.
		atomic.AddInt64(&p.stats.TotalConnections, -1)
		
		// Unlock to close (avoid holding lock) - wait, we deferred unlock.
		// Can't close inside lock efficiently if close is slow.
		// But for now keeping it simple.
		conn.Close()
		
		p.globalCond.Signal()
		return true
	}
	return false
}

// Stats returns pool statistics
func (p *Pool) Stats() types.ConnectionPoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	total := atomic.LoadInt64(&p.stats.TotalConnections)
	idle := int64(len(p.idleConnections))
	active := total - idle
	if active < 0 {
		active = 0
	}

	return types.ConnectionPoolStats{
		TotalConnections:  total,
		ActiveConnections: active,
		IdleConnections:   idle,
		TotalRequests:     atomic.LoadInt64(&p.stats.TotalRequests),
	}
}
