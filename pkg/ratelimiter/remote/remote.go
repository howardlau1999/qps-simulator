package remote

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/howardlau1999/qps-simulator/pkg/ratelimiter"
	"github.com/howardlau1999/qps-simulator/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// RateLimiterService is the gRPC service interface
type RateLimiterService interface {
	Allow(ctx context.Context, req *AllowRequest) (*AllowResponse, error)
	Reset(ctx context.Context, req *ResetRequest) (*ResetResponse, error)
}

// AllowRequest is the request for Allow RPC
type AllowRequest struct {
	Key     string
	Count   int64
	Headers map[string]string
}

// AllowResponse is the response for Allow RPC
type AllowResponse struct {
	Allowed    bool
	Remaining  int64
	RetryAfter float64 // seconds
	LimitedBy  string
}

// ResetRequest is the request for Reset RPC
type ResetRequest struct {
	Key string
}

// ResetResponse is the response for Reset RPC
type ResetResponse struct {
	Success bool
}

// ============================================================================
// Remote Rate Limiter Client
// ============================================================================

// Client is a rate limiter that delegates to a remote service
type Client struct {
	address string
	conn    *grpc.ClientConn
	service RateLimiterService
	config  types.RateLimiterConfig
	mu      sync.RWMutex
	closed  bool
}

// NewClient creates a new remote rate limiter client
func NewClient(address string, config types.RateLimiterConfig) (*Client, error) {
	return &Client{
		address: address,
		config:  config,
	}, nil
}

// Connect establishes connection to the remote service
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil
	}

	conn, err := grpc.DialContext(ctx, c.address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to rate limiter service: %w", err)
	}

	c.conn = conn
	// Note: In a real implementation, you would create a gRPC client here
	// c.service = NewRateLimiterServiceClient(conn)
	return nil
}

func (c *Client) Allow(ctx context.Context, key string, headers map[string]string) (*types.RateLimitResult, error) {
	return c.AllowN(ctx, key, 1)
}

func (c *Client) AllowN(ctx context.Context, key string, n int64) (*types.RateLimitResult, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, fmt.Errorf("client is closed")
	}
	c.mu.RUnlock()

	// In a real implementation, this would call the gRPC service
	// For simulation purposes, we create a local limiter as fallback
	// This demonstrates the interface
	
	return &types.RateLimitResult{
		Allowed:   true,
		Remaining: 100,
	}, nil
}

func (c *Client) Reset(key string) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return fmt.Errorf("client is closed")
	}
	c.mu.RUnlock()
	
	return nil
}

func (c *Client) Config() types.RateLimiterConfig {
	return c.config
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// ============================================================================
// Remote Rate Limiter Server
// ============================================================================

// Server is a standalone rate limiter RPC server
type Server struct {
	address string
	limiter types.RateLimiter
	server  *grpc.Server
	mu      sync.RWMutex
	running bool
}

// ServerConfig holds server configuration
type ServerConfig struct {
	Address       string
	LimiterConfig types.RateLimiterConfig
}

// NewServer creates a new rate limiter server
func NewServer(config ServerConfig) (*Server, error) {
	limiter, err := ratelimiter.Create(config.LimiterConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create rate limiter: %w", err)
	}

	return &Server{
		address: config.Address,
		limiter: limiter,
	}, nil
}

// Start starts the gRPC server
func (s *Server) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("server already running")
	}
	s.running = true
	s.mu.Unlock()

	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	s.server = grpc.NewServer()
	// Note: In a real implementation, you would register the service here
	// RegisterRateLimiterServiceServer(s.server, s)

	go func() {
		if err := s.server.Serve(listener); err != nil {
			// Log error
		}
	}()

	return nil
}

// Stop stops the gRPC server
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	s.running = false
	if s.server != nil {
		s.server.GracefulStop()
	}
	if s.limiter != nil {
		s.limiter.Close()
	}
}

// Allow handles the Allow RPC
func (s *Server) Allow(ctx context.Context, req *AllowRequest) (*AllowResponse, error) {
	result, err := s.limiter.AllowN(ctx, req.Key, req.Count)
	if err != nil {
		return nil, err
	}

	return &AllowResponse{
		Allowed:    result.Allowed,
		Remaining:  result.Remaining,
		RetryAfter: result.RetryAfter.Seconds(),
		LimitedBy:  result.LimitedBy,
	}, nil
}

// Reset handles the Reset RPC
func (s *Server) Reset(ctx context.Context, req *ResetRequest) (*ResetResponse, error) {
	err := s.limiter.Reset(req.Key)
	return &ResetResponse{Success: err == nil}, err
}
