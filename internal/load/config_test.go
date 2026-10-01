package load

import (
	"strings"
	"testing"
	"time"
)

func TestValidateRejectsOperationalValues(t *testing.T) {
	cases := map[string]func(*Config){
		"dial concurrency": func(c *Config) { c.DialConcurrency = -1 },
		"dial timeout":     func(c *Config) { c.DialTimeout = -time.Second },
		"sample interval":  func(c *Config) { c.SampleInterval = -time.Second },
		"sample interval ": func(c *Config) { c.SampleInterval = time.Millisecond },
		"request-timeout":  func(c *Config) { c.RequestTimeout = -time.Second },
		"warmup":           func(c *Config) { c.Warmup = -time.Second },
	}
	for want, mutate := range cases {
		cfg := testConfig("127.0.0.1:1")
		mutate(&cfg)
		cfg.Defaults()
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%s: got %v", want, err)
		}
	}

	// Zero values mean "use the default" and stay valid.
	cfg := testConfig("127.0.0.1:1")
	cfg.DialConcurrency, cfg.DialTimeout, cfg.SampleInterval = 0, 0, 0
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
