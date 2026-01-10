package metrics

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// MetricsReport contains all metrics data for export
type MetricsReport struct {
	StartTime   time.Time          `json:"start_time"`
	EndTime     time.Time          `json:"end_time"`
	Config      ReportConfig       `json:"config"`
	Snapshots   []MetricsSnapshot  `json:"snapshots"`
	FinalResult *MetricsSnapshot   `json:"final_result"`
}

// ReportConfig contains simulation configuration for the report
type ReportConfig struct {
	ClientCount        int    `json:"client_count"`
	ServerCount        int    `json:"server_count"`
	ConnectionMode     string `json:"connection_mode"`
	RateLimiterType    string `json:"rate_limiter_type"`
	RateLimitRate      int64  `json:"rate_limit_rate"`
	MaxRequestsPerConn int    `json:"max_requests_per_conn"`
	DurationSeconds    int    `json:"duration_seconds"`
}

// FileWriter writes metrics to a JSON file
type FileWriter struct {
	path      string
	report    *MetricsReport
	mu        sync.Mutex
}

// NewFileWriter creates a new file writer for metrics
func NewFileWriter(path string, config ReportConfig) *FileWriter {
	return &FileWriter{
		path: path,
		report: &MetricsReport{
			StartTime: time.Now(),
			Config:    config,
			Snapshots: make([]MetricsSnapshot, 0),
		},
	}
}

// AddSnapshot adds a metrics snapshot to the report
func (w *FileWriter) AddSnapshot(snapshot MetricsSnapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.report.Snapshots = append(w.report.Snapshots, snapshot)
}

// SetFinalResult sets the final metrics result
func (w *FileWriter) SetFinalResult(snapshot MetricsSnapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.report.FinalResult = &snapshot
	w.report.EndTime = time.Now()
}

// Save writes the metrics report to the JSON file
func (w *FileWriter) Save() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	data, err := json.MarshalIndent(w.report, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(w.path, data, 0644)
}

// Close saves and closes the file writer
func (w *FileWriter) Close() error {
	return w.Save()
}
