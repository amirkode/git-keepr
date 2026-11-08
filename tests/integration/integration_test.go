// Package integration exercises the real git-keepr binary and the real S3
// adapter end to end using only local resources: temporary Git repositories,
// an in-process fake S3 (TLS) and a black-hole HTTP proxy.
package integration

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/amirkode/git-keepr/internal/config"
	"github.com/amirkode/git-keepr/internal/gitmirror"
	"github.com/amirkode/git-keepr/internal/service"
	s3store "github.com/amirkode/git-keepr/internal/storage/s3"
	"github.com/amirkode/git-keepr/internal/storage/s3test"
	"github.com/amirkode/git-keepr/internal/testutil"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "git-keepr-it-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "git-keepr")
	build := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-X github.com/amirkode/git-keepr/internal/cli.Version=v0.0.0-inttest",
		"-o", binary, "../../cmd/git-keepr")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func baseEnv(t *testing.T, extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "AWS_EC2_METADATA_DISABLED=true"}, extra...)
}

func runBinary(t *testing.T, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

func TestVersionFromLdflags(t *testing.T) {
	code, out, _ := runBinary(t, baseEnv(t), "version")
	if code != 0 || !strings.HasPrefix(out, "git-keepr v0.0.0-inttest") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestOfflineRestoreEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses SSL_CERT_FILE to trust the test TLS certificate")
	}
	ctx := context.Background()
	src := testutil.Populated(t)
	fake := s3test.NewTLS(t, "bkt")
	root := filepath.Join(t.TempDir(), "data")

	certPEM := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fake.Certificate().Raw}), 0o600)

	yaml := fmt.Sprintf(`
version: 1
credentials:
  personal: {type: github_pat, token_env: IT_PAT}
repositories:
  - {id: github/test/one, url: "https://github.com/test/one.git", credential: personal}
storage:
  local: {root: %s}
  s3: {enabled: true, bucket: bkt, prefix: repositories/, region: us-east-1, endpoint: "%s", path_style: true}
`, root, fake.URL)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(cfgPath, []byte(yaml), 0o600)

	// --- back up through the real pipeline and the real S3 adapter (source is a local repo) ---
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repositories[0].URL = src.URL()
	store, err := s3store.New(ctx, s3store.Options{
		Bucket: "bkt", Region: "us-east-1", Endpoint: fake.URL, PathStyle: true, HTTPClient: fake.Client(), MaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider("AKIADUMMY", "dummy", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	g := gitmirror.New(t.TempDir())
	g.AllowProtocols = "file"
	svc := service.New(cfg, g, store, nil)
	svc.LookupEnv = func(string) (string, bool) { return "dummy-it-token", true }
	repo := cfg.Repositories[0]

	if r := svc.SyncRepo(ctx, repo); !r.OK() || r.S3Status != service.StatusOK {
		t.Fatalf("first sync: %+v", r)
	}
	src.Commit("later.txt", "later")
	src.Git("tag", "v2")
	if r := svc.SyncRepo(ctx, repo); !r.OK() || r.S3Status != service.StatusOK {
		t.Fatalf("second sync: %+v", r)
	}
	puts := fake.Puts.Load()
	if r := svc.SyncRepo(ctx, repo); !r.OK() || r.S3Status != service.StatusUnchanged || fake.Puts.Load() != puts {
		t.Fatalf("third sync should not upload: %+v", r)
	}
	key := cfg.S3Key(repo.ID)
	if data, meta, ok := fake.Object(key); !ok || len(data) == 0 || meta["sha256"] == "" {
		t.Fatalf("fake S3 lacks the bundle or checksum metadata (keys: %v)", fake.Keys())
	}
	wantRefs := testutil.Run(t, svc.Layout().MirrorDir(repo.ID), "for-each-ref", "--format=%(refname) %(objectname)")
	wantCommits := testutil.Run(t, src.Dir, "rev-list", "--all", "--count")

	// --- the original source disappears; restore with only the binary, no PAT, no GitHub ---
	os.RemoveAll(src.Dir)
	env := baseEnv(t, "SSL_CERT_FILE="+certPEM, "AWS_ACCESS_KEY_ID=AKIADUMMY", "AWS_SECRET_ACCESS_KEY=dummy")

	for _, from := range []string{"local", "s3"} {
		out := filepath.Join(t.TempDir(), "restored.git")
		code, stdout, stderr := runBinary(t, env, "restore", "--repo", repo.ID, "--from", from, "--output", out, "--config", cfgPath)
		if code != 0 {
			t.Fatalf("restore --from %s: exit %d\n%s\n%s", from, code, stdout, stderr)
		}
		if from == "s3" && !strings.Contains(stdout, "sha256 verified") {
			t.Fatalf("s3 restore should report checksum verification:\n%s", stdout)
		}
		if got := testutil.Run(t, out, "for-each-ref", "--format=%(refname) %(objectname)"); got != wantRefs {
			t.Fatalf("%s: refs differ\n got %s\nwant %s", from, got, wantRefs)
		}
		testutil.Run(t, out, "fsck", "--no-progress", "--strict")
		if n := testutil.Run(t, out, "rev-list", "--all", "--count"); n != wantCommits {
			t.Fatalf("%s: commits %s, want %s", from, n, wantCommits)
		}
		wc := filepath.Join(t.TempDir(), "checkout")
		testutil.Run(t, "", "clone", "--quiet", out, wc)
		if b, _ := os.ReadFile(filepath.Join(wc, "later.txt")); string(b) != "later" {
			t.Fatalf("%s: restored working tree is missing the latest commit", from)
		}
		// second restore into the same path is refused
		if code, _, _ := runBinary(t, env, "restore", "--repo", repo.ID, "--from", from, "--output", out, "--config", cfgPath); code != 1 {
			t.Fatalf("%s: overwrite must fail with exit 1, got %d", from, code)
		}
	}

	// status works offline from state files
	if code, out, _ := runBinary(t, env, "status", "--config", cfgPath); code != 0 || !strings.Contains(out, "github/test/one") || !strings.Contains(out, "ok") {
		t.Fatalf("status: %d\n%s", code, out)
	}

	// bit-rot in the bucket is detected by the checksum, nothing is restored
	data, meta, _ := fake.Object(key)
	bad := append([]byte(nil), data...)
	bad[len(bad)/2] ^= 0xff
	fake.Set(key, bad, meta)
	out := filepath.Join(t.TempDir(), "corrupt.git")
	code, _, stderr := runBinary(t, env, "restore", "--repo", repo.ID, "--from", "s3", "--output", out, "--config", cfgPath)
	if code != 1 || !strings.Contains(stderr, "checksum") {
		t.Fatalf("corrupt bundle: exit %d\n%s", code, stderr)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("corrupt restore must not create output")
	}
}

func TestServeFailsFastOnMissingCredentials(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
version: 1
credentials: {personal: {type: github_pat, token_env: IT_PAT_MISSING}}
repositories: [{id: github/test/one, url: "https://github.com/test/one.git", credential: personal}]
storage: {local: {root: %s}}
`, root)), 0o600)
	code, _, stderr := runBinary(t, baseEnv(t), "serve", "--config", cfgPath)
	if code != 3 || !strings.Contains(stderr, "IT_PAT_MISSING") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
}

// TestSigtermDuringFirstCloneExitsCleanly starts the real daemon against an
// unreachable "GitHub" (a proxy that accepts connections and never answers),
// sends SIGTERM mid-clone and checks for a clean exit without a partial mirror
// or an orphaned git helper holding the connection open.
func TestSigtermDuringFirstCloneExitsCleanly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted, closed atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						closed.Add(1)
						return
					}
				}
			}()
		}
	}()

	root := filepath.Join(t.TempDir(), "data")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
version: 1
credentials: {personal: {type: github_pat, token_env: IT_PAT}}
repositories: [{id: github/test/one, url: "https://github.com/test/one.git", credential: personal}]
storage: {local: {root: %s}}
`, root)), 0o600)

	env := baseEnv(t, "IT_PAT=dummy-it-token", "HTTPS_PROXY=http://"+ln.Addr().String(), "GIT_KEEPR_LOG_FORMAT=json")
	cmd := exec.Command(binary, "serve", "--config", cfgPath)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })

	deadline := time.Now().Add(20 * time.Second)
	for accepted.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("git never connected to the proxy; stderr:\n%s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("daemon should exit 0 on SIGTERM, got %v\n%s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("daemon did not exit after SIGTERM; stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "shutdown complete") {
		t.Fatalf("missing shutdown log:\n%s", stderr.String())
	}
	for deadline := time.Now().Add(5 * time.Second); closed.Load() < accepted.Load() && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if closed.Load() < accepted.Load() {
		t.Fatal("a git helper process kept its connection open after shutdown")
	}
	var leftovers []string
	filepath.WalkDir(filepath.Join(root, "mirrors"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			leftovers = append(leftovers, p)
		}
		if err == nil && d.IsDir() && strings.Contains(d.Name(), ".git") {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if len(leftovers) > 0 {
		t.Fatalf("partial mirror left behind: %v", leftovers)
	}
	if strings.Contains(stderr.String(), "dummy-it-token") {
		t.Fatal("token leaked into logs")
	}
}
