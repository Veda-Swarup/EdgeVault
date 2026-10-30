// Package metrics exposes a small Prometheus text endpoint without global state.
package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type Metrics struct {
	CacheHits, CacheMisses, UploadQueueDepth, DirtyBytes atomic.Int64
	CacheReadLatencySum, CacheReadLatencyCount           atomic.Int64
	S3ReadLatencySum, S3ReadLatencyCount                 atomic.Int64
	TotalChunks, DedupChunks                             atomic.Int64
	RansomwareAlerts                                     atomic.Int64
	RansomwareScore                                      atomic.Int64
	LeaseConflicts, CommitConflicts                        atomic.Int64
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE edge_cache_hits_total counter\nedge_cache_hits_total %d\n", m.CacheHits.Load())
	fmt.Fprintf(w, "# TYPE edge_cache_misses_total counter\nedge_cache_misses_total %d\n", m.CacheMisses.Load())
	fmt.Fprintf(w, "# TYPE edge_upload_queue_depth gauge\nedge_upload_queue_depth %d\n", m.UploadQueueDepth.Load())
	fmt.Fprintf(w, "# TYPE edge_dirty_bytes gauge\nedge_dirty_bytes %d\n", m.DirtyBytes.Load())
	fmt.Fprintln(w, "# TYPE edge_read_latency_seconds summary")
	fmt.Fprintf(w, "edge_read_latency_seconds_sum{cache=\"hit\"} %f\nedge_read_latency_seconds_count{cache=\"hit\"} %d\n", float64(m.CacheReadLatencySum.Load())/1e9, m.CacheReadLatencyCount.Load())
	fmt.Fprintf(w, "edge_read_latency_seconds_sum{cache=\"miss\"} %f\nedge_read_latency_seconds_count{cache=\"miss\"} %d\n", float64(m.S3ReadLatencySum.Load())/1e9, m.S3ReadLatencyCount.Load())
	fmt.Fprintln(w, "# TYPE edge_dedup_ratio gauge")
	total, dedup := m.TotalChunks.Load(), m.DedupChunks.Load()
	ratio := float64(0)
	if total > 0 {
		ratio = float64(dedup) / float64(total)
	}
	fmt.Fprintf(w, "edge_dedup_ratio %f\n", ratio)
	fmt.Fprintf(w, "# TYPE edge_ransomware_score gauge\nedge_ransomware_score %f\n", float64(m.RansomwareScore.Load())/1000)
	fmt.Fprintf(w, "# TYPE edge_ransomware_alerts_total counter\nedge_ransomware_alerts_total %d\n", m.RansomwareAlerts.Load())
	fmt.Fprintf(w,"# TYPE edge_lease_conflicts_total counter\nedge_lease_conflicts_total %d\n",m.LeaseConflicts.Load())
	fmt.Fprintf(w,"# TYPE edge_commit_conflicts_total counter\nedge_commit_conflicts_total %d\n",m.CommitConflicts.Load())
}
