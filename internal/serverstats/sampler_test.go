package serverstats

import (
	"context"
	"testing"
	"time"

	"github.com/dankomiocevic/ghoti-bench/internal/load"
)

type scripted struct{ conns []float64 }

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) Snapshot(context.Context) (Snapshot, error) {
	snap := emptySnapshot()
	snap.Connections, s.conns = s.conns[0], s.conns[1:]
	return snap, nil
}

func TestConnectionsMinIgnoresClosingSample(t *testing.T) {
	// Warm-up, window start, a dip inside the window, then the closing
	// sample taken while the generator is already disconnecting.
	src := &scripted{conns: []float64{3, 10, 10, 8, 10, 0}}
	s := &Sampler{Source: src, DefaultCores: 1}
	start := time.Now()
	phases := []load.Phase{load.PhaseWarmup, load.PhaseMeasure, load.PhaseMeasure, load.PhaseMeasure, load.PhaseMeasure, load.PhaseMeasure}
	for i, p := range phases {
		s.Tick(start.Add(time.Duration(i-1)*time.Second), p)
	}
	sum := s.Summary(start)
	if sum.ConnectionsMin == nil || *sum.ConnectionsMin != 8 {
		t.Fatalf("min %v", sum.ConnectionsMin)
	}
	if sum.ConnectionsMax == nil || *sum.ConnectionsMax != 10 {
		t.Fatalf("max %v", sum.ConnectionsMax)
	}
}
