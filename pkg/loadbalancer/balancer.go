package loadbalancer

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

// BaseBalancer provides common functionality for load balancers
type BaseBalancer struct {
	servers []*types.ServerInfo
	mu      sync.RWMutex
}

// RegisterServer adds a server to the pool
func (b *BaseBalancer) RegisterServer(server *types.ServerInfo) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, s := range b.servers {
		if s.ID == server.ID {
			return fmt.Errorf("server %s already registered", server.ID)
		}
	}

	server.Healthy = true
	b.servers = append(b.servers, server)
	return nil
}

// RemoveServer removes a server from the pool
func (b *BaseBalancer) RemoveServer(serverID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for i, s := range b.servers {
		if s.ID == serverID {
			b.servers = append(b.servers[:i], b.servers[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("server %s not found", serverID)
}

// MarkUnhealthy marks a server as unhealthy
func (b *BaseBalancer) MarkUnhealthy(serverID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, s := range b.servers {
		if s.ID == serverID {
			s.Healthy = false
			return
		}
	}
}

// MarkHealthy marks a server as healthy
func (b *BaseBalancer) MarkHealthy(serverID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, s := range b.servers {
		if s.ID == serverID {
			s.Healthy = true
			return
		}
	}
}

// Servers returns all registered servers
func (b *BaseBalancer) Servers() []*types.ServerInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]*types.ServerInfo, len(b.servers))
	copy(result, b.servers)
	return result
}

// getHealthyServers returns only healthy servers
func (b *BaseBalancer) getHealthyServers() []*types.ServerInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var healthy []*types.ServerInfo
	for _, s := range b.servers {
		if s.Healthy {
			healthy = append(healthy, s)
		}
	}
	return healthy
}

// ============================================================================
// Round Robin Load Balancer
// ============================================================================

// RoundRobin implements round-robin load balancing
type RoundRobin struct {
	BaseBalancer
	current uint64
}

// NewRoundRobin creates a new round-robin load balancer
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{}
}

// PickServer selects the next server in round-robin order
func (r *RoundRobin) PickServer() (*types.ServerInfo, error) {
	servers := r.getHealthyServers()
	if len(servers) == 0 {
		return nil, fmt.Errorf("no healthy servers available")
	}

	// Atomic increment for lock-free round robin
	idx := atomic.AddUint64(&r.current, 1) - 1
	return servers[idx%uint64(len(servers))], nil
}

// ============================================================================
// Random Load Balancer
// ============================================================================

// Random implements random load balancing
type Random struct {
	BaseBalancer
}

// NewRandom creates a new random load balancer
func NewRandom() *Random {
	return &Random{}
}

// PickServer selects a random healthy server
func (r *Random) PickServer() (*types.ServerInfo, error) {
	servers := r.getHealthyServers()
	if len(servers) == 0 {
		return nil, fmt.Errorf("no healthy servers available")
	}

	return servers[rand.Intn(len(servers))], nil
}

// ============================================================================
// Weighted Load Balancer
// ============================================================================

// Weighted implements weighted load balancing
type Weighted struct {
	BaseBalancer
	current uint64
}

// NewWeighted creates a new weighted load balancer
func NewWeighted() *Weighted {
	return &Weighted{}
}

// PickServer selects a server based on weights
func (w *Weighted) PickServer() (*types.ServerInfo, error) {
	servers := w.getHealthyServers()
	if len(servers) == 0 {
		return nil, fmt.Errorf("no healthy servers available")
	}

	// Calculate total weight
	totalWeight := 0
	for _, s := range servers {
		weight := s.Weight
		if weight <= 0 {
			weight = 1
		}
		totalWeight += weight
	}

	// Pick based on weight
	idx := atomic.AddUint64(&w.current, 1) - 1
	pick := int(idx % uint64(totalWeight))

	for _, s := range servers {
		weight := s.Weight
		if weight <= 0 {
			weight = 1
		}
		pick -= weight
		if pick < 0 {
			return s, nil
		}
	}

	return servers[0], nil
}

// ============================================================================
// Least Connections Load Balancer
// ============================================================================

// LeastConnections implements least-connections load balancing
type LeastConnections struct {
	BaseBalancer
	connections map[string]*int64
	connMu      sync.RWMutex
}

// NewLeastConnections creates a new least-connections load balancer
func NewLeastConnections() *LeastConnections {
	return &LeastConnections{
		connections: make(map[string]*int64),
	}
}

// PickServer selects the server with the fewest connections
func (l *LeastConnections) PickServer() (*types.ServerInfo, error) {
	servers := l.getHealthyServers()
	if len(servers) == 0 {
		return nil, fmt.Errorf("no healthy servers available")
	}

	l.connMu.RLock()
	defer l.connMu.RUnlock()

	var best *types.ServerInfo
	var bestCount int64 = -1

	for _, s := range servers {
		count := int64(0)
		if c, ok := l.connections[s.ID]; ok {
			count = atomic.LoadInt64(c)
		}

		if bestCount < 0 || count < bestCount {
			best = s
			bestCount = count
		}
	}

	return best, nil
}

// IncrementConnections increments the connection count for a server
func (l *LeastConnections) IncrementConnections(serverID string) {
	l.connMu.Lock()
	if _, ok := l.connections[serverID]; !ok {
		var count int64
		l.connections[serverID] = &count
	}
	l.connMu.Unlock()

	l.connMu.RLock()
	atomic.AddInt64(l.connections[serverID], 1)
	l.connMu.RUnlock()
}

// DecrementConnections decrements the connection count for a server
func (l *LeastConnections) DecrementConnections(serverID string) {
	l.connMu.RLock()
	if c, ok := l.connections[serverID]; ok {
		atomic.AddInt64(c, -1)
	}
	l.connMu.RUnlock()
}

// ============================================================================
// Factory
// ============================================================================

// New creates a load balancer based on algorithm name
func New(algorithm string) (types.LoadBalancer, error) {
	switch algorithm {
	case "round_robin":
		return NewRoundRobin(), nil
	case "random":
		return NewRandom(), nil
	case "weighted":
		return NewWeighted(), nil
	case "least_conn":
		return NewLeastConnections(), nil
	default:
		return nil, fmt.Errorf("unknown load balancer algorithm: %s", algorithm)
	}
}
