package load

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
)

// ErrorCounts classifies everything that went wrong during a phase.
type ErrorCounts struct {
	// Timeouts are requests without a complete response within the timeout.
	Timeouts uint64 `json:"timeouts"`
	// ConnectionErrors are reads or writes that failed, for example because
	// the server closed the connection.
	ConnectionErrors uint64 `json:"connectionErrors"`
	// ErrorResponses are "e" responses from Ghoti.
	ErrorResponses uint64 `json:"errorResponses"`
	// Incorrect are responses that do not match the expected bytes exactly.
	Incorrect uint64 `json:"incorrectResponses"`
	// DialFailures are failed attempts to reopen a connection.
	DialFailures uint64 `json:"dialFailures"`
}

// Total returns the sum of all error kinds.
func (e ErrorCounts) Total() uint64 {
	return e.Timeouts + e.ConnectionErrors + e.ErrorResponses + e.Incorrect + e.DialFailures
}

func (e *ErrorCounts) add(o ErrorCounts) {
	e.Timeouts += o.Timeouts
	e.ConnectionErrors += o.ConnectionErrors
	e.ErrorResponses += o.ErrorResponses
	e.Incorrect += o.Incorrect
	e.DialFailures += o.DialFailures
}

// worker drives one persistent connection in a closed loop: it sends one
// request, waits for the complete response, validates it and only then sends
// the next one.
type worker struct {
	// ops and bytes are the only fields shared while the run is going on.
	// They are read by the sampler for the timeline, so they are atomic and
	// padded to keep neighbouring workers off the same cache line.
	ops   atomic.Uint64
	bytes atomic.Uint64
	_     [48]byte

	cfg    *Config
	plan   *ops
	rng    *rand.Rand
	conn   net.Conn
	reader *bufio.Reader

	measureStart time.Time
	measureEnd   time.Time
	stop         *atomic.Bool

	// Owned by the worker until it exits, read by the runner afterwards.
	hist          *hdrhistogram.Histogram
	maxLatency    time.Duration
	measuredOps   uint64
	measuredRead  uint64
	measuredWrite uint64
	warmupErrors  ErrorCounts
	measureErrors ErrorCounts
	lateErrors    ErrorCounts
	reconnects    uint64
	// failures counts consecutive connection failures (failed requests and
	// failed dials) since the last validated response, for the backoff.
	failures    int
	asyncEvents uint64
}

func newHistogram(timeout time.Duration) *hdrhistogram.Histogram {
	// Latencies are recorded in nanoseconds with 3 significant digits. The
	// lowest discernible value of 1µs keeps the per-worker footprint small;
	// round trips are never sub-microsecond.
	return hdrhistogram.New(1000, timeout.Nanoseconds(), 3)
}

func (w *worker) errorsAt(t time.Time) *ErrorCounts {
	switch {
	case t.Before(w.measureStart):
		return &w.warmupErrors
	case t.After(w.measureEnd):
		return &w.lateErrors
	default:
		return &w.measureErrors
	}
}

func (w *worker) run() {
	defer func() {
		if w.conn != nil {
			w.conn.Close()
		}
	}()

	reads, writes := w.plan.reads, w.plan.writes
	nSlots := len(reads)
	readPercent := w.cfg.ReadPercent
	timeout := w.cfg.RequestTimeout
	var refreshDeadlineAt time.Time

	for !w.stop.Load() {
		if w.conn == nil {
			if !w.reconnect() {
				return
			}
			refreshDeadlineAt = time.Time{}
		}

		slot := 0
		if nSlots > 1 {
			slot = w.rng.IntN(nSlots)
		}
		o := &reads[slot]
		if readPercent < 100 && w.rng.IntN(100) >= readPercent {
			o = &writes[slot]
		}

		start := time.Now()
		// Setting a deadline on every request costs a timer update, so it is
		// only pushed forward once half of it is used: every request still
		// gets between timeout/2 and timeout to complete.
		if start.After(refreshDeadlineAt) {
			w.conn.SetDeadline(start.Add(timeout))
			refreshDeadlineAt = start.Add(timeout / 2)
		}

		resp, err := w.roundTrip(o.request)
		end := time.Now()
		if err != nil {
			w.countNetError(end, err)
			w.conn.Close()
			w.conn = nil
			w.failures++
			continue
		}

		switch {
		case bytes.Equal(resp, o.response):
		case len(resp) > 0 && resp[0] == 'e':
			w.errorsAt(end).ErrorResponses++
			continue
		default:
			w.errorsAt(end).Incorrect++
			continue
		}

		w.failures = 0
		w.ops.Add(1)
		w.bytes.Add(uint64(len(o.request) + len(resp)))

		if !start.Before(w.measureStart) && !end.After(w.measureEnd) {
			lat := end.Sub(start)
			if lat > w.maxLatency {
				w.maxLatency = lat
			}
			if w.hist.RecordValue(lat.Nanoseconds()) != nil {
				w.hist.RecordValue(w.hist.HighestTrackableValue())
			}
			w.measuredOps++
			if o.write {
				w.measuredWrite++
			} else {
				w.measuredRead++
			}
		}
		if !end.Before(w.measureEnd) {
			return
		}
	}
}

// roundTrip writes one request and returns the next response line, skipping
// async events. The returned slice is only valid until the next read.
func (w *worker) roundTrip(req []byte) ([]byte, error) {
	if _, err := w.conn.Write(req); err != nil {
		return nil, err
	}
	for {
		line, err := w.reader.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				// Not a Ghoti response; the stream cannot be trusted anymore.
				return nil, errResponseTooLong
			}
			return nil, err
		}
		if len(line) > 0 && line[0] == 'a' {
			w.asyncEvents++
			continue
		}
		return line, nil
	}
}

var errResponseTooLong = errors.New("response line too long")

func (w *worker) countNetError(t time.Time, err error) {
	counts := w.errorsAt(t)
	var netErr net.Error
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		counts.Timeouts++
	case errors.Is(err, errResponseTooLong):
		counts.Incorrect++
	case errors.Is(err, io.EOF):
		counts.ConnectionErrors++
	default:
		counts.ConnectionErrors++
	}
}

// reconnect opens a new connection after a failure, retrying until the run
// ends. It returns false when the run is over.
//
// Every attempt waits for an exponential backoff first, so a server that
// accepts connections but fails every request is not hammered with
// reconnects and the error counts do not explode. The backoff grows with
// consecutive failures, including failed dials, and resets on the first
// validated response.
func (w *worker) reconnect() bool {
	for !w.stop.Load() {
		if !w.sleepBackoff() {
			return false
		}
		conn, err := dial(w.cfg.Addr, w.cfg.DialTimeout)
		if err == nil {
			w.attach(conn)
			w.reconnects++
			return true
		}
		w.errorsAt(time.Now()).DialFailures++
		w.failures++
	}
	return false
}

const (
	minBackoff = 10 * time.Millisecond
	maxBackoff = time.Second
)

// backoff returns the wait before the next attempt after n consecutive
// failures: 10ms, 20ms, 40ms... capped at 1s.
func backoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := minBackoff
	for i := 1; i < n && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// sleepBackoff waits for the current backoff, never past the end of the
// run. It returns false when the run is over.
func (w *worker) sleepBackoff() bool {
	d := backoff(w.failures)
	if remaining := time.Until(w.measureEnd); remaining <= 0 {
		return false
	} else if d > remaining {
		d = remaining
	}
	time.Sleep(d)
	return !w.stop.Load() && time.Now().Before(w.measureEnd)
}

func (w *worker) attach(conn net.Conn) {
	w.conn = conn
	if w.reader == nil {
		w.reader = bufio.NewReaderSize(conn, 4096)
	} else {
		w.reader.Reset(conn)
	}
}

func dial(addr string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		// Requests are tiny and strictly sequential; never wait to coalesce.
		tcp.SetNoDelay(true)
	}
	return conn, nil
}
