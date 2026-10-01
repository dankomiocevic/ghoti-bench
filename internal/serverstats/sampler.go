package serverstats

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/dankomiocevic/ghoti-bench/internal/load"
)

// Point is one server sample on the run timeline.
type Point struct {
	T           float64    `json:"t"`
	Phase       load.Phase `json:"phase"`
	CPUPct      *float64   `json:"cpuPct,omitempty"`
	RSSMB       *float64   `json:"rssMb,omitempty"`
	Connections *float64   `json:"connections,omitempty"`
}

// Summary aggregates the samples of the measurement window. CPU percentages
// are relative to all the cores the server may use, so 100 means saturated.
// Pointer fields are nil when the source could not provide them.
type Summary struct {
	Source         string   `json:"source"`
	Cores          float64  `json:"cores,omitempty"`
	CPUAvgPct      *float64 `json:"cpuAvgPct,omitempty"`
	CPUMaxPct      *float64 `json:"cpuMaxPct,omitempty"`
	CPUAvgCores    *float64 `json:"cpuAvgCores,omitempty"`
	RSSMaxMB       *float64 `json:"rssMaxMb,omitempty"`
	RSSEndMB       *float64 `json:"rssEndMb,omitempty"`
	ConnectionsMax *float64 `json:"connectionsMax,omitempty"`
	// ConnectionsMin is the lowest count seen inside the window, excluding
	// its closing sample, when the generator is already disconnecting.
	ConnectionsMin *float64 `json:"connectionsMin,omitempty"`
	// RequestsDelta is the number of requests Ghoti itself counted during
	// the measurement window, a cross-check of the generator count.
	RequestsDelta *float64 `json:"requestsDelta,omitempty"`
	ScrapeErrors  int      `json:"scrapeErrors"`
	LastError     string   `json:"lastError,omitempty"`
	Timeline      []Point  `json:"timeline,omitempty"`
}

type sample struct {
	at    time.Time
	phase load.Phase
	snap  Snapshot
}

// Sampler collects snapshots on the load generator clock. Its Tick method
// is meant to be used as load.Config.OnTick.
type Sampler struct {
	Source Source
	// DefaultCores is used when the source does not report GOMAXPROCS.
	DefaultCores float64

	mu      sync.Mutex
	samples []sample
	errors  int
	lastErr error
}

// Tick takes one snapshot.
func (s *Sampler) Tick(t time.Time, phase load.Phase) {
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	snap, err := s.Source.Snapshot(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.errors++
		s.lastErr = err
		return
	}
	s.samples = append(s.samples, sample{at: t, phase: phase, snap: snap})
}

// Summary aggregates the measurement samples. measureStart is the start of
// the window, used as the timeline origin.
func (s *Sampler) Summary(measureStart time.Time) Summary {
	s.mu.Lock()
	defer s.mu.Unlock()

	sum := Summary{Source: s.Source.Name(), ScrapeErrors: s.errors}
	if s.lastErr != nil {
		sum.LastError = s.lastErr.Error()
	}

	cores := s.DefaultCores
	for _, sm := range s.samples {
		if sm.snap.Cores > 0 {
			cores = sm.snap.Cores
		}
	}
	sum.Cores = cores

	var first, prev *sample
	var cpuMax, rssMax, connMax, connMin float64 = -1, -1, -1, -1
	for i := range s.samples {
		sm := &s.samples[i]
		// Like the load timeline, a point is labelled by the interval that
		// ends at it: the point at the start of the window is warm-up.
		phase := load.PhaseWarmup
		if sm.at.After(measureStart) {
			phase = load.PhaseMeasure
		}
		p := Point{T: round(sm.at.Sub(measureStart).Seconds(), 3), Phase: phase}
		if sm.snap.RSSBytes >= 0 {
			p.RSSMB = ptr(round(sm.snap.RSSBytes/1e6, 1))
		}
		if sm.snap.Connections >= 0 {
			p.Connections = ptr(sm.snap.Connections)
		}
		if i > 0 {
			pr := &s.samples[i-1]
			dt := sm.at.Sub(pr.at).Seconds()
			if dt > 0 && cores > 0 && sm.snap.CPUSeconds >= 0 && pr.snap.CPUSeconds >= 0 {
				p.CPUPct = ptr(round((sm.snap.CPUSeconds-pr.snap.CPUSeconds)/dt/cores*100, 1))
			}
		}
		sum.Timeline = append(sum.Timeline, p)

		if sm.phase != load.PhaseMeasure {
			continue
		}
		if first == nil {
			first = sm
		} else if p.CPUPct != nil {
			cpuMax = max(cpuMax, *p.CPUPct)
		}
		if sm.snap.RSSBytes >= 0 {
			rssMax = max(rssMax, sm.snap.RSSBytes)
		}
		if sm.snap.Connections >= 0 {
			connMax = max(connMax, sm.snap.Connections)
			if prev != nil && prev.snap.Connections >= 0 && (connMin < 0 || prev.snap.Connections < connMin) {
				connMin = prev.snap.Connections
			}
		}
		prev = sm
	}

	if first == nil {
		return sum
	}
	if cpuMax >= 0 {
		sum.CPUMaxPct = ptr(cpuMax)
	}
	if rssMax >= 0 {
		sum.RSSMaxMB = ptr(round(rssMax/1e6, 1))
	}
	if connMax >= 0 {
		sum.ConnectionsMax = ptr(connMax)
	}
	if connMin >= 0 {
		sum.ConnectionsMin = ptr(connMin)
	}
	if prev.snap.RSSBytes >= 0 {
		sum.RSSEndMB = ptr(round(prev.snap.RSSBytes/1e6, 1))
	}
	window := prev.at.Sub(first.at).Seconds()
	if window > 0 && first.snap.CPUSeconds >= 0 && prev.snap.CPUSeconds >= 0 {
		used := (prev.snap.CPUSeconds - first.snap.CPUSeconds) / window
		sum.CPUAvgCores = ptr(round(used, 3))
		if cores > 0 {
			sum.CPUAvgPct = ptr(round(used/cores*100, 1))
		}
	}
	if first.snap.Requests >= 0 && prev.snap.Requests >= 0 {
		sum.RequestsDelta = ptr(prev.snap.Requests - first.snap.Requests)
	}
	return sum
}

func ptr(v float64) *float64 { return &v }

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}
