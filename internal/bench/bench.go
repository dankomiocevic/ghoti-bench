// Package bench runs one load run end to end: it samples the server while
// the load runs, builds the report and writes summary.csv and run.json.
package bench

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/dankomiocevic/ghoti-bench/internal/load"
	"github.com/dankomiocevic/ghoti-bench/internal/report"
	"github.com/dankomiocevic/ghoti-bench/internal/serverstats"
)

// Spec is everything needed for one run.
type Spec struct {
	Load load.Config
	Meta report.Meta
	// Stats samples the server, nil to skip server stats.
	Stats serverstats.Source
	// ServerCores normalises server CPU when the source does not report
	// GOMAXPROCS. Zero uses the generator core count, only right when the
	// server runs on the same host.
	ServerCores float64

	JSONPath string
	// SummaryPath is the CSV to append to, empty to skip it.
	SummaryPath string
	// Log receives progress and the final summary, nil for silence.
	Log io.Writer
}

// Execute runs the load and writes the reports.
func Execute(ctx context.Context, spec Spec) (*report.Run, error) {
	logf := func(format string, args ...any) {
		if spec.Log != nil {
			fmt.Fprintf(spec.Log, format, args...)
		}
	}

	var sampler *serverstats.Sampler
	if spec.Stats != nil {
		cores := spec.ServerCores
		if cores == 0 {
			cores = float64(runtime.NumCPU())
		}
		sampler = &serverstats.Sampler{Source: spec.Stats, DefaultCores: cores}
	}

	cfg := spec.Load
	lastLog := time.Time{}
	cfg.OnTick = func(t time.Time, phase load.Phase) {
		if sampler != nil {
			sampler.Tick(t, phase)
		}
		if spec.Log != nil && time.Since(lastLog) >= 10*time.Second {
			lastLog = time.Now()
			logf("  %s %s\n", phase, t.Format(time.TimeOnly))
		}
	}

	if spec.Meta.Timestamp.IsZero() {
		spec.Meta.Timestamp = time.Now()
	}
	logf("run %s: %s, %d connections, warm-up %s, measure %s\n",
		spec.Meta.RunID, cfg.Workload, cfg.Connections, cfg.Warmup, cfg.Duration)

	res, err := load.Run(ctx, cfg)
	if err != nil {
		return nil, err
	}

	var server serverstats.Summary
	if sampler != nil {
		server = sampler.Summary(res.MeasureStart)
	}
	run, err := report.Build(spec.Meta, spec.Load, res, server)
	if err != nil {
		return nil, err
	}

	if err := report.WriteJSON(spec.JSONPath, run); err != nil {
		return run, fmt.Errorf("writing %s: %w", spec.JSONPath, err)
	}
	if spec.SummaryPath != "" {
		if err := report.AppendSummary(spec.SummaryPath, run); err != nil {
			return run, fmt.Errorf("writing %s: %w", spec.SummaryPath, err)
		}
	}

	printSummary(spec.Log, run)
	return run, nil
}

func printSummary(w io.Writer, r *report.Run) {
	if w == nil {
		return
	}
	lat := r.Result.LatencyMs
	fmt.Fprintf(w, "  requests %d in %.0fs = %.0f req/s\n", r.Result.Requests, r.MeasurementSeconds, r.Result.ThroughputRps)
	fmt.Fprintf(w, "  latency ms p50 %.3f  p95 %.3f  p99 %.3f  p99.9 %.3f  max %.3f\n", lat.P50, lat.P95, lat.P99, lat.P999, lat.Max)
	fmt.Fprintf(w, "  errors %d (rate %.3g)  generator cpu peak %.1f%%", r.Result.Errors, r.Result.ErrorRate, r.Generator.CPUPeakPercent)
	s := r.ServerStats
	if s.CPUAvgPct != nil {
		fmt.Fprintf(w, "  server cpu avg %.1f%%", *s.CPUAvgPct)
	}
	if s.RSSMaxMB != nil {
		fmt.Fprintf(w, "  server rss %.1f MB", *s.RSSMaxMB)
	}
	if s.ConnectionsMax != nil {
		fmt.Fprintf(w, "  server conns %.0f", *s.ConnectionsMax)
	}
	fmt.Fprintln(w)
	if r.Result.Valid {
		fmt.Fprintln(w, "  valid")
	} else {
		for _, reason := range r.Result.InvalidReasons {
			fmt.Fprintf(w, "  INVALID: %s\n", reason)
		}
	}
}
