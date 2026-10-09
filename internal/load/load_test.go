package load

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGhoti is a minimal simple-memory server speaking the standard
// protocol. corrupt makes every n-th read return a wrong value.
//
// With counters every slot is an atomic counter instead: a read increments
// it, doubleEvery increments by two on every n-th read and staleEvery
// answers every n-th read with the value before the increment.
type fakeGhoti struct {
	ln      net.Listener
	mu      sync.Mutex
	slots   map[string]string
	reads   atomic.Int64
	corrupt int64
	errorOn string // slot answered with an error response

	counters    bool
	values      map[string]int64
	doubleEvery int64
	staleEvery  int64
}

func startFake(t *testing.T) *fakeGhoti {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGhoti{ln: ln, slots: map[string]string{}, values: map[string]int64{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeGhoti) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if len(line) < 4 {
			fmt.Fprint(c, "exxx001\n")
			continue
		}
		slot := line[1:4]
		if slot == f.errorOn {
			fmt.Fprintf(c, "e%s004\n", slot)
			continue
		}
		switch {
		case f.counters && line[0] == 'w':
			v, err := strconv.ParseInt(line[4:], 10, 64)
			if err != nil || v < 0 {
				fmt.Fprintf(c, "e%s007\n", slot)
				continue
			}
			f.mu.Lock()
			f.values[slot] = v
			f.mu.Unlock()
			fmt.Fprintf(c, "v%s%d\n", slot, v)
		case f.counters && line[0] == 'r':
			n := f.reads.Add(1)
			f.mu.Lock()
			f.values[slot]++
			if f.doubleEvery > 0 && n%f.doubleEvery == 0 {
				f.values[slot]++
			}
			v := f.values[slot]
			f.mu.Unlock()
			if f.staleEvery > 0 && n%f.staleEvery == 0 {
				v--
			}
			fmt.Fprintf(c, "v%s%d\n", slot, v)
		case line[0] == 'w':
			f.mu.Lock()
			f.slots[slot] = line[4:]
			f.mu.Unlock()
			fmt.Fprintf(c, "v%s%s\n", slot, line[4:])
		case line[0] == 'r':
			f.mu.Lock()
			v := f.slots[slot]
			f.mu.Unlock()
			n := f.reads.Add(1)
			if f.corrupt > 0 && n%f.corrupt == 0 {
				v = "wrong"
			}
			// An async event before the value must be skipped by clients.
			if n%7 == 0 {
				fmt.Fprintf(c, "a999event\n")
			}
			fmt.Fprintf(c, "v%s%s\n", slot, v)
		default:
			fmt.Fprint(c, "exxx001\n")
		}
	}
}

func testConfig(addr string) Config {
	return Config{
		Addr:           addr,
		Connections:    4,
		Workload:       WorkloadSimpleRead,
		Slots:          SlotRange{0, 0},
		PayloadSize:    36,
		Warmup:         200 * time.Millisecond,
		Duration:       time.Second,
		SampleInterval: 100 * time.Millisecond,
		RequestTimeout: time.Second,
	}
}

func TestRunValidatesReads(t *testing.T) {
	f := startFake(t)
	var ticks atomic.Int64
	cfg := testConfig(f.ln.Addr().String())
	cfg.OnTick = func(time.Time, Phase) { ticks.Add(1) }

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests == 0 || res.Latency.TotalCount() != int64(res.Requests) {
		t.Fatalf("requests %d, histogram count %d", res.Requests, res.Latency.TotalCount())
	}
	if res.Reads != res.Requests || res.Writes != 0 {
		t.Fatalf("reads %d writes %d requests %d", res.Reads, res.Writes, res.Requests)
	}
	if total := res.Errors.Total() + res.WarmupErrors.Total(); total != 0 {
		t.Fatalf("unexpected errors %+v %+v", res.Errors, res.WarmupErrors)
	}
	if res.AsyncEvents == 0 {
		t.Fatal("async events were not seen")
	}
	if res.MeasuredSeconds != 1 {
		t.Fatalf("measured %v seconds", res.MeasuredSeconds)
	}
	// 0.2s warm-up + 1s measurement at 100ms gives 13 aligned ticks.
	if n := ticks.Load(); n != 13 {
		t.Fatalf("got %d ticks", n)
	}
	measure := 0
	for _, s := range res.Timeline {
		if s.Phase == PhaseMeasure {
			measure++
		}
	}
	if measure != 10 {
		t.Fatalf("got %d measurement samples", measure)
	}
}

func TestRunCountsIncorrectAndErrorResponses(t *testing.T) {
	f := startFake(t)
	f.corrupt = 50
	cfg := testConfig(f.ln.Addr().String())
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors.Incorrect == 0 {
		t.Fatal("corrupted responses were not detected")
	}

	f2 := startFake(t)
	cfg = testConfig(f2.ln.Addr().String())
	cfg.Workload = WorkloadSimpleMixed
	cfg.ReadPercent = 50
	cfg.Slots = SlotRange{0, 9}
	cfg.SkipPreload = true
	f2.errorOn = "005"
	// Without preload unwritten slots read back empty, which is incorrect.
	res, err = Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors.ErrorResponses == 0 {
		t.Fatal("error responses were not counted")
	}
	if res.Writes == 0 || res.Reads == 0 {
		t.Fatalf("mixed workload did not mix: reads %d writes %d", res.Reads, res.Writes)
	}
}

func TestRunTelnetTerminator(t *testing.T) {
	f := startFake(t)
	cfg := testConfig(f.ln.Addr().String())
	cfg.Protocol = "telnet"
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests == 0 || res.Errors.Total() != 0 {
		t.Fatalf("requests %d errors %+v", res.Requests, res.Errors)
	}
}

func TestRunCountsTimeouts(t *testing.T) {
	// A server that accepts writes for preload but never answers reads.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if line[0] == 'w' {
						fmt.Fprintf(c, "v%s", line[1:])
					}
				}
			}()
		}
	}()
	cfg := testConfig(ln.Addr().String())
	cfg.Warmup = 0
	cfg.RequestTimeout = 200 * time.Millisecond
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors.Timeouts == 0 || res.Requests != 0 {
		t.Fatalf("timeouts %d requests %d", res.Errors.Timeouts, res.Requests)
	}
	if res.Reconnects == 0 {
		t.Fatal("connections were not reopened after timeouts")
	}
}

func TestPreloadRejectsWrongSlotKind(t *testing.T) {
	f := startFake(t)
	f.errorOn = "000"
	_, err := Run(context.Background(), testConfig(f.ln.Addr().String()))
	if err == nil || !strings.Contains(err.Error(), "simple_memory") {
		t.Fatalf("got %v", err)
	}
}

func TestInterrupt(t *testing.T) {
	f := startFake(t)
	cfg := testConfig(f.ln.Addr().String())
	cfg.Duration = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	res, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Interrupted || res.MeasuredSeconds > 1 || res.MeasuredSeconds <= 0 {
		t.Fatalf("interrupted %v measured %v", res.Interrupted, res.MeasuredSeconds)
	}
}

func TestParseSlotRange(t *testing.T) {
	cases := map[string]SlotRange{"000-099": {0, 99}, "5": {5, 5}, "999": {999, 999}}
	for in, want := range cases {
		got, err := ParseSlotRange(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v %v", in, got, err)
		}
	}
	for _, in := range []string{"099-000", "1000", "-1", "a-b", ""} {
		if _, err := ParseSlotRange(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestSlotValue(t *testing.T) {
	v := SlotValue(42, 36)
	if len(v) != 36 || string(v[:3]) != "042" || strings.ContainsAny(string(v), "\r\n") {
		t.Fatalf("bad value %q", v)
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{0, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	for n, d := range want {
		if got := backoff(n); got != d {
			t.Errorf("backoff(%d) = %s, want %s", n, got, d)
		}
	}
	if got := backoff(100); got != time.Second {
		t.Errorf("backoff(100) = %s", got)
	}
}

func TestReconnectBacksOff(t *testing.T) {
	// A server that answers the preload, then closes every new connection
	// as soon as it receives a request. Without backoff the workers would
	// reconnect thousands of times a second.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n := accepted.Add(1)
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil || n > 1 && line[0] == 'r' {
						return
					}
					fmt.Fprintf(c, "v%s", line[1:])
				}
			}()
		}
	}()

	cfg := testConfig(ln.Addr().String())
	cfg.Connections = 2
	cfg.Warmup = 0
	cfg.Duration = time.Second
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors.ConnectionErrors == 0 || res.Reconnects == 0 {
		t.Fatalf("errors %+v reconnects %d", res.Errors, res.Reconnects)
	}
	// 10+20+...+640ms is about 1.3s, so each worker fits under 10 attempts.
	if res.Reconnects > 20 {
		t.Fatalf("%d reconnects in 1s, backoff is not applied", res.Reconnects)
	}
}

func counterConfig(f *fakeGhoti) Config {
	f.counters = true
	cfg := testConfig(f.ln.Addr().String())
	cfg.Workload = WorkloadCounter
	cfg.Slots = SlotRange{100, 109}
	cfg.PayloadSize = 0
	return cfg
}

func TestRunCounter(t *testing.T) {
	f := startFake(t)
	// Start from a value the preload has to reset.
	f.values["100"] = 500
	res, err := Run(context.Background(), counterConfig(f))
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests == 0 || res.Errors.Total()+res.WarmupErrors.Total() != 0 {
		t.Fatalf("requests %d errors %+v %+v", res.Requests, res.Errors, res.WarmupErrors)
	}
	c := res.CounterCheck
	if c == nil || c.Failed() {
		t.Fatalf("counter check %+v", c)
	}
	// The final check reads every slot once more.
	if c.Increments < res.Requests || c.Increments+10 != uint64(f.reads.Load()) {
		t.Fatalf("increments %d, requests %d, server reads %d", c.Increments, res.Requests, f.reads.Load())
	}
}

func TestRunCounterDetectsDoubleIncrements(t *testing.T) {
	f := startFake(t)
	cfg := counterConfig(f)
	f.doubleEvery = 100
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The values still grow, only the final check can see it.
	if res.Errors.Total() != 0 {
		t.Fatalf("errors %+v", res.Errors)
	}
	if c := res.CounterCheck; c == nil || len(c.Mismatches) == 0 {
		t.Fatalf("counter check %+v", c)
	}
}

func TestRunCounterRejectsValuesThatDoNotGrow(t *testing.T) {
	f := startFake(t)
	cfg := counterConfig(f)
	cfg.Slots = SlotRange{100, 100}
	cfg.Connections = 1
	f.staleEvery = 50
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors.Incorrect+res.WarmupErrors.Incorrect == 0 {
		t.Fatal("a value that did not grow was accepted")
	}
	if res.CounterCheck != nil {
		t.Fatal("counters were checked after errors")
	}
}

func TestPreloadRejectsSimpleMemoryForCounters(t *testing.T) {
	f := startFake(t)
	cfg := counterConfig(f)
	f.counters = false
	f.errorOn = "100"
	_, err := Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "atomic") {
		t.Fatalf("got %v", err)
	}
}

func TestParseCounter(t *testing.T) {
	prefix := []byte("v100")
	good := map[string]int64{"v1000\n": 0, "v10042\n": 42, "v1009223372036854775807\n": 9223372036854775807}
	for in, want := range good {
		if got, ok := parseCounter([]byte(in), prefix); !ok || got != want {
			t.Errorf("%q: got %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"v100\n", "v1001", "v1011\n", "v100-1\n", "v1001a\n", "v1009223372036854775808\n", "e100004\n"} {
		if _, ok := parseCounter([]byte(in), prefix); ok {
			t.Errorf("%q: expected rejection", in)
		}
	}
}
