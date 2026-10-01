package load

import (
	"fmt"
	"strconv"
	"strings"
)

// Workload names accepted by Config.Workload.
const (
	// WorkloadSimpleRead only reads simple memory slots.
	WorkloadSimpleRead = "simple-read"
	// WorkloadSimpleMixed reads and writes simple memory slots.
	WorkloadSimpleMixed = "simple-mixed"
)

// MaxValueSize is the largest value a Ghoti slot can hold, in bytes.
const MaxValueSize = 36

// SlotRange is an inclusive range of slot numbers.
type SlotRange struct {
	First int
	Last  int
}

// ParseSlotRange parses "000-099" or a single slot such as "000".
func ParseSlotRange(s string) (SlotRange, error) {
	first, last, found := strings.Cut(s, "-")
	if !found {
		last = first
	}
	a, err := parseSlot(first)
	if err != nil {
		return SlotRange{}, err
	}
	b, err := parseSlot(last)
	if err != nil {
		return SlotRange{}, err
	}
	if b < a {
		return SlotRange{}, fmt.Errorf("slot range %q is reversed", s)
	}
	return SlotRange{First: a, Last: b}, nil
}

func parseSlot(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 || n > 999 {
		return 0, fmt.Errorf("invalid slot %q, must be 000-999", s)
	}
	return n, nil
}

// Len returns the number of slots in the range.
func (r SlotRange) Len() int { return r.Last - r.First + 1 }

func (r SlotRange) String() string { return fmt.Sprintf("%03d-%03d", r.First, r.Last) }

// SlotValue returns the deterministic value stored in a slot by the
// benchmark. It is printable ASCII, never contains a newline, and starts with
// the slot number so a response for the wrong slot can never match.
func SlotValue(slot, size int) []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	v := make([]byte, size)
	prefix := fmt.Sprintf("%03d", slot)
	for i := range v {
		if i < len(prefix) {
			v[i] = prefix[i]
			continue
		}
		v[i] = alphabet[(i*7+slot)%len(alphabet)]
	}
	return v
}

// op is one prebuilt request with the exact response it must produce. Both
// are built once before the run so the hot loop never allocates.
type op struct {
	request  []byte
	response []byte // including the trailing '\n'
	write    bool
}

// ops holds the prebuilt requests for every slot of the workload.
type ops struct {
	reads  []op
	writes []op
}

// buildOps prebuilds every request and its expected response.
//
// Every write to a slot stores the same value the slot was preloaded with,
// so each response is known exactly: reads can be validated byte by byte even
// while other connections are writing, and the server still performs the
// whole write path.
func buildOps(slots SlotRange, payloadSize int, terminator string) ops {
	var o ops
	for s := slots.First; s <= slots.Last; s++ {
		value := SlotValue(s, payloadSize)
		response := fmt.Appendf(nil, "v%03d%s\n", s, value)
		o.reads = append(o.reads, op{
			request:  fmt.Appendf(nil, "r%03d%s", s, terminator),
			response: response,
		})
		o.writes = append(o.writes, op{
			request:  fmt.Appendf(nil, "w%03d%s%s", s, value, terminator),
			response: response,
			write:    true,
		})
	}
	return o
}
