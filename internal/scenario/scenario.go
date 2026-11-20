// Package scenario defines the named benchmarks ghoti-bench can run.
package scenario

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dankomiocevic/ghoti-bench/internal/ghoti"
	"github.com/dankomiocevic/ghoti-bench/internal/load"
)

// Scenario is a workload plus the slots the server must provide for it.
type Scenario struct {
	Name        string
	Description string
	Workload    string
	ReadPercent int
	Slots       load.SlotRange
	PayloadSize int
	// Bucket is the configuration of the token bucket slots.
	Bucket load.TokenBucket
}

var all = map[string]Scenario{
	"memory-read": {
		Name: "memory-read",
		Description: "Reads a preloaded 36-byte value from simple memory slot 000 and validates it. " +
			"Measures protocol and connection overhead without mutation.",
		Workload:    load.WorkloadSimpleRead,
		ReadPercent: 100,
		Slots:       load.SlotRange{First: 0, Last: 0},
		PayloadSize: 36,
	},
	"memory-mixed": {
		Name:        "memory-mixed",
		Description: "95% reads and 5% writes of 36-byte values over simple memory slots 000-099.",
		Workload:    load.WorkloadSimpleMixed,
		ReadPercent: 95,
		Slots:       load.SlotRange{First: 0, Last: 99},
		PayloadSize: 36,
	},
	// The counters use slots 100-199 so one server configured with simple
	// memory slots 000-099 and atomic slots 100-199 can run every scenario.
	"counter-hot": {
		Name: "counter-hot",
		Description: "Every connection increments atomic counter slot 100 and validates the value. " +
			"Measures the read path when all connections contend for the same slot.",
		Workload:    load.WorkloadCounter,
		ReadPercent: 100,
		Slots:       load.SlotRange{First: 100, Last: 100},
	},
	"counter-spread": {
		Name:        "counter-spread",
		Description: "Increments atomic counter slots 100-199, slot chosen uniformly, and validates every value.",
		Workload:    load.WorkloadCounter,
		ReadPercent: 100,
		Slots:       load.SlotRange{First: 100, Last: 199},
	},
	// Every bucket holds two periods of tokens, so an uneven spread of the
	// requests does not throw refills away. The rates are low enough for a
	// single connection to keep every bucket drained on EC2 (about 3,000
	// requests per second), which the lower bound of the check needs.
	"token-bucket-hot": {
		Name: "token-bucket-hot",
		Description: "Every connection takes tokens from token bucket slot 200 (1,000 tokens per second) and " +
			"checks the tokens granted. Measures the rate limiter when all connections contend for the same slot.",
		Workload:    load.WorkloadTokenBucket,
		ReadPercent: 100,
		Slots:       load.SlotRange{First: 200, Last: 200},
		Bucket:      load.TokenBucket{Size: 2000, RefreshRate: 1000, TokensPerReq: 1, Period: "second"},
	},
	"token-bucket-spread": {
		Name:        "token-bucket-spread",
		Description: "Takes tokens from token bucket slots 300-399 (10 tokens per second each), slot chosen uniformly.",
		Workload:    load.WorkloadTokenBucket,
		ReadPercent: 100,
		Slots:       load.SlotRange{First: 300, Last: 399},
		Bucket:      load.TokenBucket{Size: 20, RefreshRate: 10, TokensPerReq: 1, Period: "second"},
	},
}

// Get returns a scenario by name.
func Get(name string) (Scenario, error) {
	s, ok := all[name]
	if !ok {
		return Scenario{}, fmt.Errorf("unknown scenario %q, available: %s", name, strings.Join(Names(), ", "))
	}
	return s, nil
}

// Names lists the available scenarios.
func Names() []string {
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ServerSlots returns the slots the server needs for the scenario.
func (s Scenario) ServerSlots() []ghoti.Slot {
	kind := load.SlotKind(s.Workload)
	var options []ghoti.Option
	if s.Workload == load.WorkloadTokenBucket {
		options = []ghoti.Option{
			{Key: "bucket_size", Value: strconv.Itoa(s.Bucket.Size)},
			{Key: "refresh_rate", Value: strconv.Itoa(s.Bucket.RefreshRate)},
			{Key: "tokens_per_req", Value: strconv.Itoa(s.Bucket.TokensPerReq)},
			{Key: "period", Value: s.Bucket.Period},
		}
	}
	slots := make([]ghoti.Slot, 0, s.Slots.Len())
	for i := s.Slots.First; i <= s.Slots.Last; i++ {
		slots = append(slots, ghoti.Slot{Number: i, Kind: kind, Options: options})
	}
	return slots
}

// AllServerSlots returns the slots of every scenario, for a server that has
// to run all of them. Scenarios may share a slot only when they need the
// same configuration for it.
func AllServerSlots() ([]ghoti.Slot, error) {
	bySlot := map[int]ghoti.Slot{}
	owner := map[int]string{}
	for _, name := range Names() {
		for _, slot := range all[name].ServerSlots() {
			if prev, ok := bySlot[slot.Number]; ok {
				if !slices.Equal(prev.Options, slot.Options) || prev.Kind != slot.Kind {
					return nil, fmt.Errorf("slot %03d is configured differently by %s and %s", slot.Number, owner[slot.Number], name)
				}
				continue
			}
			bySlot[slot.Number] = slot
			owner[slot.Number] = name
		}
	}
	slots := make([]ghoti.Slot, 0, len(bySlot))
	for _, s := range bySlot {
		slots = append(slots, s)
	}
	slices.SortFunc(slots, func(a, b ghoti.Slot) int { return a.Number - b.Number })
	return slots, nil
}
