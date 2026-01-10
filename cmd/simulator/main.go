package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/howardlau1999/qps-simulator/pkg/config"
	"github.com/howardlau1999/qps-simulator/pkg/simulator"
)

func main() {
	configPath := flag.String("config", "", "Path to configuration file")
	flag.Parse()

	var cfg *config.Config
	var err error

	if *configPath != "" {
		cfg, err = config.LoadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			os.Exit(1)
		}
	} else {
		cfg = config.DefaultConfig()
		fmt.Println("Using default configuration")
	}

	// Print configuration summary
	fmt.Printf("Configuration:\n")
	fmt.Printf("  Clients: %d (mode: %s, requests/client: %d)\n", 
		cfg.Clients.Count, cfg.Clients.ConnectionMode, cfg.Clients.RequestsPerClient)
	fmt.Printf("  Servers: %d (max requests/conn: %d)\n", 
		cfg.Servers.Count, cfg.Servers.MaxRequestsPerConn)
	fmt.Printf("  Load Balancer: %s\n", cfg.LoadBalancer.Algorithm)
	fmt.Printf("  Rate Limiter: %s (rate: %d/s, burst: %d)\n", 
		cfg.RateLimiter.Type, cfg.RateLimiter.Rate, cfg.RateLimiter.Burst)
	fmt.Printf("  Duration: %ds\n", cfg.Simulation.DurationSeconds)
	fmt.Println()

	// Create simulator
	sim, err := simulator.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating simulator: %v\n", err)
		os.Exit(1)
	}

	// Handle graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Println("\nReceived shutdown signal, stopping...")
		cancel()
	}()

	// Run simulation
	if err := sim.Run(ctx); err != nil {
		if ctx.Err() != context.Canceled {
			fmt.Fprintf(os.Stderr, "Simulation error: %v\n", err)
			os.Exit(1)
		}
	}

	// Cleanup
	if err := sim.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "Error stopping simulator: %v\n", err)
	}

	fmt.Println("Simulation complete")
}
