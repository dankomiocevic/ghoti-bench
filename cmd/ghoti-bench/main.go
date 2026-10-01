// Command ghoti-bench runs a benchmark matrix against a Ghoti server it
// obtains itself (a release, a commit built from source or a local binary)
// or against an external server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/dankomiocevic/ghoti-bench/internal/bench"
	"github.com/dankomiocevic/ghoti-bench/internal/ghoti"
	"github.com/dankomiocevic/ghoti-bench/internal/load"
	"github.com/dankomiocevic/ghoti-bench/internal/report"
	"github.com/dankomiocevic/ghoti-bench/internal/scenario"
	"github.com/dankomiocevic/ghoti-bench/internal/serverstats"
)

const usage = `Usage:
  ghoti-bench run (--ghoti-ref REF | --ghoti-binary PATH | --target HOST:PORT) [flags]
  ghoti-bench scenarios

Examples:
  ghoti-bench run --ghoti-ref v0.2.0
  ghoti-bench run --ghoti-ref 90b5271
  ghoti-bench run --ghoti-binary ./ghoti
  ghoti-bench run --target 10.0.1.15:9090 --metrics-addr 10.0.1.15:9100

Run flags:
`

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		var o options
		if err := newFlags(&o).Parse(os.Args[2:]); err != nil {
			os.Exit(2)
		}
		if err := run(&o); err != nil {
			fmt.Fprintln(os.Stderr, "ghoti-bench:", err)
			os.Exit(1)
		}
	case "scenarios":
		for _, n := range scenario.Names() {
			s, _ := scenario.Get(n)
			fmt.Printf("%-14s %s\n", s.Name, s.Description)
		}
	default:
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, usage)
	newFlags(&options{}).PrintDefaults()
}

type options struct {
	ghotiRef, ghotiBinary, target, metricsAddr string
	ghotiRepo, cacheDir                        string
	forceBuild                                 bool

	scenario, connections, resultsDir string
	repetitions, serverGOMAXPROCS     int
	warmup, duration, cooldown        time.Duration
	requestTimeout                    time.Duration
	seed                              uint64
	saveHistogram                     bool
	maxGeneratorCPU, serverCores      float64

	serverInstance, generatorInstance, region string
	ghotiVersion, ghotiCommit                 string
}

func newFlags(o *options) *flag.FlagSet {
	fs := flag.NewFlagSet("ghoti-bench run", flag.ContinueOnError)
	fs.StringVar(&o.ghotiRef, "ghoti-ref", "", "release tag or commit to benchmark (downloads the release or builds from source)")
	fs.StringVar(&o.ghotiBinary, "ghoti-binary", "", "existing Ghoti binary to benchmark")
	fs.StringVar(&o.target, "target", "", "external Ghoti server to benchmark, host:port")
	fs.StringVar(&o.metricsAddr, "metrics-addr", "", "Prometheus endpoint of the --target server")
	fs.StringVar(&o.ghotiVersion, "ghoti-version", "", "Ghoti version of the --target server, recorded in the reports")
	fs.StringVar(&o.ghotiCommit, "ghoti-commit", "", "Ghoti commit of the --target server, recorded in the reports")
	fs.StringVar(&o.ghotiRepo, "ghoti-repo", ghoti.DefaultRepo, "Ghoti git repository URL or local path")
	fs.StringVar(&o.cacheDir, "cache-dir", ghoti.DefaultCacheDir(), "cache for releases, sources and builds")
	fs.BoolVar(&o.forceBuild, "build", false, "build --ghoti-ref from source even if a release exists")

	fs.StringVar(&o.scenario, "scenario", "memory-read", "scenario to run, see 'ghoti-bench scenarios'")
	fs.StringVar(&o.connections, "connections", "1,100,1000", "comma separated connection counts")
	fs.IntVar(&o.repetitions, "repetitions", 3, "repetitions of every connection count")
	fs.DurationVar(&o.warmup, "warmup", 30*time.Second, "warm-up before each measurement")
	fs.DurationVar(&o.duration, "duration", 5*time.Minute, "measurement duration of each run")
	fs.DurationVar(&o.cooldown, "cooldown", 5*time.Second, "pause between runs")
	fs.DurationVar(&o.requestTimeout, "request-timeout", 5*time.Second, "time a request may wait for its response")
	fs.Uint64Var(&o.seed, "seed", 1, "seed for slot and operation choice")
	fs.StringVar(&o.resultsDir, "results-dir", "", "output directory (default results/<timestamp>-<scenario>)")
	fs.BoolVar(&o.saveHistogram, "save-histogram", false, "embed the compressed HDR histogram in each run JSON")
	fs.Float64Var(&o.maxGeneratorCPU, "max-generator-cpu", report.DefaultMaxGeneratorCPUPct, "generator CPU percentage above which a run is invalid")

	fs.IntVar(&o.serverGOMAXPROCS, "server-gomaxprocs", 0, "GOMAXPROCS for a server started by ghoti-bench (0 = Go default)")
	fs.Float64Var(&o.serverCores, "server-cores", 0, "--target server cores, for CPU percentages when not reported by the endpoint")
	fs.StringVar(&o.serverInstance, "server-instance", "local", "server instance type label")
	fs.StringVar(&o.generatorInstance, "generator-instance", "local", "generator instance type label")
	fs.StringVar(&o.region, "region", "", "region label")
	return fs
}

func run(o *options) error {
	sources := 0
	for _, s := range []string{o.ghotiRef, o.ghotiBinary, o.target} {
		if s != "" {
			sources++
		}
	}
	if sources != 1 {
		return errors.New("exactly one of --ghoti-ref, --ghoti-binary or --target is required")
	}
	if o.target == "" && (o.metricsAddr != "" || o.ghotiVersion != "" || o.ghotiCommit != "") {
		return errors.New("--metrics-addr, --ghoti-version and --ghoti-commit only apply to --target; " +
			"a server started by ghoti-bench exposes metrics and its version is read from the binary")
	}
	if o.target != "" && o.ghotiVersion == "" && o.ghotiCommit == "" {
		fmt.Fprintln(os.Stderr, "warning: --target without --ghoti-version or --ghoti-commit, the reports will not say which Ghoti was measured")
	}
	sc, err := scenario.Get(o.scenario)
	if err != nil {
		return err
	}
	conns, err := parseInts(o.connections)
	if err != nil {
		return fmt.Errorf("--connections: %w", err)
	}
	if o.repetitions < 1 {
		return errors.New("--repetitions must be at least 1")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var bin *ghoti.Binary
	switch {
	case o.ghotiBinary != "":
		bin, err = ghoti.Local(ctx, o.ghotiBinary)
	case o.ghotiRef != "":
		bin, err = ghoti.Resolve(ctx, o.ghotiRef, ghoti.ResolveOptions{
			Repo: o.ghotiRepo, CacheDir: o.cacheDir, ForceBuild: o.forceBuild, Log: os.Stderr,
		})
	}
	if err != nil {
		return err
	}
	if bin != nil {
		fmt.Fprintf(os.Stderr, "ghoti %s (commit %s) from %s: %s\n", orDash(bin.Version), orDash(bin.Commit), bin.Source, bin.Path)
	}

	started := time.Now().UTC()
	if o.resultsDir == "" {
		o.resultsDir = filepath.Join("results", started.Format("20060102T150405Z")+"-"+sc.Name)
	}
	summaryPath := filepath.Join(o.resultsDir, "summary.csv")

	total := len(conns) * o.repetitions
	var runs []*report.Run
	n := 0
	// Repetitions are the outer loop so slow drift of the host, thermal or
	// otherwise, spreads over every connection count instead of biasing one.
	for rep := 1; rep <= o.repetitions; rep++ {
		for _, c := range conns {
			n++
			if ctx.Err() != nil {
				break
			}
			if n > 1 && o.cooldown > 0 {
				select {
				case <-time.After(o.cooldown):
				case <-ctx.Done():
				}
			}
			runID := fmt.Sprintf("%s-c%d-r%d", sc.Name, c, rep)
			fmt.Fprintf(os.Stderr, "\n[%d/%d] ", n, total)
			r, err := runOne(ctx, o, sc, bin, c, rep, runID, summaryPath)
			if err != nil {
				return fmt.Errorf("run %s: %w", runID, err)
			}
			runs = append(runs, r)
		}
	}

	fmt.Fprintf(os.Stderr, "\nresults in %s\n\n", o.resultsDir)
	printTable(os.Stdout, runs)
	if ctx.Err() != nil {
		return errors.New("interrupted")
	}
	return nil
}

func runOne(ctx context.Context, o *options, sc scenario.Scenario, bin *ghoti.Binary, conns, rep int, runID, summaryPath string) (*report.Run, error) {
	meta := report.Meta{
		RunID:              runID,
		Scenario:           sc.Name,
		Repetition:         rep,
		Timestamp:          time.Now(),
		ServerInstance:     o.serverInstance,
		GeneratorInstance:  o.generatorInstance,
		Region:             o.region,
		SaveHistogram:      o.saveHistogram,
		MaxGeneratorCPUPct: o.maxGeneratorCPU,
	}
	spec := bench.Spec{
		Load: load.Config{
			Protocol:       "standard",
			Connections:    conns,
			Workload:       sc.Workload,
			ReadPercent:    sc.ReadPercent,
			Slots:          sc.Slots,
			PayloadSize:    sc.PayloadSize,
			Warmup:         o.warmup,
			Duration:       o.duration,
			RequestTimeout: o.requestTimeout,
			Seed:           o.seed + uint64(rep),
		},
		JSONPath:    filepath.Join(o.resultsDir, runID+".json"),
		SummaryPath: summaryPath,
		Log:         os.Stderr,
	}

	if o.target != "" {
		spec.Load.Addr = o.target
		meta.Target = o.target
		// An external server cannot be asked for its version, the labels
		// are what keeps the run comparable and reproducible.
		meta.GhotiVersion = o.ghotiVersion
		meta.GhotiCommit = o.ghotiCommit
		if o.metricsAddr != "" {
			spec.Stats = serverstats.NewPrometheus(o.metricsAddr)
		}
		spec.ServerCores = o.serverCores
		if spec.ServerCores == 0 {
			spec.ServerCores = -1
		}
		spec.Meta = meta
		return bench.Execute(ctx, spec)
	}

	meta.ServerGOMAXPROCS = o.serverGOMAXPROCS
	meta.GhotiVersion = bin.Version
	meta.GhotiCommit = bin.Commit
	meta.GhotiRef = bin.Ref
	meta.GhotiBinary = bin.Path
	spec.Meta = meta

	cfg := ghoti.ServerConfig{
		SimpleMemorySlots: sc.SlotList(),
		LogLevel:          "warn",
		GOMAXPROCS:        o.serverGOMAXPROCS,
	}
	// Every run gets a fresh server so no run inherits state, garbage or
	// connections from the previous one.
	return runManagedServer(ctx, bin, cfg, filepath.Join(o.resultsDir, "servers", runID),
		func(srv *ghoti.Server) (*report.Run, error) {
			spec.Load.Addr = srv.Config.Addr
			// The process is sampled directly for CPU and memory, which works
			// on every platform; Prometheus adds connections and GOMAXPROCS.
			spec.Stats = serverstats.Multi{
				&serverstats.Process{PID: srv.PID()},
				serverstats.NewPrometheus(srv.Config.MetricsAddr),
			}
			spec.ServerCores = float64(runtime.NumCPU())
			if o.serverGOMAXPROCS > 0 {
				spec.ServerCores = float64(o.serverGOMAXPROCS)
			}
			return bench.Execute(ctx, spec)
		})
}

// runManagedServer starts a server on free ports, runs fn against it and
// stops it. A server that crashed, failed to exit cleanly or had to be
// killed breaks the isolation between runs, so a failed stop is reported
// and returned together with any error from fn, which stops the benchmark.
func runManagedServer(ctx context.Context, bin *ghoti.Binary, cfg ghoti.ServerConfig, dir string,
	fn func(*ghoti.Server) (*report.Run, error)) (*report.Run, error) {
	srv, err := ghoti.StartOnFreePorts(ctx, bin, cfg, dir)
	if err != nil {
		return nil, err
	}
	r, runErr := fn(srv)
	if stopErr := srv.Stop(); stopErr != nil {
		fmt.Fprintln(os.Stderr, "  WARNING:", stopErr)
		return r, errors.Join(runErr, stopErr)
	}
	return r, runErr
}

// printTable prints, for every connection count, the median of the valid
// runs and the spread of their throughput. Invalid runs never feed the
// aggregates: an interrupted run would drag the median down and a run where
// the generator saturated would misstate the server. They are counted and
// listed with their reasons below the table instead.
func printTable(w io.Writer, runs []*report.Run) {
	byConns := map[int][]*report.Run{}
	var keys []int
	for _, r := range runs {
		if _, ok := byConns[r.Connections]; !ok {
			keys = append(keys, r.Connections)
		}
		byConns[r.Connections] = append(byConns[r.Connections], r)
	}
	slices.Sort(keys)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "connections\truns\tvalid\treq/s (median)\treq/s spread\tp50 ms\tp99 ms\tp99.9 ms\tmax ms\terrors\tserver cpu %\t")
	var invalid []*report.Run
	for _, c := range keys {
		var errs uint64
		var rps, p50, p99, p999, mx, cpu []float64
		for _, r := range byConns[c] {
			// Errors are summed over every run, they are the diagnostic.
			errs += r.Result.Errors
			if !r.Result.Valid {
				invalid = append(invalid, r)
				continue
			}
			rps = append(rps, r.Result.ThroughputRps)
			p50 = append(p50, r.Result.LatencyMs.P50)
			p99 = append(p99, r.Result.LatencyMs.P99)
			p999 = append(p999, r.Result.LatencyMs.P999)
			mx = append(mx, r.Result.LatencyMs.Max)
			if r.ServerStats.CPUAvgPct != nil {
				cpu = append(cpu, *r.ServerStats.CPUAvgPct)
			}
		}

		const na = "N/A"
		rpsStr, spread, p50Str, p99Str, p999Str, maxStr, cpuStr := na, na, na, na, na, na, na
		if len(rps) > 0 {
			rpsStr = fmt.Sprintf("%.0f", median(rps))
			p50Str = fmt.Sprintf("%.3f", median(p50))
			p99Str = fmt.Sprintf("%.3f", median(p99))
			p999Str = fmt.Sprintf("%.3f", median(p999))
			maxStr = fmt.Sprintf("%.3f", slices.Max(mx))
			spread = "-"
		}
		if len(rps) > 1 {
			lo, hi, med := slices.Min(rps), slices.Max(rps), median(rps)
			spread = fmt.Sprintf("±%.1f%%", (hi-lo)/2/med*100)
		}
		if len(cpu) > 0 {
			cpuStr = fmt.Sprintf("%.1f", median(cpu))
		}
		fmt.Fprintf(tw, "%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t\n",
			c, len(byConns[c]), len(rps), rpsStr, spread, p50Str, p99Str, p999Str, maxStr, errs, cpuStr)
	}
	tw.Flush()

	if len(invalid) > 0 {
		fmt.Fprintf(w, "\n%d invalid runs, excluded from the aggregates:\n", len(invalid))
		for _, r := range invalid {
			fmt.Fprintf(w, "  %s: %s\n", r.RunID, strings.Join(r.Result.InvalidReasons, "; "))
		}
	}
}

func median(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func parseInts(s string) ([]int, error) {
	var out []int
	for part := range strings.SplitSeq(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid value %q", part)
		}
		out = append(out, n)
	}
	return out, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
