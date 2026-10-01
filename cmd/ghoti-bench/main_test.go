package main

import (
	"strings"
	"testing"

	"github.com/dankomiocevic/ghoti-bench/internal/report"
)

func testRun(id string, conns int, rps, p50 float64, valid bool, reasons ...string) *report.Run {
	r := &report.Run{RunID: id, Connections: conns}
	r.Result.ThroughputRps = rps
	r.Result.LatencyMs.P50 = p50
	r.Result.Valid = valid
	r.Result.InvalidReasons = reasons
	return r
}

func TestPrintTableExcludesInvalidRuns(t *testing.T) {
	var out strings.Builder
	printTable(&out, []*report.Run{
		testRun("a", 10, 100, 1, true),
		testRun("b", 10, 110, 1, true),
		// Interrupted with no requests: would pull the median to 100.
		testRun("c", 10, 0, 0, false, "run was interrupted"),
		testRun("d", 100, 999, 9, false, "generator CPU peaked"),
	})
	got := out.String()

	lines := strings.Split(got, "\n")
	row10, row100 := strings.Fields(lines[1]), strings.Fields(lines[2])
	// connections runs valid req/s spread ...
	if row10[1] != "3" || row10[2] != "2" || row10[3] != "105" || row10[4] != "±4.8%" {
		t.Fatalf("10 connections row %v", row10)
	}
	if row100[2] != "0" || row100[3] != "N/A" || row100[5] != "N/A" {
		t.Fatalf("100 connections row %v", row100)
	}
	if !strings.Contains(got, "2 invalid runs") || !strings.Contains(got, "c: run was interrupted") ||
		!strings.Contains(got, "d: generator CPU peaked") {
		t.Fatalf("invalid runs not listed:\n%s", got)
	}
}
