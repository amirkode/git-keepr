package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amirkode/git-keepr/internal/credentials"
	"github.com/amirkode/git-keepr/internal/gitmirror"
	"github.com/amirkode/git-keepr/internal/state"
	"github.com/amirkode/git-keepr/internal/testutil"
)

func TestMain(m *testing.M) {
	credentials.MaybeRunAskpass()
	os.Exit(m.Run())
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func writeConfig(t *testing.T, root string, s3 bool) string {
	t.Helper()
	y := fmt.Sprintf(`
version: 1
credentials:
  personal: {type: github_pat, token_env: CLI_TEST_PAT}
repositories:
  - {id: github/example/backend, url: "https://github.com/example/backend.git", credential: personal, interval: 1h}
  - {id: github/example/frontend, url: "https://github.com/example/frontend.git", credential: personal}
storage:
  local: {root: %s}
  s3: {enabled: %v, bucket: my-bucket, region: us-east-1}
`, root, s3)
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBasicCommandsAndUsage(t *testing.T) {
	if code, out, _ := run(t, "version"); code != 0 || !strings.HasPrefix(out, "git-keepr ") {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, out, _ := run(t, "--version"); code != 0 || !strings.HasPrefix(out, "git-keepr ") {
		t.Fatalf("--version: %d %q", code, out)
	}
	if code, out, _ := run(t, "--help"); code != 0 || !strings.Contains(out, "Your Git history") && !strings.Contains(out, "git-keepr") {
		t.Fatalf("help: %d", code)
	}
	if code, _, _ := run(t); code != ExitUsage {
		t.Fatalf("no args should be a usage error, got %d", code)
	}
	if code, _, e := run(t, "frobnicate"); code != ExitUsage || !strings.Contains(e, "unknown command") {
		t.Fatalf("unknown: %d %q", code, e)
	}
	if code, _, _ := run(t, "serve", "--help"); code != 0 {
		t.Fatalf("subcommand --help should exit 0, got %d", code)
	}
	if code, _, _ := run(t, "serve", "--bogus"); code != ExitUsage {
		t.Fatalf("bad flag should be a usage error, got %d", code)
	}
	if code, _, _ := run(t, "config"); code != ExitUsage {
		t.Fatalf("config without subcommand: %d", code)
	}
}

func TestConfigValidate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	cfg := writeConfig(t, root, false)

	t.Setenv("CLI_TEST_PAT", "")
	code, _, e := run(t, "config", "validate", "--config", cfg)
	if code != ExitConfig || !strings.Contains(e, "CLI_TEST_PAT") {
		t.Fatalf("missing token should fail naming the variable: %d %q", code, e)
	}

	t.Setenv("CLI_TEST_PAT", "dummy-secret-token-1234")
	code, out, e := run(t, "config", "validate", "--config", cfg)
	if code != 0 {
		t.Fatalf("expected valid: %d %q", code, e)
	}
	if strings.Contains(out+e, "dummy-secret-token-1234") {
		t.Fatal("validate must never print token values")
	}
	if !strings.Contains(out, "2 repositories") {
		t.Fatalf("summary: %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, "mirrors", "github")); err == nil {
		t.Fatal("validate must not sync anything")
	}

	// config path via environment variable
	t.Setenv("GIT_KEEPR_CONFIG", cfg)
	if code, _, _ := run(t, "config", "validate"); code != 0 {
		t.Fatal("GIT_KEEPR_CONFIG should be honoured")
	}

	// invalid config
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("version: 1\nrepositories:\n  - {id: github/a/b, url: 'http://evil.example/x', credential: nope}\n"), 0o600)
	code, _, e = run(t, "config", "validate", "--config", bad)
	if code != ExitConfig || !strings.Contains(e, "url") || !strings.Contains(e, "credential") {
		t.Fatalf("bad config: %d %q", code, e)
	}
	if code, _, _ := run(t, "config", "validate", "--config", filepath.Join(t.TempDir(), "missing.yaml")); code != ExitConfig {
		t.Fatalf("missing file: %d", code)
	}
}

func TestConfigValidateS3CredentialsAreSurfaced(t *testing.T) {
	cfg := writeConfig(t, filepath.Join(t.TempDir(), "data"), true)
	t.Setenv("CLI_TEST_PAT", "dummy-secret-token-1234")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "x"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "x"))
	code, _, e := run(t, "config", "validate", "--config", cfg)
	if code != ExitConfig || !strings.Contains(e, "AWS_ACCESS_KEY_ID") {
		t.Fatalf("expected S3 credential problem: %d %q", code, e)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIADUMMY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "dummy")
	if code, _, e := run(t, "config", "validate", "--config", cfg); code != 0 {
		t.Fatalf("with credentials: %d %q", code, e)
	}
}

func TestSyncArgumentErrors(t *testing.T) {
	cfg := writeConfig(t, filepath.Join(t.TempDir(), "data"), false)
	t.Setenv("CLI_TEST_PAT", "dummy")
	if code, _, e := run(t, "sync", "--config", cfg); code != ExitUsage || !strings.Contains(e, "--once") {
		t.Fatalf("sync without --once: %d %q", code, e)
	}
	if code, _, e := run(t, "sync", "--once", "--repo", "github/none/none", "--config", cfg); code != ExitUsage || !strings.Contains(e, "unknown repository") {
		t.Fatalf("unknown repo: %d %q", code, e)
	}
	t.Setenv("CLI_TEST_PAT", "")
	if code, _, e := run(t, "sync", "--once", "--config", cfg); code != ExitConfig || !strings.Contains(e, "CLI_TEST_PAT") {
		t.Fatalf("missing token: %d %q", code, e)
	}
}

func TestStatusOutputAndExitCode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	cfgPath := writeConfig(t, root, true)
	layout := state.Layout{Root: root}

	// nothing recorded yet
	if code, out, _ := run(t, "status", "--config", cfgPath); code != 0 || !strings.Contains(out, "github/example/backend") || !strings.Contains(out, "never") {
		t.Fatalf("fresh status: %d %q", code, out)
	}

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	st := state.New("github/example/backend")
	st.Git = state.Git{Status: "ok", LastSuccess: now, RefFingerprint: "abcdef0123456789abcdef", RefCount: 7, HeadRef: "refs/heads/main", DurationMS: 1500}
	st.S3 = state.S3{Status: "ok", LastSuccess: now, PublishedFingerprint: "abcdef0123456789abcdef", Size: 4096}
	if err := state.Save(layout.StateFile("github/example/backend"), st); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "status", "--config", cfgPath)
	for _, want := range []string{"2026-01-02T03:04:05Z", "abcdef012345", "refs 7", "refs/heads/main", "1.5s", "size 4096"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status lacks %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Fatalf("no errors recorded but exit %d", code)
	}

	st.S3 = state.S3{Status: "error", LastError: "s3 upload: connection refused", PublishedFingerprint: "old"}
	st.Git.LastError = ""
	state.Save(layout.StateFile("github/example/backend"), st)
	code, out, _ = run(t, "status", "--config", cfgPath)
	if code != ExitFailure || !strings.Contains(out, "connection refused") {
		t.Fatalf("error status: %d\n%s", code, out)
	}
}

func TestRestoreFromLocalMirrorViaCLI(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	cfgPath := writeConfig(t, root, false)
	src := testutil.Populated(t)

	layout := state.Layout{Root: root}
	g := gitmirror.New(t.TempDir())
	g.AllowProtocols = "file"
	mirror := layout.MirrorDir("github/example/backend")
	os.MkdirAll(filepath.Dir(mirror), 0o700)
	if err := g.CloneMirror(context.Background(), src.URL(), mirror, nil); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(src.Dir) // source is gone; no PAT in the environment either
	t.Setenv("CLI_TEST_PAT", "")

	out := filepath.Join(t.TempDir(), "restored.git")
	code, stdout, e := run(t, "restore", "--repo", "github/example/backend", "--from", "local", "--output", out, "--config", cfgPath)
	if code != 0 {
		t.Fatalf("restore: %d %q", code, e)
	}
	if !strings.Contains(stdout, "refs: 4") || !strings.Contains(stdout, "fsck: ok") {
		t.Fatalf("output: %q", stdout)
	}
	testutil.Run(t, out, "fsck", "--no-progress")

	// never overwrite
	if code, _, e := run(t, "restore", "--repo", "github/example/backend", "--from", "local", "--output", out, "--config", cfgPath); code != ExitFailure || !strings.Contains(e, "already exists") {
		t.Fatalf("overwrite must be refused: %d %q", code, e)
	}
	// argument validation
	for _, args := range [][]string{
		{"restore", "--from", "local", "--output", out},
		{"restore", "--repo", "github/example/backend", "--from", "tape", "--output", out},
		{"restore", "--repo", "github/none/none", "--from", "local", "--output", out},
	} {
		if code, _, _ := run(t, append(args, "--config", cfgPath)...); code != ExitUsage {
			t.Fatalf("%v: expected usage error, got %d", args, code)
		}
	}
	// S3 restore with S3 disabled
	if code, _, e := run(t, "restore", "--repo", "github/example/backend", "--from", "s3", "--output", filepath.Join(t.TempDir(), "x.git"), "--config", cfgPath); code != ExitFailure || !strings.Contains(e, "S3 is not enabled") {
		t.Fatalf("s3 disabled: %d %q", code, e)
	}
}
