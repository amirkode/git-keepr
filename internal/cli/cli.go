// Package cli implements the git-keepr command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/amirkode/git-keepr/internal/config"
	"github.com/amirkode/git-keepr/internal/gitmirror"
	"github.com/amirkode/git-keepr/internal/service"
	"github.com/amirkode/git-keepr/internal/state"
	"github.com/amirkode/git-keepr/internal/storage"
	s3store "github.com/amirkode/git-keepr/internal/storage/s3"
)

// Build metadata, set with -ldflags "-X github.com/amirkode/git-keepr/internal/cli.Version=...".
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1 // a sync/restore/status check failed at runtime
	ExitUsage   = 2 // bad command line
	ExitConfig  = 3 // invalid configuration or environment
)

const usageText = `git-keepr - your Git history, your storage, your control.

Usage:
  git-keepr <command> [flags]

Commands:
  serve             Run continuously: sync on start, then at every interval
  sync --once       Synchronize now and exit (all repositories, or --repo <id>)
  status            Show the last recorded result for every repository
  restore           Restore a bare repository from the local mirror or the S3 bundle
  config validate   Check the configuration without any Git synchronization
  version           Print version information

Common flags:
  --config <path>   Configuration file (default: $GIT_KEEPR_CONFIG or ./config.yaml)

Environment:
  GIT_KEEPR_CONFIG       default configuration path
  GIT_KEEPR_LOG_FORMAT   "text" (default) or "json"
  GIT_KEEPR_LOG_LEVEL    "debug", "info" (default), "warn" or "error"

Exit codes: 0 ok, 1 failure, 2 usage error, 3 invalid configuration.
Run 'git-keepr <command> --help' for command flags.
`

// Run executes the CLI and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usageText)
		return ExitOK
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, versionString())
		return ExitOK
	case "serve":
		return cmdServe(rest, stderr)
	case "sync":
		return cmdSync(rest, stdout, stderr)
	case "status":
		return cmdStatus(rest, stdout, stderr)
	case "restore":
		return cmdRestore(rest, stdout, stderr)
	case "config":
		if len(rest) >= 1 && rest[0] == "validate" {
			return cmdValidate(rest[1:], stdout, stderr)
		}
		fmt.Fprintln(stderr, "usage: git-keepr config validate [--config <path>]")
		return ExitUsage
	}
	fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usageText)
	return ExitUsage
}

func versionString() string {
	v, commit, date := Version, Commit, Date
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && commit == "":
				commit = s.Value
			case s.Key == "vcs.time" && date == "":
				date = s.Value
			}
		}
	}
	out := "git-keepr " + v
	if commit != "" {
		if len(commit) > 12 {
			commit = commit[:12]
		}
		out += " (commit " + commit
		if date != "" {
			out += ", built " + date
		}
		out += ")"
	}
	return out
}

// newFlags creates a flag set that reports problems on stderr.
func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("git-keepr "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := fs.String("config", "", "configuration file (default $GIT_KEEPR_CONFIG or ./config.yaml)")
	return fs, cfg
}

// parse returns (exitCode, done). done is true when the caller should return exitCode.
func parse(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, true
		}
		return ExitUsage, true
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		return ExitUsage, true
	}
	return 0, false
}

func configPath(flagVal string) string {
	switch {
	case flagVal != "":
		return flagVal
	case os.Getenv("GIT_KEEPR_CONFIG") != "":
		return os.Getenv("GIT_KEEPR_CONFIG")
	}
	return "config.yaml"
}

func loadConfig(flagVal string, stderr io.Writer) (*config.Config, bool) {
	cfg, err := config.Load(configPath(flagVal))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return nil, false
	}
	return cfg, true
}

func newLogger(w io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("GIT_KEEPR_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(os.Getenv("GIT_KEEPR_LOG_FORMAT"), "json") {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// buildService prepares local storage, git and (when wantS3 and enabled) the S3 client.
func buildService(ctx context.Context, cfg *config.Config, log *slog.Logger, wantS3 bool) (*service.Service, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("the git executable was not found in PATH")
	}
	restrictUmask()
	layout := state.Layout{Root: cfg.Storage.Local.Root}
	if err := layout.Init(); err != nil {
		return nil, fmt.Errorf("prepare %s: %w", layout.Root, err)
	}
	g := gitmirror.New(layout.StagingDir())
	var store storage.Store
	if wantS3 && cfg.Storage.S3.Enabled {
		s, err := newS3(ctx, cfg)
		if err != nil {
			return nil, err
		}
		store = s
	}
	svc := service.New(cfg, g, store, log)
	svc.Version = Version
	return svc, nil
}

func newS3(ctx context.Context, cfg *config.Config) (*s3store.Store, error) {
	c := cfg.Storage.S3
	s, err := s3store.New(ctx, s3store.Options{Bucket: c.Bucket, Region: c.Region, Endpoint: c.Endpoint, PathStyle: c.PathStyle})
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.CheckCredentials(cctx); err != nil {
		return nil, err
	}
	return s, nil
}

func reportProblems(stderr io.Writer, errs []error) {
	for _, e := range errs {
		fmt.Fprintf(stderr, "error: %v\n", e)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// ---- serve ----

func cmdServe(args []string, stderr io.Writer) int {
	fs, cfgFlag := newFlags("serve", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	cfg, ok := loadConfig(*cfgFlag, stderr)
	if !ok {
		return ExitConfig
	}
	if errs := cfg.CheckEnvironment(cfg.Repositories, os.LookupEnv); len(errs) > 0 {
		reportProblems(stderr, errs)
		return ExitConfig
	}
	if err := cfg.CheckStorage(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitConfig
	}
	ctx, stop := signalContext()
	defer stop()
	log := newLogger(stderr)
	log.Info("git-keepr starting", "version", Version)
	svc, err := buildService(ctx, cfg, log, true)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitConfig
	}
	svc.Serve(ctx)
	log.Info("shutdown complete")
	return ExitOK
}

// ---- sync ----

func cmdSync(args []string, stdout, stderr io.Writer) int {
	fs, cfgFlag := newFlags("sync", stderr)
	once := fs.Bool("once", false, "synchronize once and exit (required)")
	repoID := fs.String("repo", "", "only synchronize this repository id")
	if code, done := parse(fs, args); done {
		return code
	}
	if !*once {
		fmt.Fprintln(stderr, "sync requires --once (use 'git-keepr serve' for continuous operation)")
		return ExitUsage
	}
	cfg, ok := loadConfig(*cfgFlag, stderr)
	if !ok {
		return ExitConfig
	}
	repos := cfg.Repositories
	if *repoID != "" {
		r, found := cfg.Repo(*repoID)
		if !found {
			fmt.Fprintf(stderr, "error: unknown repository %q (configured: %s)\n", *repoID, strings.Join(cfg.RepoIDs(), ", "))
			return ExitUsage
		}
		repos = []config.Repository{r}
	}
	if errs := cfg.CheckEnvironment(repos, os.LookupEnv); len(errs) > 0 {
		reportProblems(stderr, errs)
		return ExitConfig
	}
	if err := cfg.CheckStorage(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitConfig
	}
	ctx, stop := signalContext()
	defer stop()
	svc, err := buildService(ctx, cfg, newLogger(stderr), true)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitConfig
	}
	results := svc.SyncMany(ctx, repos)
	return printSyncSummary(stdout, results)
}

func printSyncSummary(w io.Writer, results []service.Result) int {
	failed := 0
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RESULT\tREPOSITORY\tGIT\tS3\tBYTES\tDURATION")
	for _, r := range results {
		res := "ok"
		if !r.OK() {
			res = "FAILED"
			failed++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", res, r.RepoID, r.GitStatus, r.S3Status, r.Bytes, r.Duration.Round(time.Millisecond))
	}
	tw.Flush()
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(w, "\n%s: %s\n", r.RepoID, oneLine(r.Err.Error()))
		}
	}
	fmt.Fprintf(w, "\n%d of %d repositories synchronized\n", len(results)-failed, len(results))
	if failed > 0 {
		return ExitFailure
	}
	return ExitOK
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// ---- status ----

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs, cfgFlag := newFlags("status", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	cfg, ok := loadConfig(*cfgFlag, stderr)
	if !ok {
		return ExitConfig
	}
	layout := state.Layout{Root: cfg.Storage.Local.Root}
	problems := 0
	for i, r := range cfg.Repositories {
		st, err := state.Load(layout.StateFile(r.ID), r.ID)
		if err != nil {
			fmt.Fprintf(stderr, "warning: %v\n", err)
		}
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		if printRepoStatus(stdout, cfg, r, st) {
			problems++
		}
	}
	if problems > 0 {
		return ExitFailure
	}
	return ExitOK
}

func ts(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "-"
	}
	return s
}

// printRepoStatus prints one repository block; it returns true if an error is recorded.
func printRepoStatus(w io.Writer, cfg *config.Config, r config.Repository, st state.Repo) bool {
	fmt.Fprintf(w, "%s  (every %s)\n", r.ID, r.Every)
	g := st.Git
	fmt.Fprintf(w, "  git: %-8s last fetch %s  refs %d  digest %s  head %s  took %s\n",
		g.Status, ts(g.LastSuccess), g.RefCount, short(g.RefFingerprint), orDash(g.HeadRef), (time.Duration(g.DurationMS) * time.Millisecond))
	bad := g.LastError != ""
	if !cfg.Storage.S3.Enabled {
		fmt.Fprintf(w, "  s3:  %-8s\n", state.StatusDisabled)
	} else {
		s := st.S3
		fmt.Fprintf(w, "  s3:  %-8s last publish %s  digest %s  size %d  took %s\n",
			s.Status, ts(s.LastSuccess), short(s.PublishedFingerprint), s.Size, time.Duration(s.DurationMS)*time.Millisecond)
		if s.Status == state.StatusEmpty {
			fmt.Fprintf(w, "       %s\n", s.Message)
		}
		if s.LastError != "" {
			bad = true
			fmt.Fprintf(w, "  s3 last error: %s\n", oneLine(s.LastError))
		}
		if s.Status == state.StatusOK && s.PublishedFingerprint != g.RefFingerprint && g.RefFingerprint != "" {
			fmt.Fprintln(w, "  note: the S3 bundle is behind the local mirror; it will be updated on the next sync")
		}
	}
	if g.LastError != "" {
		fmt.Fprintf(w, "  git last error: %s\n", oneLine(g.LastError))
	}
	return bad
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- restore ----

func cmdRestore(args []string, stdout, stderr io.Writer) int {
	fs, cfgFlag := newFlags("restore", stderr)
	repoID := fs.String("repo", "", "repository id to restore (required)")
	from := fs.String("from", "", `source: "local" mirror or "s3" bundle (required)`)
	output := fs.String("output", "", "path of the new bare repository to create (required, must not exist)")
	if code, done := parse(fs, args); done {
		return code
	}
	if *repoID == "" || *from == "" || *output == "" {
		fmt.Fprintln(stderr, "restore requires --repo, --from and --output")
		return ExitUsage
	}
	if *from != service.FromLocal && *from != service.FromS3 {
		fmt.Fprintf(stderr, "error: --from must be %q or %q\n", service.FromLocal, service.FromS3)
		return ExitUsage
	}
	cfg, ok := loadConfig(*cfgFlag, stderr)
	if !ok {
		return ExitConfig
	}
	repo, found := cfg.Repo(*repoID)
	if !found {
		fmt.Fprintf(stderr, "error: unknown repository %q (configured: %s)\n", *repoID, strings.Join(cfg.RepoIDs(), ", "))
		return ExitUsage
	}
	out, err := filepath.Abs(*output)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitUsage
	}
	ctx, stop := signalContext()
	defer stop()
	// Restoring needs neither GitHub access nor a PAT; S3 credentials only for --from s3.
	svc, err := buildService(ctx, cfg, newLogger(stderr), *from == service.FromS3)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitConfig
	}
	res, err := svc.Restore(ctx, repo, *from, out)
	if err != nil {
		fmt.Fprintf(stderr, "restore failed: %v\n", err)
		return ExitFailure
	}
	fmt.Fprintf(stdout, "restored %s from %s to %s\n", repo.ID, res.Source, res.Output)
	fmt.Fprintf(stdout, "  refs: %d   HEAD: %s   fsck: ok\n", res.Refs, orDash(res.Head))
	if res.SHA256 != "" {
		fmt.Fprintf(stdout, "  bundle sha256 verified: %s\n", res.SHA256)
	}
	fmt.Fprintf(stdout, "check it with: git clone %s checkout-test\n", res.Output)
	return ExitOK
}

// ---- config validate ----

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs, cfgFlag := newFlags("config validate", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	path := configPath(*cfgFlag)
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return ExitConfig
	}
	var problems []error
	problems = append(problems, cfg.CheckEnvironment(cfg.Repositories, os.LookupEnv)...)
	if err := cfg.CheckStorage(); err != nil {
		problems = append(problems, err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		problems = append(problems, errors.New("the git executable was not found in PATH"))
	}
	if cfg.Storage.S3.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if _, err := newS3(ctx, cfg); err != nil {
			problems = append(problems, err)
		}
		cancel()
	}
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "configuration %s has problems:\n", path)
		reportProblems(stderr, problems)
		return ExitConfig
	}
	s3 := "disabled"
	if cfg.Storage.S3.Enabled {
		s3 = "enabled (bucket " + cfg.Storage.S3.Bucket + ")"
	}
	fmt.Fprintf(stdout, "configuration %s is valid: %d repositories, local root %s, s3 %s\n",
		path, len(cfg.Repositories), cfg.Storage.Local.Root, s3)
	return ExitOK
}
