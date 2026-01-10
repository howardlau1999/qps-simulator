package loadbalancer

import (
	"sync"
	"testing"

	"github.com/howardlau1999/qps-simulator/pkg/types"
)

func TestRoundRobin_PickServer(t *testing.T) {
	lb := NewRoundRobin()

	servers := []*types.ServerInfo{
		{ID: "server-1", Address: "localhost", Port: 8080, Healthy: true},
		{ID: "server-2", Address: "localhost", Port: 8081, Healthy: true},
		{ID: "server-3", Address: "localhost", Port: 8082, Healthy: true},
	}

	for _, s := range servers {
		if err := lb.RegisterServer(s); err != nil {
			t.Fatalf("Failed to register server: %v", err)
		}
	}

	// Pick servers in round-robin order
	expectedOrder := []string{"server-1", "server-2", "server-3", "server-1", "server-2"}
	for i, expected := range expectedOrder {
		picked, err := lb.PickServer()
		if err != nil {
			t.Fatalf("PickServer failed: %v", err)
		}
		if picked.ID != expected {
			t.Errorf("Pick %d: expected %s, got %s", i, expected, picked.ID)
		}
	}
}

func TestRoundRobin_SkipsUnhealthy(t *testing.T) {
	lb := NewRoundRobin()

	lb.RegisterServer(&types.ServerInfo{ID: "server-1", Healthy: true})
	lb.RegisterServer(&types.ServerInfo{ID: "server-2", Healthy: true})

	// Mark server-1 as unhealthy
	lb.MarkUnhealthy("server-1")

	// All picks should be server-2
	for i := 0; i < 5; i++ {
		picked, _ := lb.PickServer()
		if picked.ID != "server-2" {
			t.Errorf("Expected server-2, got %s", picked.ID)
		}
	}
}

func TestRandom_PickServer(t *testing.T) {
	lb := NewRandom()

	lb.RegisterServer(&types.ServerInfo{ID: "server-1", Healthy: true})
	lb.RegisterServer(&types.ServerInfo{ID: "server-2", Healthy: true})

	// Just verify it doesn't error
	for i := 0; i < 100; i++ {
		picked, err := lb.PickServer()
		if err != nil {
			t.Fatalf("PickServer failed: %v", err)
		}
		if picked.ID != "server-1" && picked.ID != "server-2" {
			t.Errorf("Unexpected server: %s", picked.ID)
		}
	}
}

func TestWeighted_PickServer(t *testing.T) {
	lb := NewWeighted()

	lb.RegisterServer(&types.ServerInfo{ID: "server-1", Weight: 2, Healthy: true})
	lb.RegisterServer(&types.ServerInfo{ID: "server-2", Weight: 1, Healthy: true})

	counts := make(map[string]int)
	for i := 0; i < 300; i++ {
		picked, _ := lb.PickServer()
		counts[picked.ID]++
	}

	// server-1 should get roughly 2x the requests
	ratio := float64(counts["server-1"]) / float64(counts["server-2"])
	if ratio < 1.5 || ratio > 2.5 {
		t.Errorf("Weighted distribution off: server-1=%d, server-2=%d, ratio=%.2f",
			counts["server-1"], counts["server-2"], ratio)
	}
}

func TestLeastConnections_PickServer(t *testing.T) {
	lb := NewLeastConnections()

	lb.RegisterServer(&types.ServerInfo{ID: "server-1", Healthy: true})
	lb.RegisterServer(&types.ServerInfo{ID: "server-2", Healthy: true})

	// Add connections to server-1
	lb.IncrementConnections("server-1")
	lb.IncrementConnections("server-1")

	// Should pick server-2 (0 connections)
	picked, _ := lb.PickServer()
	if picked.ID != "server-2" {
		t.Errorf("Expected server-2 (least connections), got %s", picked.ID)
	}
}

func TestLoadBalancer_Concurrency(t *testing.T) {
	lb := NewRoundRobin()

	for i := 0; i < 10; i++ {
		lb.RegisterServer(&types.ServerInfo{
			ID:      string(rune('A' + i)),
			Healthy: true,
		})
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, err := lb.PickServer()
				if err != nil {
					t.Errorf("PickServer failed: %v", err)
				}
			}
		}()
	}

	wg.Wait()
}

func TestNew(t *testing.T) {
	tests := []struct {
		algorithm string
		wantErr   bool
	}{
		{"round_robin", false},
		{"random", false},
		{"weighted", false},
		{"least_conn", false},
		{"unknown", true},
	}

	for _, tt := range tests {
		t.Run(tt.algorithm, func(t *testing.T) {
			_, err := New(tt.algorithm)
			if (err != nil) != tt.wantErr {
				t.Errorf("New(%s) error = %v, wantErr %v", tt.algorithm, err, tt.wantErr)
			}
		})
	}
}
