package load

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
)

// Sample is one point of the run timeline.
type Sample struct {
	// T is the time in seconds relative to the start of the measurement,
	// negative during the warm-up.
	T     float64 `json:"t"`
	Phase Phase   `json:"phase"`
	// RPS is the rate of validated responses since the previous sample.
	RPS  float64 `json:"rps"`
	Mbps float64 `json:"mbps"`
	// GeneratorCPUPct is the generator process CPU use as a percentage of
	// all the cores of the generator host.
	GeneratorCPUPct float64 `json:"generatorCpuPct"`
}

// Result of a load run.
type Result struct {
	ConnectionsRequested   int
	ConnectionsEstablished int

	StartedAt    time.Time
	MeasureStart time.Time
	MeasureEnd   time.Time
	// MeasuredSeconds is the length of the measurement window, shorter than
	// the configured duration only when the run was interrupted.
	MeasuredSeconds float64

	// Requests counts validated responses whose whole round trip fell
	// inside the measurement window.
	Requests      uint64
	Reads         uint64
	Writes        uint64
	ThroughputRPS float64

	// Latency holds every measured round trip in nanoseconds.
	Latency    *hdrhistogram.Histogram
	MaxLatency time.Duration

	// Errors happened during the measurement window, WarmupErrors before it
	// and LateErrors while in-flight requests were finishing after it.
	Errors       ErrorCounts
	WarmupErrors ErrorCounts
	LateErrors   ErrorCounts
	Reconnects   uint64
	AsyncEvents  uint64
	Interrupted  bool

	Timeline           []Sample
	GeneratorCPUAvgPct float64
	GeneratorCPUMaxPct float64
	NetworkPeakMbps    float64
}

// Run executes a closed-loop load run. It preloads the slots, opens every
// connection, runs the warm-up and the measurement, and returns the merged
// result. Cancelling ctx stops the run early and marks it interrupted.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	plan := buildOps(cfg.Slots, cfg.PayloadSize, cfg.terminator())
	if !cfg.SkipPreload {
		if err := preload(&cfg, &plan); err != nil {
			return nil, fmt.Errorf("preloading slots: %w", err)
		}
	}

	conns, err := openConnections(ctx, &cfg)
	if err != nil {
		return nil, err
	}

	res := &Result{
		ConnectionsRequested:   cfg.Connections,
		ConnectionsEstablished: len(conns),
		StartedAt:              time.Now(),
	}
	res.MeasureStart = res.StartedAt.Add(cfg.Warmup)
	res.MeasureEnd = res.MeasureStart.Add(cfg.Duration)

	var stop atomic.Bool
	workers := make([]*worker, len(conns))
	for i, conn := range conns {
		w := &worker{
			cfg:          &cfg,
			plan:         &plan,
			rng:          rand.New(rand.NewPCG(cfg.Seed, uint64(i))),
			measureStart: res.MeasureStart,
			measureEnd:   res.MeasureEnd,
			stop:         &stop,
			hist:         newHistogram(cfg.RequestTimeout),
		}
		w.attach(conn)
		workers[i] = w
	}

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.run()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	samplerDone := make(chan struct{})
	var tl timeline
	go func() {
		defer close(samplerDone)
		tl.run(&cfg, res, workers, done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		stop.Store(true)
		res.Interrupted = true
		<-done
	}
	<-samplerDone

	merge(res, workers, &cfg, &tl)
	return res, nil
}

func merge(res *Result, workers []*worker, cfg *Config, tl *timeline) {
	res.Latency = newHistogram(cfg.RequestTimeout)
	for _, w := range workers {
		res.Latency.Merge(w.hist)
		res.Requests += w.measuredOps
		res.Reads += w.measuredRead
		res.Writes += w.measuredWrite
		res.Errors.add(w.measureErrors)
		res.WarmupErrors.add(w.warmupErrors)
		res.LateErrors.add(w.lateErrors)
		res.Reconnects += w.reconnects
		res.AsyncEvents += w.asyncEvents
		if w.maxLatency > res.MaxLatency {
			res.MaxLatency = w.maxLatency
		}
	}

	end := res.MeasureEnd
	if res.Interrupted && tl.stoppedAt.Before(end) {
		end = tl.stoppedAt
	}
	res.MeasuredSeconds = end.Sub(res.MeasureStart).Seconds()
	if res.MeasuredSeconds > 0 {
		res.ThroughputRPS = float64(res.Requests) / res.MeasuredSeconds
	} else {
		res.MeasuredSeconds = 0
	}

	res.Timeline = tl.samples
	res.GeneratorCPUAvgPct = tl.cpuAvgPct
	for _, s := range tl.samples {
		if s.Phase != PhaseMeasure {
			continue
		}
		res.GeneratorCPUMaxPct = max(res.GeneratorCPUMaxPct, s.GeneratorCPUPct)
		res.NetworkPeakMbps = max(res.NetworkPeakMbps, s.Mbps)
	}
}

// preload writes the known value into every slot of the workload over a
// single connection and checks the answer.
func preload(cfg *Config, plan *ops) error {
	conn, err := dial(cfg.Addr, cfg.DialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(cfg.RequestTimeout + time.Duration(len(plan.writes))*100*time.Millisecond))

	r := bufio.NewReader(conn)
	for _, o := range plan.writes {
		if _, err := conn.Write(o.request); err != nil {
			return err
		}
		for {
			line, err := r.ReadSlice('\n')
			if err != nil {
				return err
			}
			if line[0] == 'a' {
				continue
			}
			if !bytes.Equal(line, o.response) {
				return fmt.Errorf("request %q answered %q, expected %q; is the slot a simple_memory slot?",
					bytes.TrimSpace(o.request), bytes.TrimSpace(line), bytes.TrimSpace(o.response))
			}
			break
		}
	}
	return nil
}

// openConnections dials every connection before the run starts, with a
// bounded number of concurrent dials. It fails if any connection cannot be
// opened after a few attempts, because a run with fewer connections than
// requested would not measure what was asked.
func openConnections(ctx context.Context, cfg *Config) ([]net.Conn, error) {
	conns := make([]net.Conn, cfg.Connections)
	errs := make([]error, cfg.Connections)
	sem := make(chan struct{}, cfg.DialConcurrency)
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			for attempt := range 5 {
				if ctx.Err() != nil {
					errs[i] = ctx.Err()
					return
				}
				conns[i], errs[i] = dial(cfg.Addr, cfg.DialTimeout)
				if errs[i] == nil {
					return
				}
				time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			}
		}()
	}
	wg.Wait()

	failed := 0
	var firstErr error
	for _, err := range errs {
		if err != nil {
			failed++
			firstErr = err
		}
	}
	if failed > 0 {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
		return nil, fmt.Errorf("could not open %d of %d connections: %w", failed, cfg.Connections, firstErr)
	}
	return conns, nil
}

// timeline samples the workers at a fixed interval aligned to the start of
// the measurement, so the first and last measurement samples fall exactly on
// the window boundaries.
type timeline struct {
	samples   []Sample
	cpuAvgPct float64
	stoppedAt time.Time
}

func (tl *timeline) run(cfg *Config, res *Result, workers []*worker, done <-chan struct{}) {
	interval := cfg.SampleInterval
	cores := float64(runtime.NumCPU())

	// First tick: the earliest aligned tick at or after now.
	k := -int64(cfg.Warmup / interval)
	tickAt := func(k int64) time.Time { return res.MeasureStart.Add(time.Duration(k) * interval) }
	for tickAt(k).Before(res.StartedAt) {
		k++
	}

	var prevT time.Time
	var prevOps, prevBytes uint64
	var prevCPU time.Duration
	var measureStartCPU time.Duration
	haveMeasureStart := false

	for {
		t := tickAt(k)
		timer := time.NewTimer(time.Until(t))
		select {
		case <-timer.C:
		case <-done:
			timer.Stop()
			tl.stoppedAt = time.Now()
			return
		}

		now := time.Now()
		var ops, byts uint64
		for _, w := range workers {
			ops += w.ops.Load()
			byts += w.bytes.Load()
		}
		cpu := processCPUTime()

		phase := PhaseWarmup
		if !t.Before(res.MeasureStart) {
			phase = PhaseMeasure
		}
		if phase == PhaseMeasure && !haveMeasureStart {
			measureStartCPU = cpu
			haveMeasureStart = true
		}

		if !prevT.IsZero() {
			// A sample covers the interval that ends at t, so the one ending
			// exactly at the start of the measurement is still warm-up.
			interval := PhaseWarmup
			if t.After(res.MeasureStart) {
				interval = PhaseMeasure
			}
			dt := now.Sub(prevT).Seconds()
			tl.samples = append(tl.samples, Sample{
				T:               round(t.Sub(res.MeasureStart).Seconds(), 3),
				Phase:           interval,
				RPS:             round(float64(ops-prevOps)/dt, 1),
				Mbps:            round(float64(byts-prevBytes)*8/dt/1e6, 2),
				GeneratorCPUPct: round((cpu-prevCPU).Seconds()/dt/cores*100, 1),
			})
		}
		if cfg.OnTick != nil {
			cfg.OnTick(t, phase)
		}
		prevT, prevOps, prevBytes, prevCPU = now, ops, byts, cpu

		if !t.Before(res.MeasureEnd) {
			if haveMeasureStart {
				window := res.MeasureEnd.Sub(res.MeasureStart).Seconds()
				tl.cpuAvgPct = (cpu - measureStartCPU).Seconds() / window / cores * 100
			}
			tl.stoppedAt = res.MeasureEnd
			return
		}
		k++
	}
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}
