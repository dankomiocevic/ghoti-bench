package serverstats

import (
	"strings"
	"testing"
)

func TestParseMetrics(t *testing.T) {
	in := `# HELP ghoti_connected_clients Number of currently connected clients.
# TYPE ghoti_connected_clients gauge
ghoti_connected_clients 1000
process_cpu_seconds_total 12.5
go_info{version="go1.27.1"} 1
ghoti_request_duration_seconds_bucket{le="0.001"} 7
ghoti_request_duration_seconds_bucket{le="0.0025"} 9
process_resident_memory_bytes 5.4e+07
`
	m, err := ParseMetrics(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"ghoti_connected_clients":               1000,
		"process_cpu_seconds_total":             12.5,
		"go_info":                               1,
		"ghoti_request_duration_seconds_bucket": 7,
		"process_resident_memory_bytes":         5.4e7,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
}

func TestParseCPUTime(t *testing.T) {
	cases := map[string]float64{"0:01.50": 1.5, "1:02.25": 62.25, "1:00:00.00": 3600, "2-00:00:01": 172801}
	for in, want := range cases {
		got, err := parseCPUTime(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v %v", in, got, err)
		}
	}
}
