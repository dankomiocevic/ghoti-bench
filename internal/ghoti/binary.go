// Package ghoti obtains and runs Ghoti server binaries. It only uses the
// release archives, git and the Go toolchain; it never imports the Ghoti
// module, so the benchmark talks to Ghoti exactly like any other client.
package ghoti

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// DefaultRepo is the upstream Ghoti repository.
const DefaultRepo = "https://github.com/dankomiocevic/ghoti"

// Binary is a Ghoti executable ready to run.
type Binary struct {
	Path    string
	Ref     string
	Version string
	Commit  string
	// Source is "release", "build" or "local".
	Source string
}

// ResolveOptions selects how a binary is obtained.
type ResolveOptions struct {
	// Repo is the git URL or local path of the Ghoti repository.
	Repo string
	// CacheDir holds downloaded releases, the source clone and builds.
	CacheDir string
	// ForceBuild builds from source even when a release archive exists.
	ForceBuild bool
	// OS and Arch select the target platform, the local one when empty. A
	// binary for another platform cannot be run to read its version, so it
	// is taken from the tag or the build instead.
	OS   string
	Arch string
	Log  io.Writer
}

func (o *ResolveOptions) logf(format string, args ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format, args...)
	}
}

// DefaultCacheDir returns the per-user cache directory for ghoti-bench.
func DefaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ghoti-bench")
}

// Local wraps an existing binary.
func Local(ctx context.Context, path string) (*Binary, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	b := &Binary{Path: abs, Source: "local"}
	b.Version, b.Commit = readVersion(ctx, abs)
	return b, nil
}

var tagPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+`)

// Resolve returns a binary for ref: the release archive for tags when one
// exists for this platform, otherwise a build of that commit from source.
func Resolve(ctx context.Context, ref string, o ResolveOptions) (*Binary, error) {
	if o.Repo == "" {
		o.Repo = DefaultRepo
	}
	if o.CacheDir == "" {
		o.CacheDir = DefaultCacheDir()
	}
	if o.OS == "" {
		o.OS = runtime.GOOS
	}
	if o.Arch == "" {
		o.Arch = runtime.GOARCH
	}

	if tagPattern.MatchString(ref) && !o.ForceBuild && isGitHub(o.Repo) {
		b, err := downloadRelease(ctx, ref, o)
		if err == nil {
			return b, nil
		}
		o.logf("release %s not usable (%v), building from source\n", ref, err)
	}
	return build(ctx, ref, o)
}

func isGitHub(repo string) bool { return strings.HasPrefix(repo, "https://github.com/") }

// downloadRelease fetches ghoti_<version>_<os>_<arch>.tar.gz, verifies it
// against checksums.txt and extracts the binary.
func downloadRelease(ctx context.Context, tag string, o ResolveOptions) (*Binary, error) {
	dir := filepath.Join(o.CacheDir, "releases", tag, o.OS+"_"+o.Arch)
	bin := filepath.Join(dir, "ghoti")
	version := strings.TrimPrefix(tag, "v")
	if _, err := os.Stat(bin); err == nil {
		o.logf("using cached release %s\n", bin)
		return finish(ctx, bin, tag, "release", o, version, "")
	}

	base := strings.TrimSuffix(strings.TrimSuffix(o.Repo, "/"), ".git") + "/releases/download/" + tag + "/"
	archive := fmt.Sprintf("ghoti_%s_%s_%s.tar.gz", version, o.OS, o.Arch)

	o.logf("downloading %s%s\n", base, archive)
	sums, err := fetch(ctx, base+"checksums.txt")
	if err != nil {
		return nil, err
	}
	want, err := checksumFor(sums, archive)
	if err != nil {
		return nil, err
	}
	data, err := fetch(ctx, base+archive)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("checksum mismatch for %s", archive)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := extractFile(bytes.NewReader(data), "ghoti", bin); err != nil {
		return nil, err
	}
	return finish(ctx, bin, tag, "release", o, version, "")
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s not listed in checksums.txt", name)
}

func extractFile(r io.Reader, name, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s not found in archive", name)
		}
		if err != nil {
			return err
		}
		if filepath.Clean(h.Name) != name || h.Typeflag != tar.TypeReg {
			continue
		}
		tmp := dst + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Rename(tmp, dst)
	}
}

// build compiles ref from a cached clone of the repository, with the same
// flags the release pipeline uses so builds and releases are comparable.
func build(ctx context.Context, ref string, o ResolveOptions) (*Binary, error) {
	src := filepath.Join(o.CacheDir, "src", "ghoti")
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		o.logf("cloning %s\n", o.Repo)
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			return nil, err
		}
		if err := runCmd(ctx, "", nil, "git", "clone", "--quiet", o.Repo, src); err != nil {
			return nil, err
		}
	} else {
		o.logf("fetching %s\n", o.Repo)
		if err := runCmd(ctx, src, nil, "git", "fetch", "--quiet", "--tags", "--force", o.Repo, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return nil, err
		}
	}

	commit, err := output(ctx, src, "git", "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		// A branch name only exists as a remote-tracking ref in the cache.
		commit, err = output(ctx, src, "git", "rev-parse", "--verify", "origin/"+ref+"^{commit}")
		if err != nil {
			return nil, fmt.Errorf("unknown ref %q", ref)
		}
	}

	bin := filepath.Join(o.CacheDir, "builds", commit, o.OS+"_"+o.Arch, "ghoti")
	if _, err := os.Stat(bin); err == nil {
		o.logf("using cached build %s\n", bin)
		return finish(ctx, bin, ref, "build", o, ref, commit[:12])
	}

	work, err := os.MkdirTemp("", "ghoti-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	o.logf("building %s (%s)\n", ref, commit[:12])
	if err := gitArchive(ctx, src, commit, work); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return nil, err
	}
	const appinfo = "github.com/dankomiocevic/ghoti/internal/appinfo"
	ldflags := fmt.Sprintf("-s -w -X %s.Version=%s -X %s.Commit=%s", appinfo, ref, appinfo, commit[:12])
	env := append(os.Environ(), "CGO_ENABLED=0", "GOOS="+o.OS, "GOARCH="+o.Arch)
	if err := runCmd(ctx, work, env, "go", "build", "-trimpath", "-ldflags", ldflags, "-o", bin, "./cmd/ghoti"); err != nil {
		return nil, err
	}
	return finish(ctx, bin, ref, "build", o, ref, commit[:12])
}

func gitArchive(ctx context.Context, repo, commit, dst string) error {
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", commit)
	cmd.Dir = repo
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	tr := tar.NewReader(out)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cmd.Wait()
			return err
		}
		target := filepath.Join(dst, filepath.Clean(h.Name))
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return cmd.Wait()
}

// finish describes the binary. A local binary reports its own version; one
// built for another platform cannot run here, so the version and commit known
// from the tag or the build are used.
func finish(ctx context.Context, bin, ref, source string, o ResolveOptions, version, commit string) (*Binary, error) {
	b := &Binary{Path: bin, Ref: ref, Source: source, Version: version, Commit: commit}
	if o.OS == runtime.GOOS && o.Arch == runtime.GOARCH {
		if v, c := readVersion(ctx, bin); v != "" {
			b.Version, b.Commit = v, c
		}
	}
	return b, nil
}

var versionPattern = regexp.MustCompile("version `([^`]*)` build from `([^`]*)`")

// readVersion runs "ghoti version", which prints
// "Ghoti version `X` build from `Y` on `Z`".
func readVersion(ctx context.Context, bin string) (version, commit string) {
	out, err := exec.CommandContext(ctx, bin, "version").CombinedOutput()
	if err != nil {
		return "", ""
	}
	m := versionPattern.FindSubmatch(out)
	if m == nil {
		return strings.TrimSpace(string(out)), ""
	}
	return string(m[1]), string(m[2])
}

func runCmd(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

func output(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
