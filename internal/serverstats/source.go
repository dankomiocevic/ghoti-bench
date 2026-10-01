// Package serverstats samples the resource use of the Ghoti server under
// test, either from its Prometheus endpoint or from the local process.
package serverstats

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Snapshot is the cumulative server state at one moment. Fields that the
// source cannot provide are negative.
type Snapshot struct {
	CPUSeconds  float64
	RSSBytes    float64
	Connections float64
	Requests    float64
	// Cores is the number of cores the server may use (GOMAXPROCS), used to
	// normalise CPU use to a percentage of the server capacity.
	Cores float64
}

func emptySnapshot() Snapshot {
	return Snapshot{CPUSeconds: -1, RSSBytes: -1, Connections: -1, Requests: -1, Cores: -1}
}

// Source provides snapshots of the server.
type Source interface {
	Name() string
	Snapshot(ctx context.Context) (Snapshot, error)
}

// Prometheus scrapes a Ghoti metrics endpoint. Ghoti registers the standard
// process and Go collectors, so CPU, RSS and GOMAXPROCS are available next to
// the Ghoti metrics wherever the process collector is supported.
type Prometheus struct {
	URL    string
	Client *http.Client
}

// NewPrometheus accepts a host:port or a full URL.
func NewPrometheus(addrOrURL string) *Prometheus {
	u := addrOrURL
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	if !strings.HasSuffix(strings.TrimSuffix(u, "/"), "/metrics") {
		u = strings.TrimSuffix(u, "/") + "/metrics"
	}
	return &Prometheus{URL: u, Client: &http.Client{Timeout: 800 * time.Millisecond}}
}

func (p *Prometheus) Name() string { return "prometheus:" + p.URL }

func (p *Prometheus) Snapshot(ctx context.Context) (Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return emptySnapshot(), err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return emptySnapshot(), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return emptySnapshot(), fmt.Errorf("metrics endpoint returned %s", resp.Status)
	}
	values, err := ParseMetrics(resp.Body)
	if err != nil {
		return emptySnapshot(), err
	}

	s := emptySnapshot()
	pick := func(dst *float64, name string) {
		if v, ok := values[name]; ok {
			*dst = v
		}
	}
	pick(&s.CPUSeconds, "process_cpu_seconds_total")
	pick(&s.RSSBytes, "process_resident_memory_bytes")
	pick(&s.Connections, "ghoti_connected_clients")
	pick(&s.Requests, "ghoti_requests_total")
	pick(&s.Cores, "go_sched_gomaxprocs_threads")
	return s, nil
}

// ParseMetrics reads the Prometheus text exposition format and returns the
// first value of every unlabelled or labelled sample by metric name. It only
// needs the handful of gauges and counters used here, so labels are ignored.
func ParseMetrics(r io.Reader) (map[string]float64, error) {
	values := make(map[string]float64)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		var name, rest string
		if i := strings.IndexByte(line, '{'); i >= 0 {
			j := strings.LastIndexByte(line, '}')
			if j < i {
				continue
			}
			name, rest = line[:i], line[j+1:]
		} else {
			var ok bool
			name, rest, ok = strings.Cut(line, " ")
			if !ok {
				continue
			}
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		if _, seen := values[name]; !seen {
			values[name] = v
		}
	}
	return values, sc.Err()
}

// Process samples a local process by PID. It works for any binary, with or
// without metrics enabled.
type Process struct {
	PID int
}

func (p *Process) Name() string { return fmt.Sprintf("process:%d", p.PID) }

func (p *Process) Snapshot(ctx context.Context) (Snapshot, error) {
	s := emptySnapshot()
	var err error
	switch runtime.GOOS {
	case "linux":
		s.CPUSeconds, s.RSSBytes, err = procLinux(p.PID)
	default:
		s.CPUSeconds, s.RSSBytes, err = procPS(ctx, p.PID)
	}
	return s, err
}

// procLinux reads utime+stime from /proc/<pid>/stat and VmRSS from
// /proc/<pid>/status.
func procLinux(pid int) (cpu, rss float64, err error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1, -1, err
	}
	// The command name may contain spaces, fields start after its ')'.
	i := strings.LastIndexByte(string(stat), ')')
	if i < 0 {
		return -1, -1, errors.New("malformed /proc stat")
	}
	fields := strings.Fields(string(stat[i+1:]))
	// fields[0] is state (field 3), utime is field 14 and stime field 15.
	if len(fields) < 13 {
		return -1, -1, errors.New("malformed /proc stat")
	}
	utime, _ := strconv.ParseFloat(fields[11], 64)
	stime, _ := strconv.ParseFloat(fields[12], 64)
	const clockTicks = 100 // USER_HZ, 100 on every mainstream Linux build
	cpu = (utime + stime) / clockTicks

	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return cpu, -1, err
	}
	rss = -1
	for line := range strings.SplitSeq(string(status), "\n") {
		if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			kb, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 64)
			rss = kb * 1024
		}
	}
	return cpu, rss, nil
}

// procPS uses ps(1), which on macOS reports the cumulative CPU time with
// centisecond resolution.
func procPS(ctx context.Context, pid int) (cpu, rss float64, err error) {
	out, err := exec.CommandContext(ctx, "ps", "-o", "time=,rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1, -1, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return -1, -1, fmt.Errorf("unexpected ps output %q", out)
	}
	cpu, err = parseCPUTime(fields[0])
	if err != nil {
		return -1, -1, err
	}
	kb, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return -1, -1, err
	}
	return cpu, kb * 1024, nil
}

// parseCPUTime parses ps time values such as "1:02.35", "1:02:03.45" or
// "2-01:02:03".
func parseCPUTime(s string) (float64, error) {
	var days float64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.ParseFloat(d, 64)
		if err != nil {
			return 0, err
		}
		days, s = n, rest
	}
	total := 0.0
	for part := range strings.SplitSeq(s, ":") {
		n, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid cpu time %q", s)
		}
		total = total*60 + n
	}
	return days*86400 + total, nil
}

// Multi combines sources: every field comes from the first source that
// provides it, so Prometheus can supply connections while the local process
// supplies CPU on platforms where the process collector is not available.
type Multi []Source

func (m Multi) Name() string {
	names := make([]string, len(m))
	for i, s := range m {
		names[i] = s.Name()
	}
	return strings.Join(names, "+")
}

func (m Multi) Snapshot(ctx context.Context) (Snapshot, error) {
	out := emptySnapshot()
	var errs []error
	for _, src := range m {
		s, err := src.Snapshot(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
			continue
		}
		fill := func(dst *float64, v float64) {
			if *dst < 0 && v >= 0 {
				*dst = v
			}
		}
		fill(&out.CPUSeconds, s.CPUSeconds)
		fill(&out.RSSBytes, s.RSSBytes)
		fill(&out.Connections, s.Connections)
		fill(&out.Requests, s.Requests)
		fill(&out.Cores, s.Cores)
	}
	if len(errs) == len(m) {
		return out, errors.Join(errs...)
	}
	return out, nil
}
