package load

import (
	"errors"
	"fmt"
	"time"
)

// Config describes one closed-loop load run.
type Config struct {
	// Addr is the host:port of the Ghoti TCP listener.
	Addr string
	// Protocol is "standard" (requests end in \n) or "telnet" (\r\n).
	Protocol string
	// Connections is the number of persistent connections, each with one
	// request in flight at a time.
	Connections int
	// Workload is WorkloadSimpleRead or WorkloadSimpleMixed.
	Workload string
	// ReadPercent is the share of reads for WorkloadSimpleMixed (0-100).
	ReadPercent int
	// Slots is the range of simple memory slots the workload uses.
	Slots SlotRange
	// PayloadSize is the value size in bytes (1-36).
	PayloadSize int

	Warmup   time.Duration
	Duration time.Duration

	// RequestTimeout bounds how long a request may wait for its response
	// before it is counted as a timeout and the connection is reopened.
	RequestTimeout time.Duration
	// DialTimeout bounds each connection attempt.
	DialTimeout time.Duration
	// DialConcurrency limits the number of concurrent dials while opening
	// the connections, so a large run does not overflow the listen backlog.
	DialConcurrency int
	// Seed makes the slot and operation choices repeatable.
	Seed uint64
	// SkipPreload does not write the known values before the run. The slots
	// must already hold them or every read will be counted as incorrect.
	SkipPreload bool
	// SampleInterval is the timeline resolution, at least MinSampleInterval.
	// OnTick runs synchronously at every tick, so it must finish well
	// within the interval.
	SampleInterval time.Duration

	// OnTick, if set, is called from the sampler at every timeline tick, so
	// server-side stats can be collected on the same clock as the load.
	OnTick func(t time.Time, phase Phase)
}

// MinSampleInterval bounds the timeline resolution so the sampler, which
// scrapes server metrics synchronously, cannot dominate the run.
const MinSampleInterval = 100 * time.Millisecond

// Phase of the run at a given moment.
type Phase string

const (
	PhaseWarmup  Phase = "warmup"
	PhaseMeasure Phase = "measure"
)

// Defaults fills unset optional fields.
func (c *Config) Defaults() {
	if c.Protocol == "" {
		c.Protocol = "standard"
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 5 * time.Second
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.DialConcurrency == 0 {
		c.DialConcurrency = 64
	}
	if c.SampleInterval == 0 {
		c.SampleInterval = time.Second
	}
	if c.Workload == WorkloadSimpleRead {
		c.ReadPercent = 100
	}
}

// Validate reports the first invalid field. Call it after Defaults, which
// replaces zero values; any value left out of range is an error.
func (c *Config) Validate() error {
	switch {
	case c.Addr == "":
		return errors.New("addr is required")
	case c.Protocol != "standard" && c.Protocol != "telnet":
		return fmt.Errorf("protocol %q not supported, use standard or telnet", c.Protocol)
	case c.Connections < 1:
		return errors.New("connections must be at least 1")
	case c.Workload != WorkloadSimpleRead && c.Workload != WorkloadSimpleMixed:
		return fmt.Errorf("workload %q not supported, use %s or %s", c.Workload, WorkloadSimpleRead, WorkloadSimpleMixed)
	case c.ReadPercent < 0 || c.ReadPercent > 100:
		return errors.New("read-percent must be between 0 and 100")
	case c.PayloadSize < 1 || c.PayloadSize > MaxValueSize:
		return fmt.Errorf("payload-size must be between 1 and %d bytes", MaxValueSize)
	case c.Slots.Len() < 1:
		return errors.New("slots range is empty")
	case c.Warmup < 0:
		return errors.New("warmup must not be negative")
	case c.Duration <= 0:
		return errors.New("duration must be positive")
	case c.RequestTimeout <= 0:
		return errors.New("request-timeout must be positive")
	case c.DialTimeout <= 0:
		return errors.New("dial timeout must be positive")
	case c.DialConcurrency < 1:
		return errors.New("dial concurrency must be at least 1")
	case c.SampleInterval < MinSampleInterval:
		return fmt.Errorf("sample interval must be at least %s", MinSampleInterval)
	}
	return nil
}

func (c *Config) terminator() string {
	if c.Protocol == "telnet" {
		return "\r\n"
	}
	return "\n"
}

// RequestPattern describes the requests on the wire, for reports.
func (c *Config) RequestPattern() string {
	term := `\n`
	if c.Protocol == "telnet" {
		term = `\r\n`
	}
	read := fmt.Sprintf("r%03d%s", c.Slots.First, term)
	if c.Slots.Len() > 1 {
		read = fmt.Sprintf("r{%s}%s", c.Slots, term)
	}
	if c.ReadPercent == 100 {
		return read
	}
	write := fmt.Sprintf("w%03d{value}%s", c.Slots.First, term)
	if c.Slots.Len() > 1 {
		write = fmt.Sprintf("w{%s}{value}%s", c.Slots, term)
	}
	if c.ReadPercent == 0 {
		return write
	}
	return fmt.Sprintf("%d%% %s | %d%% %s", c.ReadPercent, read, 100-c.ReadPercent, write)
}

// ResponseWireBytes is the size of a value response including its prefix
// and terminator.
func (c *Config) ResponseWireBytes() int { return 1 + 3 + c.PayloadSize + 1 }
