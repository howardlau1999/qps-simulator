package client

import (
	"context"
	"testing"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

// Mock Transport
type MockTransport struct{}

func (m *MockTransport) Send(ctx context.Context, serverID, connID string, req *types.Request) (*types.Response, error) {
	return &types.Response{}, nil
}

func (m *MockTransport) CloseConnection(serverID, connID string) {}


// Mock LoadBalancer
type MockLoadBalancer struct {
	Server *types.ServerInfo
}

func (m *MockLoadBalancer) PickServer() (*types.ServerInfo, error) {
	return m.Server, nil
}
func (m *MockLoadBalancer) RegisterServer(server *types.ServerInfo) error { return nil }
func (m *MockLoadBalancer) RemoveServer(serverID string) error { return nil }
func (m *MockLoadBalancer) MarkUnhealthy(serverID string) {}
func (m *MockLoadBalancer) MarkHealthy(serverID string) {}
func (m *MockLoadBalancer) Servers() []*types.ServerInfo { return []*types.ServerInfo{m.Server} }

func TestPool_LIFO_Strategy(t *testing.T) {
	server := &types.ServerInfo{ID: "server-1"}
	lb := &MockLoadBalancer{Server: server}
	// NewPool(clientID, maxRequestsPerConn, transport, strategy, maxTotalConnections)
	pool := NewPool("client-1", 10, &MockTransport{}, "lifo", 100)
	ctx := context.Background()

	picker := func() (string, error) {
		s, err := lb.PickServer()
		if err != nil {
			return "", err
		}
		return s.ID, nil
	}

	// Get two connections
	conn1, err := pool.Get(ctx, picker)
	if err != nil {
		t.Fatalf("Failed to get conn1: %v", err)
	}
	// conn1ID := conn1.ID()


	conn2, err := pool.Get(ctx, picker)
	if err != nil {
		t.Fatalf("Failed to get conn2: %v", err)
	}
	conn2ID := conn2.ID()

	// Return them to pool
	// Order: Return 1, then 2.
	// Pool should have [conn1, conn2] (appended)
	pool.Put(conn1)
	pool.Put(conn2)

	// LIFO: Should get conn2 (last put)
	conn3, err := pool.Get(ctx, picker)
	if err != nil {
		t.Fatalf("Failed to get conn3: %v", err)
	}

	if conn3.ID() != conn2ID {
		t.Errorf("Expected conn2 (LIFO), got %s", conn3.ID())
	}

	// Put it back
	pool.Put(conn3)

	// Get again -> Conn2 again
	conn4, err := pool.Get(ctx, picker)
	if err != nil {
		t.Fatalf("Failed to get conn4: %v", err)
	}
	if conn4.ID() != conn2ID {
		t.Errorf("Expected conn2 again, got %s", conn4.ID())
	}
}

func TestPool_FIFO_Strategy(t *testing.T) {
	server := &types.ServerInfo{ID: "server-1"}
	lb := &MockLoadBalancer{Server: server}
	pool := NewPool("client-1", 10, &MockTransport{}, "fifo", 100)
	ctx := context.Background()

	picker := func() (string, error) {
		s, err := lb.PickServer()
		if err != nil {
			return "", err
		}
		return s.ID, nil
	}

	// Get two connections
	conn1, _ := pool.Get(ctx, picker)
	conn2, _ := pool.Get(ctx, picker)

	// Return them
	pool.Put(conn1) // Pool: [conn1]
	pool.Put(conn2) // Pool: [conn1, conn2]

	// FIFO: Should get conn1 (first put)
	conn3, _ := pool.Get(ctx, picker)
	if conn3.ID() != conn1.ID() {
		t.Errorf("Expected conn1 (FIFO), got %s", conn3.ID())
	}
	
	conn4, _ := pool.Get(ctx, picker)
	if conn4.ID() != conn2.ID() {
		t.Errorf("Expected conn2 (FIFO), got %s", conn4.ID())
	}
}
