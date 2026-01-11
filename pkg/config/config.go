package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the complete simulation configuration
type Config struct {
	Clients          ClientsConfig          `yaml:"clients"`
	Servers          ServersConfig          `yaml:"servers"`
	LoadBalancer     LoadBalancerConfig     `yaml:"load_balancer"`
	RateLimiter      RateLimiterConfig      `yaml:"rate_limiter"`
	RateLimiterService RateLimiterServiceConfig `yaml:"rate_limiter_service"`
	Simulation       SimulationConfig       `yaml:"simulation"`
	Metrics          MetricsConfig          `yaml:"metrics"`
}

// ClientsConfig configures the client simulator
type ClientsConfig struct {
	Count             int               `yaml:"count"`
	RequestsPerClient int               `yaml:"requests_per_client"` // 0 = unlimited (use test_time_seconds)
	TestTimeSeconds   int               `yaml:"test_time_seconds"`   // Duration to run (alternative to requests_per_client)
	ConnectionMode    string            `yaml:"connection_mode"`     // reuse, per_request, hybrid
	ConnectionPoolStrategy string       `yaml:"connection_pool_strategy"` // fifo (default), lifo
	MaxConcurrency    int               `yaml:"max_concurrency"`     // Max concurrent requests per client
	Headers           map[string]string `yaml:"headers"`
	RequestRate       int               `yaml:"request_rate"` // Target requests per second per client
	MaxConnections    int               `yaml:"max_connections"` // Max total connections per client (global)
}

// ServersConfig configures the server simulator
type ServersConfig struct {
	Count                 int      `yaml:"count"`
	MaxRequestsPerConn    int      `yaml:"max_requests_per_conn"` // Default: 150
	BasePort              int      `yaml:"base_port"`
	ProcessingTimeMs      int      `yaml:"processing_time_ms"` // Simulated processing time
	ProcessingTimeJitter  int      `yaml:"processing_time_jitter_ms"`
	RejectedDelayMs         int      `yaml:"rejected_delay_ms"`        // Delay for rate-limited requests
	RejectedDelayJitter     int      `yaml:"rejected_delay_jitter_ms"` // Jitter for rejected delay
	RateLimitCheckDelayMs   int      `yaml:"rate_limit_check_delay_ms"` // Simulated delay for check
	RateLimitCheckJitterMs  int      `yaml:"rate_limit_check_jitter_ms"`
	RateLimitHeaderKeys     []string `yaml:"rate_limit_header_keys"`   // Headers to use for rate limiting
	RemoteCheckDelayMs      int      `yaml:"remote_check_delay_ms"`    // Latency for remote RL fetch
	RemoteCheckJitterMs     int      `yaml:"remote_check_jitter_ms"`
}

// LoadBalancerConfig configures the load balancer
type LoadBalancerConfig struct {
	Algorithm       string `yaml:"algorithm"` // round_robin, random, weighted, least_conn
	HealthCheckMs   int    `yaml:"health_check_ms"`
	MaxConnPerServer int   `yaml:"max_conn_per_server"`
}

// RateLimiterConfig configures the rate limiter
type RateLimiterConfig struct {
	Type       string        `yaml:"type"` // token_bucket, sliding_window, fixed_window, leaky_bucket
	Rate       int64         `yaml:"rate"` // Requests per second
	Burst      int64         `yaml:"burst"`
	WindowMs   int           `yaml:"window_ms"`
	Shards     int           `yaml:"shards"` // For high-concurrency (1M+ QPS)
	KeyTemplate string       `yaml:"key_template"` // Template for rate limit key from headers
	PrefetchCount int        `yaml:"prefetch_count"` // For local-cached: tokens to fetch
}

// RateLimiterServiceConfig configures remote rate limiter service
type RateLimiterServiceConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Address     string `yaml:"address"`
	TLSEnabled  bool   `yaml:"tls_enabled"`
	TLSCertPath string `yaml:"tls_cert_path"`
	TimeoutMs   int    `yaml:"timeout_ms"`
}

// SimulationConfig configures the simulation run
type SimulationConfig struct {
	DurationSeconds int  `yaml:"duration_seconds"`
	WarmupSeconds   int  `yaml:"warmup_seconds"`
	RampUpSeconds   int  `yaml:"ramp_up_seconds"`
	StopOnError     bool `yaml:"stop_on_error"`
}

// MetricsConfig configures metrics collection
type MetricsConfig struct {
	Enabled          bool   `yaml:"enabled"`
	IntervalSeconds  int    `yaml:"interval_seconds"`
	OutputFormat     string `yaml:"output_format"` // json, csv, prometheus
	OutputPath       string `yaml:"output_path"`
	HistogramBuckets []float64 `yaml:"histogram_buckets"`
}

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Clients: ClientsConfig{
			Count:             10,
			RequestsPerClient: 10000,
			ConnectionMode:    "reuse",
			ConnectionPoolStrategy: "fifo",
			MaxConcurrency:    100,
			RequestRate:       1000,
			Headers:           make(map[string]string),
		},
		Servers: ServersConfig{
			Count:               3,
			MaxRequestsPerConn:  150,
			BasePort:            8080,
			ProcessingTimeMs:    1,
			ProcessingTimeJitter: 0,
			RateLimitHeaderKeys: []string{"X-User-ID", "X-API-Key"},
			RemoteCheckDelayMs: 0,
		},
		LoadBalancer: LoadBalancerConfig{
			Algorithm:       "round_robin",
			HealthCheckMs:   1000,
			MaxConnPerServer: 10000,
		},
		RateLimiter: RateLimiterConfig{
			Type:        "token_bucket",
			Rate:        10000,
			Burst:       1000,
			WindowMs:    1000,
			Shards:      256, // High shard count for 1M QPS
			KeyTemplate: "{{.X-User-ID}}",
			PrefetchCount: 10,
		},
		RateLimiterService: RateLimiterServiceConfig{
			Enabled:   false,
			Address:   "localhost:9090",
			TimeoutMs: 10,
		},
		Simulation: SimulationConfig{
			DurationSeconds: 60,
			WarmupSeconds:   5,
			RampUpSeconds:   10,
			StopOnError:     false,
		},
		Metrics: MetricsConfig{
			Enabled:         true,
			IntervalSeconds: 1,
			OutputFormat:    "json",
			OutputPath:      "./metrics",
			HistogramBuckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
		},
	}
}

// LoadConfig loads configuration from a YAML file
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	config := DefaultConfig()
	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return config, nil
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.Clients.Count <= 0 {
		return fmt.Errorf("clients.count must be positive")
	}
	if c.Servers.Count <= 0 {
		return fmt.Errorf("servers.count must be positive")
	}
	if c.Servers.MaxRequestsPerConn <= 0 {
		c.Servers.MaxRequestsPerConn = 150
	}
	if c.RateLimiter.Rate <= 0 {
		return fmt.Errorf("rate_limiter.rate must be positive")
	}
	if c.RateLimiter.Shards <= 0 {
		c.RateLimiter.Shards = 256
	}
	
	validModes := map[string]bool{"reuse": true, "per_request": true, "hybrid": true, "dedicated": true}
	if !validModes[c.Clients.ConnectionMode] {
		return fmt.Errorf("invalid connection_mode: %s", c.Clients.ConnectionMode)
	}

	validStrategies := map[string]bool{"fifo": true, "lifo": true}
	if c.Clients.ConnectionPoolStrategy != "" && !validStrategies[c.Clients.ConnectionPoolStrategy] {
		return fmt.Errorf("invalid connection_pool_strategy: %s", c.Clients.ConnectionPoolStrategy)
	}
	if c.Clients.ConnectionPoolStrategy == "" {
		c.Clients.ConnectionPoolStrategy = "fifo"
	}
	
	validAlgorithms := map[string]bool{
		"round_robin": true, "random": true, "weighted": true, "least_conn": true,
	}
	if !validAlgorithms[c.LoadBalancer.Algorithm] {
		return fmt.Errorf("invalid load_balancer.algorithm: %s", c.LoadBalancer.Algorithm)
	}
	
	validLimiters := map[string]bool{
		"token_bucket": true, "sliding_window": true, "fixed_window": true, "leaky_bucket": true, "local_cached": true,
	}
	if !validLimiters[c.RateLimiter.Type] {
		return fmt.Errorf("invalid rate_limiter.type: %s", c.RateLimiter.Type)
	}
	
	return nil
}

// GetWindowDuration returns the window duration for rate limiting
func (c *RateLimiterConfig) GetWindowDuration() time.Duration {
	if c.WindowMs <= 0 {
		return time.Second
	}
	return time.Duration(c.WindowMs) * time.Millisecond
}
