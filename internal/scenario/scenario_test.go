package scenario

import (
	"testing"

	"github.com/dankomiocevic/ghoti-bench/internal/load"
)

func TestAllServerSlots(t *testing.T) {
	// Every scenario has to be runnable on one server, so no two scenarios
	// may need a different configuration for the same slot.
	slots, err := AllServerSlots()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(slots); i++ {
		if slots[i].Number <= slots[i-1].Number {
			t.Fatalf("slots not sorted or repeated at %03d", slots[i].Number)
		}
	}
	for _, name := range Names() {
		sc, _ := Get(name)
		cfg := load.Config{Addr: "x", Connections: 1, Workload: sc.Workload, Slots: sc.Slots,
			PayloadSize: sc.PayloadSize, Bucket: sc.Bucket, Duration: 1}
		cfg.Defaults()
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
