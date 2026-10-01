package report

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// SummaryHeader is the column list of summary.csv.
var SummaryHeader = []string{
	"run_id", "timestamp", "scenario", "server_instance", "generators", "connections",
	"duration_s", "total_requests", "throughput_rps",
	"latency_p50_ms", "latency_p95_ms", "latency_p99_ms", "latency_p999_ms", "max_latency_ms",
	"errors", "error_rate",
	"server_cpu_avg_pct", "server_cpu_max_pct", "server_rss_mb", "generator_cpu_max_pct",
	"valid",
}

// SummaryRow returns the summary.csv row of a run. Server figures that could
// not be collected are left empty rather than written as zero.
func SummaryRow(r *Run) []string {
	f := func(v float64, digits int) string { return strconv.FormatFloat(v, 'f', digits, 64) }
	opt := func(v *float64, digits int) string {
		if v == nil {
			return ""
		}
		return f(*v, digits)
	}
	lat := r.Result.LatencyMs
	return []string{
		r.RunID,
		r.Timestamp.UTC().Format(time.RFC3339),
		r.Scenario,
		r.Server.InstanceType,
		strconv.Itoa(r.GeneratorHosts),
		strconv.Itoa(r.Connections),
		f(r.MeasurementSeconds, 0),
		strconv.FormatUint(r.Result.Requests, 10),
		f(r.Result.ThroughputRps, 1),
		f(lat.P50, 3), f(lat.P95, 3), f(lat.P99, 3), f(lat.P999, 3), f(lat.Max, 3),
		strconv.FormatUint(r.Result.Errors, 10),
		strconv.FormatFloat(r.Result.ErrorRate, 'g', 6, 64),
		opt(r.ServerStats.CPUAvgPct, 1),
		opt(r.ServerStats.CPUMaxPct, 1),
		opt(r.ServerStats.RSSMaxMB, 1),
		f(r.Generator.CPUPeakPercent, 1),
		strconv.FormatBool(r.Result.Valid),
	}
}

// AppendSummary appends one row to the CSV, writing the header first when
// the file is new or empty. It refuses to append to a file with a different
// header so runs with incompatible columns are never mixed.
func AppendSummary(path string, r *Run) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	st, err := file.Stat()
	if err != nil {
		return err
	}
	w := csv.NewWriter(file)
	if st.Size() == 0 {
		if err := w.Write(SummaryHeader); err != nil {
			return err
		}
	} else {
		header, err := csv.NewReader(file).Read()
		if err != nil {
			return fmt.Errorf("reading %s header: %w", path, err)
		}
		if !equal(header, SummaryHeader) {
			return fmt.Errorf("%s has a different header, use a new summary file", path)
		}
	}
	if err := w.Write(SummaryRow(r)); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
