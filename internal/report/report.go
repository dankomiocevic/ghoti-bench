// Package report turns a load result into the two files kept for every run:
// a row in summary.csv for comparing runs and a run.json with the full
// configuration and diagnostics.
package report

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"

	"github.com/dankomiocevic/ghoti-bench/internal/load"
	"github.com/dankomiocevic/ghoti-bench/internal/serverstats"
)

// DefaultMaxGeneratorCPUPct is the generator CPU use above which a run is
// flagged invalid: past it the generator, not Ghoti, may be the bottleneck.
const DefaultMaxGeneratorCPUPct = 80

// Meta is the context of a run that the load result does not know.
type Meta struct {
	RunID      string
	Scenario   string
	Repetition int
	Timestamp  time.Time

	ServerInstance    string
	GeneratorInstance string
	Region            string

	GhotiVersion string
	GhotiCommit  string
	GhotiRef     string
	GhotiBinary  string
	// Target is set when the benchmark ran against an external server.
	Target string
	// ServerGOMAXPROCS is the GOMAXPROCS forced on a server the benchmark
	// started, 0 when left to the Go runtime.
	ServerGOMAXPROCS int

	SaveHistogram      bool
	MaxGeneratorCPUPct float64
}

// Run is the content of run.json.
type Run struct {
	RunID              string    `json:"runId"`
	Timestamp          time.Time `json:"timestamp"`
	GitCommit          string    `json:"gitCommit"`
	Scenario           string    `json:"scenario"`
	Repetition         int       `json:"repetition,omitempty"`
	Workload           string    `json:"workload"`
	Protocol           string    `json:"protocol"`
	Request            string    `json:"request"`
	ResponseBytes      int       `json:"responseBytes"`
	ResponseWireBytes  int       `json:"responseWireBytes"`
	ReadPercent        int       `json:"readPercent"`
	Slots              string    `json:"slots"`
	WarmupSeconds      float64   `json:"warmupSeconds"`
	MeasurementSeconds float64   `json:"measurementSeconds"`
	Connections        int       `json:"connections"`
	GeneratorHosts     int       `json:"generatorHosts"`
	RequestTimeoutMs   float64   `json:"requestTimeoutMs"`
	Seed               uint64    `json:"seed"`

	Server    ServerInfo    `json:"server"`
	Generator GeneratorInfo `json:"generator"`
	Result    ResultInfo    `json:"result"`

	ServerStats serverstats.Summary `json:"serverStats"`
	Timeline    []load.Sample       `json:"timeline"`
	// Histogram is the HDR histogram of the measured round trips in
	// nanoseconds, V2 compressed and base64 encoded.
	Histogram string `json:"histogram,omitempty"`
}

type ServerInfo struct {
	InstanceType string `json:"instanceType"`
	Region       string `json:"region,omitempty"`
	GhotiVersion string `json:"ghotiVersion,omitempty"`
	GhotiRef     string `json:"ghotiRef,omitempty"`
	Binary       string `json:"binary,omitempty"`
	Addr         string `json:"addr"`
	External     bool   `json:"external"`
	GOMAXPROCS   int    `json:"gomaxprocs,omitempty"`
}

type GeneratorInfo struct {
	InstanceType    string  `json:"instanceType"`
	Hostname        string  `json:"hostname"`
	OS              string  `json:"os"`
	Arch            string  `json:"arch"`
	CPUs            int     `json:"cpus"`
	GoVersion       string  `json:"goVersion"`
	BenchCommit     string  `json:"benchCommit,omitempty"`
	CPUPeakPercent  float64 `json:"cpuPeakPercent"`
	CPUAvgPercent   float64 `json:"cpuAvgPercent"`
	NetworkPeakMbps float64 `json:"networkPeakMbps"`
}

type ResultInfo struct {
	Requests               uint64           `json:"requests"`
	Reads                  uint64           `json:"reads"`
	Writes                 uint64           `json:"writes"`
	ThroughputRps          float64          `json:"throughputRps"`
	Errors                 uint64           `json:"errors"`
	ErrorRate              float64          `json:"errorRate"`
	ErrorBreakdown         load.ErrorCounts `json:"errorBreakdown"`
	WarmupErrors           load.ErrorCounts `json:"warmupErrors"`
	LateErrors             load.ErrorCounts `json:"lateErrors"`
	LatencyMs              Latency          `json:"latencyMs"`
	ConnectionsEstablished int              `json:"connectionsEstablished"`
	Reconnects             uint64           `json:"reconnects"`
	AsyncEvents            uint64           `json:"asyncEvents"`
	Interrupted            bool             `json:"interrupted"`
	Valid                  bool             `json:"valid"`
	InvalidReasons         []string         `json:"invalidReasons,omitempty"`
}

type Latency struct {
	Min    float64 `json:"min"`
	Mean   float64 `json:"mean"`
	StdDev float64 `json:"stddev"`
	P50    float64 `json:"p50"`
	P90    float64 `json:"p90"`
	P95    float64 `json:"p95"`
	P99    float64 `json:"p99"`
	P999   float64 `json:"p999"`
	Max    float64 `json:"max"`
}

// Build assembles the run report and decides whether the run is valid.
func Build(meta Meta, cfg load.Config, res *load.Result, server serverstats.Summary) (*Run, error) {
	hostname, _ := os.Hostname()
	r := &Run{
		RunID:              meta.RunID,
		Timestamp:          meta.Timestamp.UTC(),
		GitCommit:          meta.GhotiCommit,
		Scenario:           meta.Scenario,
		Repetition:         meta.Repetition,
		Workload:           cfg.Workload,
		Protocol:           cfg.Protocol,
		Request:            cfg.RequestPattern(),
		ResponseBytes:      cfg.PayloadSize,
		ResponseWireBytes:  cfg.ResponseWireBytes(),
		ReadPercent:        cfg.ReadPercent,
		Slots:              cfg.Slots.String(),
		WarmupSeconds:      cfg.Warmup.Seconds(),
		MeasurementSeconds: res.MeasuredSeconds,
		Connections:        cfg.Connections,
		GeneratorHosts:     1,
		RequestTimeoutMs:   float64(cfg.RequestTimeout) / 1e6,
		Seed:               cfg.Seed,
		Server: ServerInfo{
			InstanceType: meta.ServerInstance,
			Region:       meta.Region,
			GhotiVersion: meta.GhotiVersion,
			GhotiRef:     meta.GhotiRef,
			Binary:       meta.GhotiBinary,
			Addr:         cfg.Addr,
			External:     meta.Target != "",
			GOMAXPROCS:   meta.ServerGOMAXPROCS,
		},
		Generator: GeneratorInfo{
			InstanceType:    meta.GeneratorInstance,
			Hostname:        hostname,
			OS:              runtime.GOOS,
			Arch:            runtime.GOARCH,
			CPUs:            runtime.NumCPU(),
			GoVersion:       runtime.Version(),
			BenchCommit:     benchCommit(),
			CPUPeakPercent:  round(res.GeneratorCPUMaxPct, 1),
			CPUAvgPercent:   round(res.GeneratorCPUAvgPct, 1),
			NetworkPeakMbps: round(res.NetworkPeakMbps, 1),
		},
		ServerStats: server,
		Timeline:    res.Timeline,
	}

	errs := res.Errors.Total()
	r.Result = ResultInfo{
		Requests:               res.Requests,
		Reads:                  res.Reads,
		Writes:                 res.Writes,
		ThroughputRps:          round(res.ThroughputRPS, 1),
		Errors:                 errs,
		ErrorBreakdown:         res.Errors,
		WarmupErrors:           res.WarmupErrors,
		LateErrors:             res.LateErrors,
		LatencyMs:              latency(res.Latency, res.MaxLatency),
		ConnectionsEstablished: res.ConnectionsEstablished,
		Reconnects:             res.Reconnects,
		AsyncEvents:            res.AsyncEvents,
		Interrupted:            res.Interrupted,
	}
	if total := res.Requests + errs; total > 0 {
		r.Result.ErrorRate = float64(errs) / float64(total)
	}

	maxGen := meta.MaxGeneratorCPUPct
	if maxGen == 0 {
		maxGen = DefaultMaxGeneratorCPUPct
	}
	r.Result.InvalidReasons = invalidReasons(res, server, maxGen)
	r.Result.Valid = len(r.Result.InvalidReasons) == 0

	if meta.SaveHistogram && res.Latency != nil {
		enc, err := res.Latency.Encode(hdrhistogram.V2CompressedEncodingCookieBase)
		if err != nil {
			return nil, fmt.Errorf("encoding histogram: %w", err)
		}
		r.Histogram = string(enc)
	}
	return r, nil
}

func invalidReasons(res *load.Result, server serverstats.Summary, maxGenCPU float64) []string {
	var reasons []string
	if res.Interrupted {
		reasons = append(reasons, "run was interrupted")
	}
	if res.Requests == 0 {
		reasons = append(reasons, "no validated requests")
	}
	if res.ConnectionsEstablished != res.ConnectionsRequested {
		reasons = append(reasons, fmt.Sprintf("%d of %d connections established", res.ConnectionsEstablished, res.ConnectionsRequested))
	}
	if n := res.Errors.Total(); n > 0 {
		reasons = append(reasons, fmt.Sprintf("%d errors during measurement", n))
	}
	if n := res.WarmupErrors.Total() + res.LateErrors.Total(); n > 0 {
		reasons = append(reasons, fmt.Sprintf("%d errors outside the measurement window", n))
	}
	if res.GeneratorCPUMaxPct > maxGenCPU {
		reasons = append(reasons, fmt.Sprintf("generator CPU peaked at %.1f%% (limit %.0f%%), the generator may be the bottleneck", res.GeneratorCPUMaxPct, maxGenCPU))
	}
	// The lowest count inside the window catches connections dropped during
	// the measurement; the maximum only proves they existed at some point.
	// It falls back to the maximum when the window had a single sample.
	if conns := server.ConnectionsMin; conns != nil || server.ConnectionsMax != nil {
		if conns == nil {
			conns = server.ConnectionsMax
		}
		if *conns < float64(res.ConnectionsRequested) {
			reasons = append(reasons, fmt.Sprintf("server reported as few as %.0f connected clients during the measurement, expected %d", *conns, res.ConnectionsRequested))
		}
	}
	return reasons
}

func latency(h *hdrhistogram.Histogram, maxLat time.Duration) Latency {
	if h == nil || h.TotalCount() == 0 {
		return Latency{}
	}
	ms := func(ns float64) float64 { return round(ns/1e6, 3) }
	return Latency{
		Min:    ms(float64(h.Min())),
		Mean:   ms(h.Mean()),
		StdDev: ms(h.StdDev()),
		P50:    ms(float64(h.ValueAtPercentile(50))),
		P90:    ms(float64(h.ValueAtPercentile(90))),
		P95:    ms(float64(h.ValueAtPercentile(95))),
		P99:    ms(float64(h.ValueAtPercentile(99))),
		P999:   ms(float64(h.ValueAtPercentile(99.9))),
		// The exact maximum, the histogram only keeps it to 3 digits.
		Max: ms(float64(maxLat.Nanoseconds())),
	}
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

func benchCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev == "" {
		return ""
	}
	return rev + dirty
}

// WriteJSON writes the run report, creating the directory if needed.
func WriteJSON(path string, r *Run) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
