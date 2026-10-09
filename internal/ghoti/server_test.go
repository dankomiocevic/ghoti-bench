package ghoti

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for Ghoti: started as "<bin> run"
// it reads config.yaml from its working directory and behaves according to
// the "mode" file next to it.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "run" {
		fakeGhoti()
		return
	}
	os.Exit(m.Run())
}

var addrPattern = regexp.MustCompile(`(?m)^\s*addr: "([^"]+)"`)

func fakeGhoti() {
	cfg, _ := os.ReadFile("config.yaml")
	mode, _ := os.ReadFile("mode")
	signal.Ignore(syscall.SIGTERM)
	term := make(chan os.Signal, 1)
	if string(mode) != "ignore-term" {
		signal.Notify(term, syscall.SIGTERM)
	}
	// Metrics first, then TCP, like Ghoti. A taken port is fatal.
	addrs := addrPattern.FindAllStringSubmatch(string(cfg), -1)
	for i := len(addrs) - 1; i >= 0; i-- {
		ln, err := net.Listen("tcp", addrs[i][1])
		if err != nil {
			fmt.Println("Error starting server", err)
			os.Exit(4)
		}
		defer ln.Close()
	}
	switch string(mode) {
	case "crash":
		time.Sleep(500 * time.Millisecond)
		os.Exit(3)
	case "fail-on-term":
		<-term
		os.Exit(1)
	case "ignore-term":
		time.Sleep(time.Hour)
	default:
		<-term
	}
}

func testServer(t *testing.T, mode string) (*Binary, string) {
	t.Helper()
	dir := t.TempDir()
	if mode != "" {
		os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0o644)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Binary{Path: self}, dir
}

func TestStartOnFreePortsAndStop(t *testing.T) {
	bin, dir := testServer(t, "")
	s, err := StartOnFreePorts(context.Background(), bin, ServerConfig{Slots: []int{0}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config.Addr == "" || s.Config.MetricsAddr == "" {
		t.Fatalf("addresses not set: %+v", s.Config)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStartReportsAddrInUse(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	free, _ := FreeAddr()

	bin, dir := testServer(t, "")
	_, err = Start(context.Background(), bin, ServerConfig{Addr: taken.Addr().String(), MetricsAddr: free}, dir)
	if !errors.Is(err, ErrAddrInUse) {
		t.Fatalf("got %v", err)
	}
}

func TestStartOnFreePortsRetriesTakenPort(t *testing.T) {
	// Another process grabs the first port handed out, between the free
	// port check and Ghoti binding it.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	calls := 0
	freeAddr = func() (string, error) {
		calls++
		if calls == 1 {
			return taken.Addr().String(), nil
		}
		return FreeAddr()
	}
	defer func() { freeAddr = FreeAddr }()

	bin, dir := testServer(t, "")
	s, err := StartOnFreePorts(context.Background(), bin, ServerConfig{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if s.Config.Addr == taken.Addr().String() || calls != 4 {
		t.Fatalf("addr %s after %d port picks", s.Config.Addr, calls)
	}
}

func TestStopReportsUnhealthyServer(t *testing.T) {
	stopTimeout = 300 * time.Millisecond
	defer func() { stopTimeout = 10 * time.Second }()

	cases := map[string]string{
		"crash":        "exited before it was stopped",
		"fail-on-term": "did not exit cleanly",
		"ignore-term":  "was killed",
	}
	for mode, want := range cases {
		t.Run(mode, func(t *testing.T) {
			bin, dir := testServer(t, mode)
			s, err := StartOnFreePorts(context.Background(), bin, ServerConfig{}, dir)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "crash" {
				<-s.exited
			}
			err = s.Stop()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want %q", err, want)
			}
		})
	}
}
