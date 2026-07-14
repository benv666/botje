package core

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"time"

	"go-botje/internal/metrics"
)

// startMetrics registers the collectors and serves the Prometheus
// endpoint. The collector reads bus callstats and connection state at
// scrape time; it runs on the scraping goroutine, and bus.Stats takes
// its own lock, so no dispatcher round-trip is needed.
func (c *core) startMetrics(ctx context.Context) {
	reg := c.cfg.Metrics
	if reg == nil {
		return
	}
	seenQueues := map[string]bool{} // zero out buckets that drained away
	reg.AddCollector(func() {
		for id, cs := range c.bus.Stats() {
			labels := map[string]string{"module": id.Module, "event": id.Event}
			reg.SetCounter("botje_hook_calls_total", labels, float64(cs.Count))
			reg.SetCounter("botje_hook_duration_seconds_sum", labels, time.Duration(cs.Total).Seconds())
		}
		connected := 0.0
		if c.conn != nil {
			connected = 1
		}
		reg.SetGauge("botje_connected", nil, connected)
		reg.SetGauge("botje_modules", nil, float64(len(c.bus.Modules())))

		// runtime memory + scheduler pressure (backlog item 3a)
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		reg.SetGauge("go_goroutines", nil, float64(runtime.NumGoroutine()))
		reg.SetGauge("go_memstats_heap_alloc_bytes", nil, float64(m.HeapAlloc))
		reg.SetGauge("go_memstats_heap_sys_bytes", nil, float64(m.HeapSys))
		reg.SetGauge("go_memstats_sys_bytes", nil, float64(m.Sys))
		reg.SetCounter("go_gc_cycles_total", nil, float64(m.NumGC))
		reg.SetCounter("go_gc_pause_seconds_total", nil, time.Duration(m.PauseTotalNs).Seconds())
		reg.SetGauge("botje_work_backlog", nil, float64(len(c.work)))

		depths := map[string]int{}
		if conn := c.conn; conn != nil {
			depths = conn.QueueDepths()
		}
		for ch := range seenQueues {
			if _, live := depths[ch]; !live {
				reg.SetGauge("botje_flood_queue_depth", map[string]string{"channel": ch}, 0)
			}
		}
		for ch, n := range depths {
			seenQueues[ch] = true
			reg.SetGauge("botje_flood_queue_depth", map[string]string{"channel": ch}, float64(n))
		}
	})
	if c.cfg.MetricsAddr == "" {
		return
	}
	ln, err := net.Listen("tcp", c.cfg.MetricsAddr)
	if err != nil {
		slog.Error("core: metrics listen", "addr", c.cfg.MetricsAddr, "err", err)
		return
	}
	slog.Info("core: metrics endpoint up", "addr", ln.Addr())
	srv := &http.Server{Handler: metricsMux(reg)}
	go srv.Serve(ln)
	go func() { <-ctx.Done(); srv.Close() }()
}

func metricsMux(reg *metrics.Registry) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	return mux
}
