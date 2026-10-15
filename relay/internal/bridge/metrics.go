package bridge

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// metrics.go — Prometheus text exposition for the bridge (P8-T3, roadmap §13).
// Mounted only when TUNR_METRICS_TOKEN is set; otherwise 404 like
// /api/v1/density. Gauges come from live state and the database at scrape
// time; the kill-latency histogram is recorded in memory.

const EnvMetricsToken = "TUNR_METRICS_TOKEN"

var killBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5}

type histogram struct {
	mu      sync.Mutex
	buckets []float64
	counts  []uint64
	sum     float64
	n       uint64
}

func newHistogram(b []float64) *histogram {
	return &histogram{buckets: b, counts: make([]uint64, len(b))}
}

func (h *histogram) observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.n++
}

func (h *histogram) write(b *strings.Builder, name, help string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	writeHist(b, name, help, h.buckets, h.counts, h.sum, h.n)
}

func writeHist(b *strings.Builder, name, help string, buckets []float64, counts []uint64, sum float64, n uint64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for i, le := range buckets {
		fmt.Fprintf(b, "%s_bucket{le=\"%g\"} %d\n", name, le, counts[i])
	}
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n", name, n, name, sum, name, n)
}

// histFrom builds bucket counts from raw samples.
func histFrom(samples []float64, buckets []float64) ([]uint64, float64) {
	counts := make([]uint64, len(buckets))
	sum := 0.0
	for _, v := range samples {
		sum += v
		for i, le := range buckets {
			if v <= le {
				counts[i]++
			}
		}
	}
	return counts, sum
}

func gauge(b *strings.Builder, name, help string, v float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.MetricsToken)) != 1 {
		http.NotFound(w, r)
		return
	}
	var b strings.Builder
	s.hub.mu.Lock()
	online := len(s.hub.sessions)
	s.hub.mu.Unlock()
	s.access.mu.Lock()
	open := len(s.access.streams)
	s.access.mu.Unlock()
	gauge(&b, "tunr_bridge_sites_online", "Connectors attached right now.", float64(online))
	gauge(&b, "tunr_bridge_sessions_open", "Open agent streams.", float64(open))

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	if st, err := s.db.GetBridgeStats(ctx); err == nil {
		gauge(&b, "tunr_bridge_active_sites_7d", "Sites with at least one authorised stream in the last 7 days (north star).", float64(st.ActiveSites7d))
		fmt.Fprint(&b, "# HELP tunr_bridge_grants Grants by status.\n# TYPE tunr_bridge_grants gauge\n")
		statuses := make([]string, 0, len(st.GrantsByStatus))
		for k := range st.GrantsByStatus {
			statuses = append(statuses, k)
		}
		sort.Strings(statuses)
		for _, k := range statuses {
			fmt.Fprintf(&b, "tunr_bridge_grants{status=%q} %d\n", k, st.GrantsByStatus[k])
		}
		fmt.Fprint(&b, "# HELP tunr_bridge_queries_total Query summaries recorded, by blocked.\n# TYPE tunr_bridge_queries_total counter\n")
		fmt.Fprintf(&b, "tunr_bridge_queries_total{blocked=\"false\"} %d\ntunr_bridge_queries_total{blocked=\"true\"} %d\n", st.Queries[false], st.Queries[true])
		ab := []float64{60, 300, 900, 3600, 4 * 3600, 24 * 3600}
		c, sum := histFrom(st.ApprovalSeconds, ab)
		writeHist(&b, "tunr_bridge_grant_approval_seconds", "Time from request to human approval, last 30 days.", ab, c, sum, uint64(len(st.ApprovalSeconds)))
		fb := []float64{600, 1800, 3600, 4 * 3600, 24 * 3600, 7 * 24 * 3600}
		c, sum = histFrom(st.FirstQuerySecs, fb)
		writeHist(&b, "tunr_bridge_time_to_first_query_seconds", "Site created to first successful query (the 'under an hour' promise).", fb, c, sum, uint64(len(st.FirstQuerySecs)))
	}
	s.killLatency.write(&b, "tunr_bridge_kill_latency_seconds", "Time to cut live streams on revoke or kill (invariant: ≤ 2 s).")
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) observeKill(d time.Duration) { s.killLatency.observe(d.Seconds()) }
