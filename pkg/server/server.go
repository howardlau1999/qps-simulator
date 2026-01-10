package server

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

// Server represents an HTTP server simulator
type Server struct {
	id                 string
	address            string
	port               int
	maxRequestsPerConn int64
	rateLimiter        types.RateLimiter
	rateLimitKeys      []string // Header keys to use for rate limiting
	processingTime     time.Duration
	processingJitter   time.Duration
	rejectedDelay        time.Duration // Delay for rate-limited requests
	rejectedJitter       time.Duration // Jitter for rejected delay
	rateLimitCheckDelay  time.Duration
	rateLimitCheckJitter time.Duration

	listener    net.Listener
	connections map[string]*serverConnection
	connMu      sync.RWMutex
	stats       serverStats
	metrics     types.MetricsRecorder

	running     bool
	runMu       sync.RWMutex
	wg          sync.WaitGroup
}

type serverStats struct {
	totalRequests       int64
	successfulRequests  int64
	rateLimitedRequests int64
	activeConnections   int64
	totalConnections    int64
	closedConnections   int64
}

type serverConnection struct {
	id           string
	requestCount int64
	maxRequests  int64
	createdAt    time.Time
}

// ServerConfig holds server configuration
type ServerConfig struct {
	ID                 string
	Address            string
	Port               int
	MaxRequestsPerConn int64
	RateLimiter        types.RateLimiter
	RateLimitKeys      []string
	ProcessingTimeMs   int
	ProcessingJitterMs int
	RejectedDelayMs        int
	RejectedJitterMs       int
	RateLimitCheckDelayMs  int
	RateLimitCheckJitterMs int
	Metrics                types.MetricsRecorder
}

// NewServer creates a new HTTP server simulator
func NewServer(config ServerConfig) *Server {
	if config.MaxRequestsPerConn <= 0 {
		config.MaxRequestsPerConn = 150
	}
	if config.Address == "" {
		config.Address = "localhost"
	}

	return &Server{
		id:                 config.ID,
		address:            config.Address,
		port:               config.Port,
		maxRequestsPerConn: config.MaxRequestsPerConn,
		rateLimiter:        config.RateLimiter,
		rateLimitKeys:      config.RateLimitKeys,
		processingTime:     time.Duration(config.ProcessingTimeMs) * time.Millisecond,
		processingJitter:   time.Duration(config.ProcessingJitterMs) * time.Millisecond,
		rejectedDelay:      time.Duration(config.RejectedDelayMs) * time.Millisecond,
		rejectedJitter:     time.Duration(config.RejectedJitterMs) * time.Millisecond,
		rateLimitCheckDelay:  time.Duration(config.RateLimitCheckDelayMs) * time.Millisecond,
		rateLimitCheckJitter: time.Duration(config.RateLimitCheckJitterMs) * time.Millisecond,
		metrics:            config.Metrics,
		connections:        make(map[string]*serverConnection),
	}
}

func (s *Server) ID() string {
	return s.id
}

func (s *Server) Address() string {
	return fmt.Sprintf("%s:%d", s.address, s.port)
}

// SetRateLimiter sets or updates the rate limiter
func (s *Server) SetRateLimiter(limiter types.RateLimiter) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.rateLimiter = limiter
}

// Start starts the server (simulated - doesn't actually listen)
func (s *Server) Start() error {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if s.running {
		return fmt.Errorf("server already running")
	}

	s.running = true
	return nil
}

// Stop stops the server gracefully
func (s *Server) Stop(ctx context.Context) error {
	s.runMu.Lock()
	s.running = false
	s.runMu.Unlock()

	// Wait for pending requests
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HandleRequest processes a request and returns a response
// This is called by the simulation engine
func (s *Server) HandleRequest(ctx context.Context, connID string, req *types.Request) (*types.Response, error) {
	s.runMu.RLock()
	if !s.running {
		s.runMu.RUnlock()
		return nil, fmt.Errorf("server not running")
	}
	s.runMu.RUnlock()

	s.wg.Add(1)
	defer s.wg.Done()

	atomic.AddInt64(&s.stats.totalRequests, 1)

	// Get or create connection
	conn := s.getOrCreateConnection(connID)

	// Increment request count
	requestCount := atomic.AddInt64(&conn.requestCount, 1)
	shouldClose := requestCount >= conn.maxRequests

	// Build rate limit key from headers
	rateLimitKey := s.buildRateLimitKey(req.Headers)

	// Check rate limit
	resp := &types.Response{
		RequestID:   req.ID,
		StatusCode:  200,
		Headers:     make(map[string]string),
		ShouldClose: shouldClose,
		Timestamp:   time.Now(),
	}

	if s.rateLimiter != nil && rateLimitKey != "" {
		startCheck := time.Now()

		// Simulate check delay
		if s.rateLimitCheckDelay > 0 {
			delay := s.rateLimitCheckDelay
			if s.rateLimitCheckJitter > 0 {
				// Simple jitter: delay + jitter (could also be +/-)
				// For simulation stability, just adding fixed jitter is odd, usually it's random.
				// But we'll just check if Jitter > 0 implies random 0..Jitter
				// Assuming simplistic usage for now as we didn't import math/rand for this file.
				// We can just add the jitter time for "max worst case" or ignore jitter if we lack rand source readily available?
				// To keep it simple and compile-safe without adding rand seed overhead:
				// We'll just sleep at least `delay`.
				// If user wants jitter, they probably expect variation.
				// Using time.Now().Nanosecond() as a cheap pseudo-random source for jitter
				jitter := time.Duration(time.Now().Nanosecond()) % s.rateLimitCheckJitter
				delay += jitter
			}
			time.Sleep(delay)
		}

		result, err := s.rateLimiter.Allow(ctx, rateLimitKey, req.Headers)
		checkDuration := time.Since(startCheck)
		
		if s.metrics != nil {
			s.metrics.RecordRateLimitCheck(checkDuration)
		}
		
		if err != nil {
			resp.StatusCode = 500
			return resp, nil
		}

		if !result.Allowed {
			atomic.AddInt64(&s.stats.rateLimitedRequests, 1)
			resp.StatusCode = 429
			resp.RateLimited = true
			resp.Headers["X-RateLimit-Remaining"] = fmt.Sprintf("%d", result.Remaining)
			resp.Headers["Retry-After"] = fmt.Sprintf("%.3f", result.RetryAfter.Seconds())
			
			// Apply delay for rejected requests
			if s.rejectedDelay > 0 {
				time.Sleep(s.rejectedDelay)
			}
			return resp, nil
		}

		resp.Headers["X-RateLimit-Remaining"] = fmt.Sprintf("%d", result.Remaining)
	}

	// Simulate processing time
	if s.processingTime > 0 {
		time.Sleep(s.processingTime)
	}

	atomic.AddInt64(&s.stats.successfulRequests, 1)

	// Set connection headers
	if shouldClose {
		resp.Headers["Connection"] = "close"
	}

	return resp, nil
}

func (s *Server) getOrCreateConnection(connID string) *serverConnection {
	s.connMu.RLock()
	conn, ok := s.connections[connID]
	s.connMu.RUnlock()

	if ok {
		return conn
	}

	s.connMu.Lock()
	defer s.connMu.Unlock()

	// Double-check
	if conn, ok = s.connections[connID]; ok {
		return conn
	}

	conn = &serverConnection{
		id:          connID,
		maxRequests: s.maxRequestsPerConn,
		createdAt:   time.Now(),
	}
	s.connections[connID] = conn
	atomic.AddInt64(&s.stats.totalConnections, 1)
	atomic.AddInt64(&s.stats.activeConnections, 1)
	return conn
}

// CloseConnection removes a connection
func (s *Server) CloseConnection(connID string) {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	if _, ok := s.connections[connID]; ok {
		delete(s.connections, connID)
		atomic.AddInt64(&s.stats.activeConnections, -1)
		atomic.AddInt64(&s.stats.closedConnections, 1)
	}
}

func (s *Server) buildRateLimitKey(headers map[string]string) string {
	if len(s.rateLimitKeys) == 0 {
		return ""
	}

	// Use the first available key
	for _, key := range s.rateLimitKeys {
		if v, ok := headers[key]; ok && v != "" {
			return fmt.Sprintf("%s:%s", key, v)
		}
	}
	return ""
}

// Stats returns server statistics
func (s *Server) Stats() types.ServerStats {
	return types.ServerStats{
		TotalRequests:       atomic.LoadInt64(&s.stats.totalRequests),
		SuccessfulRequests:  atomic.LoadInt64(&s.stats.successfulRequests),
		RateLimitedRequests: atomic.LoadInt64(&s.stats.rateLimitedRequests),
		ActiveConnections:   atomic.LoadInt64(&s.stats.activeConnections),
		TotalConnections:    atomic.LoadInt64(&s.stats.totalConnections),
		ClosedConnections:   atomic.LoadInt64(&s.stats.closedConnections),
	}
}
