// Package scenario defines the named benchmarks ghoti-bench can run.
package scenario

import (
	"fmt"
	"sort"
	"strings"

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

// SlotList expands the scenario slots for the server configuration.
func (s Scenario) SlotList() []int {
	slots := make([]int, 0, s.Slots.Len())
	for i := s.Slots.First; i <= s.Slots.Last; i++ {
		slots = append(slots, i)
	}
	return slots
}
