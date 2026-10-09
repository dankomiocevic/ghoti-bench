package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"

	"github.com/dankomiocevic/ghoti-bench/internal/load"
	"github.com/dankomiocevic/ghoti-bench/internal/serverstats"
)

func sampleRun(t *testing.T, mutate func(*load.Result)) *Run {
	t.Helper()
	conns := 10.0
	return sampleRunWithServer(t, mutate, serverstats.Summary{ConnectionsMax: &conns, ConnectionsMin: &conns})
}

func sampleRunWithServer(t *testing.T, mutate func(*load.Result), server serverstats.Summary) *Run {
	t.Helper()
	h := hdrhistogram.New(1000, 5e9, 3)
	for i := int64(1); i <= 1000; i++ {
		h.RecordValue(i * 10_000) // 0.01ms .. 10ms
	}
	res := &load.Result{
		ConnectionsRequested: 10, ConnectionsEstablished: 10,
		Requests: 1000, Reads: 1000, MeasuredSeconds: 10, ThroughputRPS: 100,
		Latency: h, MaxLatency: 10 * time.Millisecond, GeneratorCPUMaxPct: 12,
	}
	if mutate != nil {
		mutate(res)
	}
	cfg := load.Config{Addr: "127.0.0.1:9090", Workload: load.WorkloadSimpleRead, Connections: 10,
		Slots: load.SlotRange{}, PayloadSize: 36, ReadPercent: 100, Warmup: 30 * time.Second}
	r, err := Build(Meta{RunID: "read-001", Scenario: "memory-read", ServerInstance: "t4g.small", SaveHistogram: true},
		cfg, res, server)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBuildValidRun(t *testing.T) {
	r := sampleRun(t, nil)
	if !r.Result.Valid {
		t.Fatalf("expected valid, got %v", r.Result.InvalidReasons)
	}
	if r.Request != `r000\n` || r.ResponseBytes != 36 || r.ResponseWireBytes != 41 {
		t.Fatalf("request %q response %d/%d", r.Request, r.ResponseBytes, r.ResponseWireBytes)
	}
	if p := r.Result.LatencyMs.P50; p < 4.9 || p > 5.1 {
		t.Fatalf("p50 %v", p)
	}
	if r.Result.LatencyMs.Max != 10 {
		t.Fatalf("max %v", r.Result.LatencyMs.Max)
	}
	if h, err := hdrhistogram.Decode([]byte(r.Histogram)); err != nil || h.TotalCount() != 1000 {
		t.Fatalf("histogram round trip: %v", err)
	}
}

func TestBuildInvalidReasons(t *testing.T) {
	r := sampleRun(t, func(res *load.Result) {
		res.Errors.Incorrect = 1
		res.GeneratorCPUMaxPct = 95
		res.ConnectionsEstablished = 9
	})
	if r.Result.Valid || len(r.Result.InvalidReasons) != 3 {
		t.Fatalf("reasons %v", r.Result.InvalidReasons)
	}
	if r.Result.Errors != 1 || r.Result.ErrorRate == 0 {
		t.Fatalf("errors %d rate %v", r.Result.Errors, r.Result.ErrorRate)
	}
}

func TestAppendSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "summary.csv")
	r := sampleRun(t, nil)
	for range 2 {
		if err := AppendSummary(path, r); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := "run_id,timestamp,scenario,server_instance,generators,connections,duration_s,total_requests,throughput_rps,latency_p50_ms,latency_p95_ms,latency_p99_ms,latency_p999_ms,max_latency_ms,errors,error_rate,server_cpu_avg_pct,server_cpu_max_pct,server_rss_mb,generator_cpu_max_pct,valid"
	if len(lines) != 3 || lines[0] != want {
		t.Fatalf("got\n%s", data)
	}
	if !strings.HasPrefix(lines[1], "read-001,") || !strings.HasSuffix(lines[1], ",,,,12.0,true") {
		t.Fatalf("row %q", lines[1])
	}

	os.WriteFile(path, []byte("other,header\n"), 0o644)
	if err := AppendSummary(path, r); err == nil {
		t.Fatal("appending to a file with another header must fail")
	}
}

func TestBuildConnectionsDroppedDuringMeasurement(t *testing.T) {
	peak, low := 10.0, 7.0
	r := sampleRunWithServer(t, nil, serverstats.Summary{ConnectionsMax: &peak, ConnectionsMin: &low})
	if r.Result.Valid || len(r.Result.InvalidReasons) != 1 ||
		!strings.Contains(r.Result.InvalidReasons[0], "as few as 7 connected clients") {
		t.Fatalf("reasons %v", r.Result.InvalidReasons)
	}

	// With a single measurement sample there is no minimum, the maximum is
	// the only evidence.
	r = sampleRunWithServer(t, nil, serverstats.Summary{ConnectionsMax: &low})
	if r.Result.Valid {
		t.Fatal("a maximum below the requested connections must be invalid")
	}

	// A server without connection metrics cannot invalidate the run.
	r = sampleRunWithServer(t, nil, serverstats.Summary{})
	if !r.Result.Valid {
		t.Fatalf("reasons %v", r.Result.InvalidReasons)
	}
}

func TestBuildCounterCheck(t *testing.T) {
	r := sampleRun(t, func(res *load.Result) {
		res.CounterCheck = &load.CounterCheck{Increments: 1200}
	})
	if !r.Result.Valid || r.Result.CounterCheck == nil {
		t.Fatalf("passed counter check: valid %v, check %+v", r.Result.Valid, r.Result.CounterCheck)
	}

	r = sampleRun(t, func(res *load.Result) {
		res.CounterCheck = &load.CounterCheck{Increments: 1200, Mismatches: []string{"slot 100: 1200 increments validated, counter at 1199"}}
	})
	if r.Result.Valid || !strings.Contains(strings.Join(r.Result.InvalidReasons, ";"), "counter mismatch, slot 100") {
		t.Fatalf("failed counter check: valid %v, reasons %v", r.Result.Valid, r.Result.InvalidReasons)
	}
}
