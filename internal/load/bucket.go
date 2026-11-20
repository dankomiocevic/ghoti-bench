package load

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"time"
)

// TokenBucket is the configuration of the token bucket slots. It has to
// match the server, the bounds of the end of run check are computed from it.
type TokenBucket struct {
	// Size is the most tokens the bucket can hold (bucket_size).
	Size int
	// RefreshRate is the number of tokens added every period (refresh_rate).
	RefreshRate int
	// TokensPerReq is the most tokens one read takes (tokens_per_req).
	TokensPerReq int
	// Period is second, minute or hour.
	Period string
}

// PeriodDuration returns the refill period, zero if Period is invalid.
func (b TokenBucket) PeriodDuration() time.Duration {
	switch b.Period {
	case "second":
		return time.Second
	case "minute":
		return time.Minute
	case "hour":
		return time.Hour
	}
	return 0
}

func (b TokenBucket) validate() error {
	switch {
	case b.Size < 1:
		return errors.New("token bucket size must be at least 1")
	case b.RefreshRate < 1 || b.RefreshRate > b.Size:
		return errors.New("token bucket refresh rate must be between 1 and the size")
	case b.TokensPerReq < 1 || b.TokensPerReq > b.Size:
		return errors.New("token bucket tokens per request must be between 1 and the size")
	case b.PeriodDuration() == 0:
		return fmt.Errorf("token bucket period %q not supported, use second, minute or hour", b.Period)
	}
	return nil
}

// BucketCheck compares the tokens every bucket granted during the run with
// what its configuration allows. Ghoti refills a bucket when a read finds that
// a new period started, aligned to the server clock, so over a run of T:
//
//   - a bucket can never grant more than it held at the start (at most Size)
//     plus RefreshRate for every period boundary in the run, at most
//     ceil(T/period) of them,
//   - and, while the load keeps it drained, it has to grant every refill but
//     the ones at the edges of the run, at least RefreshRate for
//     floor(T/period)-2 boundaries.
//
// The server and generator clocks are not the same, which is why the bounds
// count boundaries in the length of the run instead of comparing times.
type BucketCheck struct {
	// Seconds is the span of the run, from the first request sent to the
	// last response received, warm-up included.
	Seconds float64 `json:"seconds"`
	// Requests and Tokens are the validated reads and the tokens they were
	// granted over the whole run, every slot together.
	Requests uint64 `json:"requests"`
	Tokens   uint64 `json:"tokens"`
	// MinTokens and MaxTokens are the bounds for every slot.
	MinTokens uint64 `json:"minTokensPerSlot"`
	MaxTokens uint64 `json:"maxTokensPerSlot"`
	// LowerBoundSkipped lists the slots that did not get enough requests to
	// be kept drained, so only the upper bound applies to them.
	LowerBoundSkipped []string `json:"lowerBoundSkipped,omitempty"`
	// Mismatches describes every slot outside its bounds.
	Mismatches []string `json:"mismatches,omitempty"`
}

// Failed reports whether the check found a problem.
func (c *BucketCheck) Failed() bool { return len(c.Mismatches) > 0 }

// checkBucket validates a token bucket response and returns the tokens it
// granted. Any value from 0 (denied) to TokensPerReq is a valid answer,
// whether it was right is only known at the end of the run, from the totals.
func (w *worker) checkBucket(slot int, prefix, resp []byte) (uint64, bool) {
	v, ok := parseNumber(resp, prefix)
	if !ok || v > int64(w.cfg.Bucket.TokensPerReq) {
		return 0, false
	}
	w.bucketReqs[slot]++
	w.bucketTokens[slot] += uint64(v)
	return uint64(v), true
}

// checkBuckets applies the bounds of BucketCheck to every slot.
func checkBuckets(cfg *Config, workers []*worker, span time.Duration) *BucketCheck {
	n := cfg.Slots.Len()
	reqs := make([]uint64, n)
	tokens := make([]uint64, n)
	check := &BucketCheck{Seconds: round(span.Seconds(), 3)}
	for _, w := range workers {
		for s := range n {
			reqs[s] += w.bucketReqs[s]
			tokens[s] += w.bucketTokens[s]
			check.Requests += w.bucketReqs[s]
			check.Tokens += w.bucketTokens[s]
		}
	}

	b := cfg.Bucket
	periods := span.Seconds() / b.PeriodDuration().Seconds()
	rate := uint64(b.RefreshRate)
	check.MaxTokens = uint64(b.Size) + rate*uint64(math.Ceil(periods))
	if full := math.Floor(periods) - 2; full > 0 {
		check.MinTokens = rate * uint64(full)
	}
	// To keep a bucket drained every refill has to be met by more reads than
	// it can grant. Twice as many leaves room for the uneven spread of the
	// requests over the slots.
	needed := uint64(math.Ceil(2 * periods * float64(b.RefreshRate) / float64(b.TokensPerReq)))

	for s := range n {
		slot := cfg.Slots.First + s
		switch {
		case tokens[s] > check.MaxTokens:
			check.Mismatches = append(check.Mismatches,
				fmt.Sprintf("slot %03d granted %d tokens in %.1fs, at most %d allowed", slot, tokens[s], span.Seconds(), check.MaxTokens))
		case reqs[s] < needed:
			check.LowerBoundSkipped = append(check.LowerBoundSkipped,
				fmt.Sprintf("slot %03d: %d requests, %d needed", slot, reqs[s], needed))
		case tokens[s] < check.MinTokens:
			check.Mismatches = append(check.Mismatches,
				fmt.Sprintf("slot %03d granted %d tokens in %.1fs, at least %d expected", slot, tokens[s], span.Seconds(), check.MinTokens))
		}
	}
	return check
}

// probeBuckets reads every token bucket once before the run, so a slot of the
// wrong kind fails the run before it starts. The read can take a token, which
// the upper bound already allows for: a bucket never holds more than Size.
func probeBuckets(cfg *Config, plan *ops) error {
	conn, err := dial(cfg.Addr, cfg.DialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(cfg.RequestTimeout + time.Duration(len(plan.reads))*100*time.Millisecond))

	r := bufio.NewReader(conn)
	for _, o := range plan.reads {
		if _, err := conn.Write(o.request); err != nil {
			return err
		}
		line, err := readResponse(r)
		if err != nil {
			return err
		}
		if v, ok := parseNumber(line, o.response); !ok || v > int64(cfg.Bucket.TokensPerReq) {
			return fmt.Errorf("request %q answered %q, expected 0 to %d tokens; is the slot a %s slot with tokens_per_req %d?",
				bytes.TrimSpace(o.request), bytes.TrimSpace(line), cfg.Bucket.TokensPerReq, SlotKind(cfg.Workload), cfg.Bucket.TokensPerReq)
		}
	}
	return nil
}
