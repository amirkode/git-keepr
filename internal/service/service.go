// Package service orchestrates one synchronization of a repository: mirror
// from GitHub, then (optionally) publish a verified bundle to S3, with each
// destination's success tracked independently.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/amirkode/git-keepr/internal/config"
	"github.com/amirkode/git-keepr/internal/credentials"
	"github.com/amirkode/git-keepr/internal/gitmirror"
	"github.com/amirkode/git-keepr/internal/scheduler"
	"github.com/amirkode/git-keepr/internal/state"
	"github.com/amirkode/git-keepr/internal/storage"
)

// Result statuses reported per destination.
const (
	StatusOK        = "ok"        // mirror updated / bundle published
	StatusUnchanged = "unchanged" // S3 already holds the current bundle
	StatusEmpty     = "empty"     // repository has no commits yet
	StatusError     = "error"
	StatusSkipped   = "skipped"
	StatusDisabled  = "disabled"
)

// Service wires configuration, git, state and (optional) S3 storage.
type Service struct {
	Cfg     *config.Config
	Git     *gitmirror.Git
	S3      storage.Store // nil when S3 is disabled
	Log     *slog.Logger
	Version string

	// LookupEnv resolves PAT variables (defaults to os.LookupEnv).
	LookupEnv func(string) (string, bool)
	// Now is the clock (defaults to time.Now).
	Now func() time.Time

	layout state.Layout
}

// New builds a Service. store may be nil when S3 is disabled.
func New(cfg *config.Config, g *gitmirror.Git, store storage.Store, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		Cfg: cfg, Git: g, S3: store, Log: log, Version: "dev",
		LookupEnv: os.LookupEnv, Now: func() time.Time { return time.Now().UTC() },
		layout: state.Layout{Root: cfg.Storage.Local.Root},
	}
}

// Layout exposes the local storage layout.
func (s *Service) Layout() state.Layout { return s.layout }

// Result summarises one repository synchronization.
type Result struct {
	RepoID    string
	Duration  time.Duration
	GitStatus string
	S3Status  string
	Bytes     int64 // bundle bytes uploaded in this run
	Busy      bool  // skipped because another sync holds the repository lock
	GitErr    error
	S3Err     error
	Err       error // non-nil when a required destination failed
}

// OK reports whether every required destination succeeded.
func (r Result) OK() bool { return r.Err == nil }

// SyncRepo performs one full synchronization. It never panics on repository
// errors; failures are reported in the Result and persisted in the state file.
func (s *Service) SyncRepo(ctx context.Context, repo config.Repository) Result {
	start := s.Now()
	res := Result{RepoID: repo.ID, GitStatus: StatusSkipped, S3Status: StatusSkipped}
	if s.S3 == nil {
		res.S3Status = StatusDisabled
	}
	s.syncLocked(ctx, repo, &res)
	res.Duration = s.Now().Sub(start)
	s.logResult(res)
	return res
}

func (s *Service) syncLocked(ctx context.Context, repo config.Repository, res *Result) {
	if err := s.layout.Init(); err != nil {
		res.Err = fmt.Errorf("prepare local storage: %w", err)
		return
	}
	lock, err := state.TryLock(s.layout.LockFile(repo.ID))
	if errors.Is(err, state.ErrLocked) {
		res.Busy = true
		res.Err = errors.New("another sync of this repository is already running; skipped")
		return
	}
	if err != nil {
		res.Err = fmt.Errorf("acquire lock: %w", err)
		return
	}
	defer lock.Release()

	stFile := s.layout.StateFile(repo.ID)
	st, err := state.Load(stFile, repo.ID)
	if err != nil {
		s.Log.Warn("state file problem", "repo_id", repo.ID, "error", err)
	}

	res.GitErr = s.syncGit(ctx, repo, &st)
	res.GitStatus = st.Git.Status
	if err := state.Save(stFile, st); err != nil {
		res.Err = fmt.Errorf("persist state: %w", err)
		return
	}

	// S3 is evaluated from the local mirror even when the fetch failed, so a
	// pending or failed publication is retried without contacting GitHub.
	res.S3Err = s.syncS3(ctx, repo, &st, res)
	if err := state.Save(stFile, st); err != nil {
		res.Err = errors.Join(res.Err, fmt.Errorf("persist state: %w", err))
	}

	var errs []error
	if res.GitErr != nil {
		errs = append(errs, fmt.Errorf("git: %w", res.GitErr))
	}
	if res.S3Err != nil {
		errs = append(errs, fmt.Errorf("s3: %w", res.S3Err))
	}
	res.Err = errors.Join(append(errs, res.Err)...)
}

func (s *Service) token(repo config.Repository) (string, error) {
	env := s.Cfg.TokenEnvFor(repo)
	v, ok := s.LookupEnv(env)
	if !ok || strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("environment variable %s (credential %q) is not set or empty", env, repo.Credential)
	}
	return strings.TrimSpace(v), nil
}

func isDir(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

// syncGit clones or fetches the local mirror and records the outcome in st.Git.
func (s *Service) syncGit(ctx context.Context, repo config.Repository, st *state.Repo) (err error) {
	t0 := s.Now()
	st.Git.LastAttempt = t0
	var token string
	defer func() {
		st.Git.DurationMS = s.Now().Sub(t0).Milliseconds()
		if err != nil {
			st.Git.Status = state.StatusError
			st.Git.LastError = credentials.Redact(err.Error(), token)
		}
	}()

	token, err = s.token(repo)
	if err != nil {
		return err
	}
	auth := &credentials.Auth{Token: token}
	mirror := s.layout.MirrorDir(repo.ID)

	if !isDir(mirror) {
		if err := s.cloneNew(ctx, repo, mirror, auth); err != nil {
			return err
		}
	} else if err := s.Git.Fetch(ctx, mirror, auth); err != nil {
		return err
	}

	// Follow the source's default branch (including renames) when it is known.
	if head, herr := s.Git.SourceHead(ctx, mirror, auth); herr != nil {
		s.Log.Warn("could not query source default branch", "repo_id", repo.ID, "error", credentials.Redact(herr.Error(), token))
	} else if head != "" && s.Git.HasRef(ctx, mirror, head) {
		if cur, _ := s.Git.Snapshot(ctx, mirror); cur.Head != head {
			if err := s.Git.SetHead(ctx, mirror, head); err != nil {
				return err
			}
		}
	}

	snap, err := s.Git.Snapshot(ctx, mirror)
	if err != nil {
		return err
	}
	st.Git.Status = state.StatusOK
	if snap.Empty() {
		st.Git.Status = state.StatusEmpty
	}
	st.Git.LastSuccess = s.Now()
	st.Git.LastError = ""
	st.Git.RefFingerprint = snap.Fingerprint
	st.Git.RefCount = snap.RefCount
	st.Git.HeadRef = snap.Head
	return nil
}

// cloneNew clones into a temporary sibling and renames it into place only on
// success, so an interrupted first sync never leaves a partial mirror behind.
func (s *Service) cloneNew(ctx context.Context, repo config.Repository, mirror string, auth *credentials.Auth) error {
	parent := filepath.Dir(mirror)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	// Stale temporaries can only come from a crash; we hold the repo lock.
	stale, _ := filepath.Glob(mirror + ".tmp-*")
	for _, p := range stale {
		_ = os.RemoveAll(p)
	}
	tmp, err := os.MkdirTemp(parent, filepath.Base(mirror)+".tmp-")
	if err != nil {
		return err
	}
	// git clone requires a non-existent or empty directory; MkdirTemp gives an empty one.
	if err := s.Git.CloneMirror(ctx, repo.URL, tmp, auth); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, mirror); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

// syncS3 publishes the current bundle when needed and records the outcome in st.S3.
func (s *Service) syncS3(ctx context.Context, repo config.Repository, st *state.Repo, res *Result) (err error) {
	if s.S3 == nil {
		res.S3Status = StatusDisabled
		return nil
	}
	mirror := s.layout.MirrorDir(repo.ID)
	if !isDir(mirror) {
		res.S3Status = StatusSkipped // nothing mirrored yet
		return nil
	}
	t0 := s.Now()
	st.S3.LastAttempt = t0
	defer func() {
		st.S3.DurationMS = s.Now().Sub(t0).Milliseconds()
		if err != nil {
			// Previous published fingerprint/checksum stay untouched: the last completed object is still there.
			st.S3.Status = state.StatusError
			st.S3.LastError = credentials.Redact(err.Error())
			res.S3Status = StatusError
		}
	}()

	snap, err := s.Git.Snapshot(ctx, mirror)
	if err != nil {
		return err
	}
	if snap.Empty() {
		st.S3.Status = state.StatusEmpty
		st.S3.Message = "repository has no commits yet; S3 backup begins after its first commit"
		st.S3.LastError = ""
		res.S3Status = StatusEmpty
		return nil
	}
	key := s.Cfg.S3Key(repo.ID)

	if st.S3.Status == state.StatusOK && st.S3.PublishedFingerprint == snap.Fingerprint && st.S3.Key == key {
		obj, herr := s.S3.Head(ctx, key)
		switch {
		case herr == nil && obj.Size == st.S3.Size && obj.Metadata[storage.MetaSHA256] == st.S3.SHA256:
			st.S3.Message = "unchanged since last publish"
			st.S3.LastError = ""
			res.S3Status = StatusUnchanged
			return nil
		case herr == nil:
			s.Log.Warn("published S3 object does not match recorded size/checksum; republishing", "repo_id", repo.ID)
		case errors.Is(herr, storage.ErrNotFound):
			s.Log.Warn("published S3 object is missing; republishing", "repo_id", repo.ID)
		default:
			s.Log.Warn("could not verify published S3 object; republishing", "repo_id", repo.ID, "error", credentials.Redact(herr.Error()))
		}
	}

	dir, err := os.MkdirTemp(s.layout.StagingDir(), "bundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	bundle := filepath.Join(dir, "latest.bundle")
	if err := s.Git.CreateBundle(ctx, mirror, bundle); err != nil {
		return err
	}
	if err := s.Git.VerifyBundle(ctx, mirror, bundle); err != nil {
		return fmt.Errorf("created bundle failed verification: %w", err)
	}
	sum, size, err := gitmirror.HashFile(bundle)
	if err != nil {
		return err
	}
	meta := map[string]string{
		storage.MetaSHA256:      sum,
		storage.MetaFingerprint: snap.Fingerprint,
		storage.MetaRefsDigest:  snap.RefsDigest,
		storage.MetaHeadRef:     snap.Head,
		storage.MetaVersion:     s.Version,
	}
	if err := s.S3.Put(ctx, key, bundle, meta); err != nil {
		return err
	}
	obj, err := s.S3.Head(ctx, key)
	if err != nil {
		return fmt.Errorf("completed-object check failed: %w", err)
	}
	if obj.Size != size || obj.Metadata[storage.MetaSHA256] != sum {
		return fmt.Errorf("completed-object check failed: stored size/checksum differ from the uploaded bundle (size %d, want %d)", obj.Size, size)
	}

	st.S3 = state.S3{
		Status: state.StatusOK, LastAttempt: t0, LastSuccess: s.Now(),
		PublishedFingerprint: snap.Fingerprint, Key: key, SHA256: sum, Size: size,
		Message: "published",
	}
	res.S3Status = StatusOK
	res.Bytes = size
	return nil
}

func (s *Service) logResult(r Result) {
	attrs := []any{
		"repo_id", r.RepoID, "duration", r.Duration.Round(time.Millisecond).String(),
		"git_status", r.GitStatus, "s3_status", r.S3Status, "bytes", r.Bytes,
	}
	if r.Err != nil {
		s.Log.Error("sync finished with errors", append(attrs, "error", credentials.Redact(r.Err.Error()))...)
		return
	}
	s.Log.Info("sync finished", append(attrs, "error", "")...)
}

// SyncMany synchronizes the given repositories with bounded concurrency and
// returns results in input order. One repository's failure never affects another.
func (s *Service) SyncMany(ctx context.Context, repos []config.Repository) []Result {
	results := make([]Result, len(repos))
	sem := make(chan struct{}, max(1, s.Cfg.Scheduler.MaxConcurrency))
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = Result{RepoID: r.ID, GitStatus: StatusSkipped, S3Status: StatusSkipped, Err: ctx.Err()}
				return
			}
			results[i] = s.SyncRepo(ctx, r)
		}()
	}
	wg.Wait()
	return results
}

// Serve runs the in-process scheduler until ctx is cancelled.
func (s *Service) Serve(ctx context.Context) {
	jobs := make([]scheduler.Job, 0, len(s.Cfg.Repositories))
	for _, r := range s.Cfg.Repositories {
		jobs = append(jobs, scheduler.Job{ID: r.ID, Interval: r.Every, Run: func(ctx context.Context) { s.SyncRepo(ctx, r) }})
	}
	s.Log.Info("scheduler started", "repositories", len(jobs), "max_concurrency", s.Cfg.Scheduler.MaxConcurrency, "sync_on_start", s.Cfg.Scheduler.OnStart, "s3_enabled", s.S3 != nil)
	scheduler.Run(ctx, jobs, scheduler.Options{MaxConcurrency: s.Cfg.Scheduler.MaxConcurrency, OnStart: s.Cfg.Scheduler.OnStart, Log: s.Log})
	s.Log.Info("scheduler stopped")
}
