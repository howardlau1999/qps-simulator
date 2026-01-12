package simulator

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/howardlau1999/qps-simulator/pkg/client"
	"github.com/howardlau1999/qps-simulator/pkg/config"
	"github.com/howardlau1999/qps-simulator/pkg/loadbalancer"
	"github.com/howardlau1999/qps-simulator/pkg/metrics"
	"github.com/howardlau1999/qps-simulator/pkg/ratelimiter"
	"github.com/howardlau1999/qps-simulator/pkg/server"
	"github.com/howardlau1999/qps-simulator/pkg/types"
)

// Simulator orchestrates the QPS simulation
type Simulator struct {
	config        *config.Config
	clients       []*client.Client
	servers       []*server.Server
	loadBalancer  types.LoadBalancer
	metrics       *metrics.Collector
	metricsWriter *metrics.FileWriter

	running bool
	runMu   sync.RWMutex
	stopCh  chan struct{}

	requestCount int64
}

// New creates a new simulator from configuration
func New(cfg *config.Config) (*Simulator, error) {
	// Create load balancer
	lb, err := loadbalancer.New(cfg.LoadBalancer.Algorithm)
	if err != nil {
		return nil, fmt.Errorf("failed to create load balancer: %w", err)
	}

	// Create rate limiter
	limiterConfig := types.RateLimiterConfig{
		Type:   cfg.RateLimiter.Type,
		Rate:   cfg.RateLimiter.Rate,
		Burst:  cfg.RateLimiter.Burst,
		Window: cfg.RateLimiter.GetWindowDuration(),
		Shards: cfg.RateLimiter.Shards,
	}

	var rateLimiter types.RateLimiter
	var remoteTokenServer *ratelimiter.RemoteTokenServer

	if cfg.RateLimiter.Type == "local_cached" {
		remoteTokenServer = ratelimiter.NewRemoteTokenServer(cfg.RateLimiter.Rate, cfg.RateLimiter.Burst)
	} else {
		var err error
		rateLimiter, err = ratelimiter.Create(limiterConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create rate limiter: %w", err)
		}
	}

	// Create metrics collector
	metricsCollector := metrics.NewCollector(256)

	// Create servers
	servers := make([]*server.Server, cfg.Servers.Count)
	for i := 0; i < cfg.Servers.Count; i++ {
		serverConfig := server.ServerConfig{
			ID:                     fmt.Sprintf("server-%d", i),
			Address:                "localhost",
			Port:                   cfg.Servers.BasePort + i,
			MaxRequestsPerConn:     int64(cfg.Servers.MaxRequestsPerConn),
			RateLimitKeys:          cfg.Servers.RateLimitHeaderKeys,
			ProcessingTimeMs:       cfg.Servers.ProcessingTimeMs,
			ProcessingJitterMs:     cfg.Servers.ProcessingTimeJitter,
			RejectedDelayMs:        cfg.Servers.RejectedDelayMs,
			RejectedJitterMs:       cfg.Servers.RejectedDelayJitter,
			RateLimitCheckDelayMs:  cfg.Servers.RateLimitCheckDelayMs,
			RateLimitCheckJitterMs: cfg.Servers.RateLimitCheckJitterMs,
			Metrics:                metricsCollector,
		}

		if cfg.RateLimiter.Type == "local_cached" {
			serverConfig.RateLimiter = ratelimiter.NewLocalCachedLimiter(
				limiterConfig,
				remoteTokenServer,
				cfg.RateLimiter.PrefetchCount,
				cfg.Servers.RemoteCheckDelayMs,
				cfg.Servers.RemoteCheckJitterMs,
				metricsCollector,
			)
		} else {
			serverConfig.RateLimiter = rateLimiter
		}

		s := server.NewServer(serverConfig)
		servers[i] = s

		// Register with load balancer
		lb.RegisterServer(&types.ServerInfo{
			ID:      serverConfig.ID,
			Address: serverConfig.Address,
			Port:    serverConfig.Port,
			Weight:  1,
			Healthy: true,
		})
	}

	// Determine connection mode
	var connMode types.ConnectionMode
	switch cfg.Clients.ConnectionMode {
	case "reuse":
		connMode = types.ConnectionModeReuse
	case "per_request":
		connMode = types.ConnectionModePerRequest
	case "hybrid":
		connMode = types.ConnectionModeHybrid
	case "dedicated":
		connMode = types.ConnectionModeDedicated
	default:
		connMode = types.ConnectionModeReuse
	}

	// Create transport
	transport := &simulatorTransport{servers: servers}

	// Create clients
	clients := make([]*client.Client, cfg.Clients.Count)
	for i := 0; i < cfg.Clients.Count; i++ {
		clientConfig := client.ClientConfig{
			ID:                     fmt.Sprintf("client-%d", i),
			LoadBalancer:           lb,
			ConnectionMode:         connMode,
			ConnectionPoolStrategy: cfg.Clients.ConnectionPoolStrategy,
			MaxConnsPerServer:      cfg.LoadBalancer.MaxConnPerServer / cfg.Clients.Count,
			MaxRequestsPerConn:     int64(cfg.Servers.MaxRequestsPerConn),
			Headers:                cfg.Clients.Headers,
			Transport:              transport,
			MaxConcurrency:         1, // Force low concurrency for testing skew? No, use config.
		}

		if cfg.Clients.MaxConnections > 0 {
			// This is now passed as the global limit, not per-server
			clientConfig.MaxConnections = cfg.Clients.MaxConnections
		} else {
			// Default behavior: no global limit? Or derive?
			// Since we want to support existing behavior where everything was limited per server,
			// but if user didn't specify MaxConnections we assume they want per-server limits as configured in load balancer.
			// But if we want NO global limit by default, we just pass 0.
		}
		// Oh, wait, I shouldn't override MaxConcurrency here.
		clientConfig.MaxConcurrency = cfg.Clients.MaxConcurrency
		clients[i] = client.NewClient(clientConfig)
	}

	// Create metrics writer if output path is configured
	var metricsWriter *metrics.FileWriter
	if cfg.Metrics.OutputPath != "" {
		reportConfig := metrics.ReportConfig{
			ClientCount:        cfg.Clients.Count,
			ServerCount:        cfg.Servers.Count,
			ConnectionMode:     cfg.Clients.ConnectionMode,
			RateLimiterType:    cfg.RateLimiter.Type,
			RateLimitRate:      cfg.RateLimiter.Rate,
			MaxRequestsPerConn: cfg.Servers.MaxRequestsPerConn,
			DurationSeconds:    cfg.Simulation.DurationSeconds,
		}
		metricsWriter = metrics.NewFileWriter(cfg.Metrics.OutputPath, reportConfig)
	}

	return &Simulator{
		config:        cfg,
		clients:       clients,
		servers:       servers,
		loadBalancer:  lb,
		metrics:       metricsCollector,
		metricsWriter: metricsWriter,
		stopCh:        make(chan struct{}),
	}, nil
}

// Run starts the simulation
func (s *Simulator) Run(ctx context.Context) error {
	s.runMu.Lock()
	if s.running {
		s.runMu.Unlock()
		return fmt.Errorf("simulator already running")
	}
	s.running = true
	s.runMu.Unlock()

	// Start servers
	for _, srv := range s.servers {
		if err := srv.Start(); err != nil {
			return fmt.Errorf("failed to start server %s: %w", srv.ID(), err)
		}
	}

	// Create simulation context with timeout
	duration := time.Duration(s.config.Simulation.DurationSeconds) * time.Second
	simCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	// Warmup period
	warmupDuration := time.Duration(s.config.Simulation.WarmupSeconds) * time.Second
	if warmupDuration > 0 {
		fmt.Printf("Warming up for %v...\n", warmupDuration)
		time.Sleep(warmupDuration)
	}

	// Reset metrics after warmup
	s.metrics.Reset()

	fmt.Printf("Starting simulation for %v with %d clients and %d servers\n",
		duration, len(s.clients), len(s.servers))

	// Start metrics reporter
	reporterDone := make(chan struct{})
	go s.reportMetrics(simCtx, reporterDone)

	// Start client workers
	var wg sync.WaitGroup
	for _, c := range s.clients {
		wg.Add(1)
		go s.runClient(simCtx, c, &wg)
	}

	// Wait for completion
	wg.Wait()
	close(reporterDone)

	// Final metrics
	// Use CaptureFinalSnapshot to get the very last partial bucket if any
	finalSnapshot := s.metrics.CaptureFinalSnapshot()
	finalSnapshot.ActiveConnections, finalSnapshot.TotalConnections, finalSnapshot.ClosedConnections = s.getConnectionStats()
	fmt.Printf("\n=== Final Results ===\n%s\n", finalSnapshot.String())

	// Save metrics to file if configured
	if s.metricsWriter != nil {
		// Add the final snapshot to the list so the graph includes the tail
		s.metricsWriter.AddSnapshot(*finalSnapshot)

		s.metricsWriter.SetFinalResult(*finalSnapshot)
		if err := s.metricsWriter.Save(); err != nil {
			fmt.Printf("Warning: failed to save metrics file: %v\n", err)
		} else {
			fmt.Printf("Metrics saved to: %s\n", s.config.Metrics.OutputPath)
		}
	}

	return nil
}

func (s *Simulator) runClient(ctx context.Context, c *client.Client, wg *sync.WaitGroup) {
	defer wg.Done()

	requestsPerClient := int64(s.config.Clients.RequestsPerClient)
	testTimeSeconds := s.config.Clients.TestTimeSeconds
	targetRate := s.config.Clients.RequestRate
	maxConcurrency := s.config.Clients.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}

	// Create rate limiter if needed
	var rl types.RateLimiter
	if targetRate > 0 {
		// Use token bucket for smooth client-side rate limiting
		// Burst equal to rate implies we allow 1s worth of requests to burst, which is reasonable
		// or we can make it smaller. Using Rate as Burst is a safe default for "Target Rate".
		rlConfig := types.RateLimiterConfig{
			Rate:  int64(targetRate),
			Burst: int64(targetRate),
			Type:  "token_bucket",
		}
		var err error
		rl, err = ratelimiter.NewTokenBucket(rlConfig)
		if err != nil {
			fmt.Printf("Error creating rate limiter for client %s: %v\n", c.ID(), err)
			return
		}
	}

	// Determine termination conditions
	useTimeBased := requestsPerClient <= 0 && testTimeSeconds > 0
	var clientDeadline time.Time
	if useTimeBased {
		clientDeadline = time.Now().Add(time.Duration(testTimeSeconds) * time.Second)
	}

	var requestCounter int64 // Atomic counter for requests executed by this client's workers

	// Worker coordination
	var workerWg sync.WaitGroup
	workerWg.Add(maxConcurrency)

	for i := 0; i < maxConcurrency; i++ {
		go func(workerID int) {
			defer workerWg.Done()

			// Worker-local connection for dedicated mode
			var dedicatedConn types.Connection
			
			// Pre-create dedicated connection at startup (only in dedicated mode)
			if c.ConnectionMode() == types.ConnectionModeDedicated {
				conn, err := c.CreateDedicatedConnection(ctx)
				if err != nil {
					fmt.Printf("Worker %d failed to pre-create connection: %v\n", workerID, err)
				} else {
					dedicatedConn = conn
				}
			}
			
			// Cleanup on exit
			defer func() {
				if dedicatedConn != nil {
					dedicatedConn.Close()
				}
			}()

			for {
				// Check termination conditions
				if useTimeBased {
					if time.Now().After(clientDeadline) {
						return
					}
				} else if requestsPerClient > 0 {
					// Atomically check and increment
					currentReq := atomic.AddInt64(&requestCounter, 1)
					if currentReq > requestsPerClient {
						return
					}
				}

				select {
				case <-ctx.Done():
					return
				default:
				}

				// Rate limiting
				if rl != nil {
					for {
						// Blocking wait for token
						res, _ := rl.Allow(ctx, "client", nil)
						if res.Allowed {
							break
						}

						wait := res.RetryAfter
						if wait <= 0 {
							wait = time.Millisecond
						}

						select {
						case <-time.After(wait):
							// Retry loop
						case <-ctx.Done():
							return
						}
					}
				}

				reqID := fmt.Sprintf("req-%s-%d", c.ID(), atomic.AddInt64(&s.requestCount, 1))
				req := &types.Request{
					ID:        reqID,
					ClientID:  c.ID(),
					Headers:   make(map[string]string),
					Timestamp: time.Now(),
				}

				// Add user ID header for rate limiting (can be overridden by config headers)
				req.Headers["X-User-ID"] = c.ID()

				// Copy config headers (allows override of X-User-ID for global rate limiting)
				for k, v := range s.config.Clients.Headers {
					req.Headers[k] = v
				}

				// Build context with worker info and dedicated connection
				ctxWithWorker := context.WithValue(ctx, client.WorkerIDKey, workerID)
				if dedicatedConn != nil {
					ctxWithWorker = context.WithValue(ctxWithWorker, client.DedicatedConnKey, dedicatedConn)
				}

				start := time.Now()
				resp, err := c.Send(ctxWithWorker, req)
				latency := time.Since(start)

				if err != nil {
					s.metrics.RecordFailedRequest()
					// If connection failed, try to recreate for dedicated mode
					if c.ConnectionMode() == types.ConnectionModeDedicated {
						newConn, err := c.CreateDedicatedConnection(ctx)
						if err == nil {
							dedicatedConn = newConn
						}
					}
					continue
				}

				s.metrics.RecordRequest(c.ID(), "", latency, resp.RateLimited)
				
				// Handle connection replacement for dedicated mode
				if resp.ShouldClose && c.ConnectionMode() == types.ConnectionModeDedicated {
					// Connection was closed, create replacement
					newConn, err := c.CreateDedicatedConnection(ctx)
					if err == nil {
						dedicatedConn = newConn
					} else {
						dedicatedConn = nil
					}
				}
			}
		}(i)
	}

	workerWg.Wait()
}

func (s *Simulator) reportMetrics(ctx context.Context, done <-chan struct{}) {
	consoleInterval := time.Duration(s.config.Metrics.IntervalSeconds) * time.Second
	if consoleInterval <= 0 {
		consoleInterval = time.Second
	}

	// File recording is fixed at 1s interval as per requirement
	fileInterval := time.Second

	// Use a faster tick to check wall clock more frequently
	// This ensures we don't miss the interval boundary
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	// Initialize to past time so first tick triggers report
	lastConsoleReport := time.Now().Add(-consoleInterval)
	lastFileRecord := time.Now().Add(-fileInterval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			// Use wall-clock time for accurate interval checking
			now := time.Now()

			// Check if it's time to report (using wall clock, not ticker time)
			shouldReportConsole := now.Sub(lastConsoleReport) >= consoleInterval
			shouldRecordFile := s.metricsWriter != nil && now.Sub(lastFileRecord) >= fileInterval

			if !shouldReportConsole && !shouldRecordFile {
				continue // Skip expensive Snapshot() if not needed
			}

			snapshot := s.metrics.Snapshot()
			// Add server connection counts
			snapshot.ActiveConnections, snapshot.TotalConnections, snapshot.ClosedConnections = s.getConnectionStats()

			// Record to file every 1s
			if shouldRecordFile {
				s.metricsWriter.AddSnapshot(*snapshot)
				lastFileRecord = now
			}

			// Report to console at configured interval
			if shouldReportConsole {
				fmt.Printf("[%v] %s\n", snapshot.Duration.Round(time.Second), snapshot.String())
				lastConsoleReport = now
			}
		}
	}
}

// getConnectionStats returns connection statistics across all servers
func (s *Simulator) getConnectionStats() (active, total, closed int64) {
	for _, srv := range s.servers {
		stats := srv.Stats()
		active += stats.ActiveConnections
		total += stats.TotalConnections
		closed += stats.ClosedConnections
	}
	return
}

// Stop stops the simulation
func (s *Simulator) Stop() error {
	s.runMu.Lock()
	if !s.running {
		s.runMu.Unlock()
		return nil
	}
	s.running = false
	s.runMu.Unlock()

	close(s.stopCh)

	// Stop servers
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, srv := range s.servers {
		srv.Stop(ctx)
	}

	// Close clients
	for _, c := range s.clients {
		c.Close()
	}

	return nil
}

// GetMetrics returns the current metrics snapshot
func (s *Simulator) GetMetrics() *metrics.MetricsSnapshot {
	return s.metrics.Snapshot()
}

// simulatorTransport implements client.Transport
type simulatorTransport struct {
	servers []*server.Server
}

func (t *simulatorTransport) Send(ctx context.Context, serverID, connID string, req *types.Request) (*types.Response, error) {
	// Find the target server
	var targetServer *server.Server
	for _, srv := range t.servers {
		if srv.ID() == serverID {
			targetServer = srv
			break
		}
	}

	if targetServer == nil {
		return nil, fmt.Errorf("server not found: %s", serverID)
	}

	// Process request through server
	resp, err := targetServer.HandleRequest(ctx, connID, req)
	if err != nil {
		return resp, err
	}

	// Close connection if server instructs (max requests reached)
	if resp.ShouldClose {
		targetServer.CloseConnection(connID)
	}

	return resp, nil
}

func (t *simulatorTransport) CloseConnection(serverID, connID string) {
	for _, srv := range t.servers {
		if srv.ID() == serverID {
			srv.CloseConnection(connID)
			return
		}
	}
}
