package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/amirkode/git-keepr/internal/config"
	"github.com/amirkode/git-keepr/internal/gitmirror"
	"github.com/amirkode/git-keepr/internal/state"
	"github.com/amirkode/git-keepr/internal/storage"
)

// Restore sources.
const (
	FromLocal = "local"
	FromS3    = "s3"
)

// RestoreResult describes a completed, verified restore.
type RestoreResult struct {
	Output      string
	Source      string
	Refs        int
	Head        string
	Fingerprint string
	SHA256      string // S3 only: verified bundle checksum
}

// Restore recreates a bare repository at output from the local mirror or the S3
// bundle. It needs neither GitHub access nor a PAT. It refuses to touch an
// existing output path, verifies checksums/refs and runs git fsck.
func (s *Service) Restore(ctx context.Context, repo config.Repository, from, output string) (RestoreResult, error) {
	switch from {
	case FromLocal:
		return s.restoreLocal(ctx, repo, output)
	case FromS3:
		return s.restoreS3(ctx, repo, output)
	}
	return RestoreResult{}, fmt.Errorf("unknown restore source %q (use %q or %q)", from, FromLocal, FromS3)
}

func (s *Service) restoreLocal(ctx context.Context, repo config.Repository, output string) (RestoreResult, error) {
	mirror := s.layout.MirrorDir(repo.ID)
	if !isDir(mirror) {
		return RestoreResult{}, fmt.Errorf("no local mirror for %s at %s", repo.ID, mirror)
	}
	lock, err := state.TryLock(s.layout.LockFile(repo.ID))
	if errors.Is(err, state.ErrLocked) {
		return RestoreResult{}, errors.New("a sync of this repository is running; wait for it to finish and retry")
	}
	if err != nil {
		return RestoreResult{}, err
	}
	defer lock.Release()

	want, err := s.Git.Snapshot(ctx, mirror)
	if err != nil {
		return RestoreResult{}, err
	}
	if want.Empty() {
		return RestoreResult{}, errors.New("local mirror contains no refs (the source repository was empty)")
	}
	got, err := s.Git.RestoreFromMirror(ctx, mirror, output, want.RefsDigest)
	if err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Output: output, Source: FromLocal, Refs: got.RefCount, Head: got.Head, Fingerprint: got.Fingerprint}, nil
}

func (s *Service) restoreS3(ctx context.Context, repo config.Repository, output string) (RestoreResult, error) {
	if s.S3 == nil {
		return RestoreResult{}, errors.New("S3 is not enabled in the configuration")
	}
	if _, err := os.Lstat(output); err == nil {
		return RestoreResult{}, fmt.Errorf("%w: %s", gitmirror.ErrExists, output)
	}
	key := s.Cfg.S3Key(repo.ID)
	obj, err := s.S3.Head(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return RestoreResult{}, fmt.Errorf("no bundle at s3 key %s", key)
	}
	if err != nil {
		return RestoreResult{}, err
	}
	wantSum := obj.Metadata[storage.MetaSHA256]
	if wantSum == "" {
		return RestoreResult{}, fmt.Errorf("object %s has no %s metadata; refusing to restore an unverifiable bundle", key, storage.MetaSHA256)
	}

	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return RestoreResult{}, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(output), ".git-keepr-download-")
	if err != nil {
		return RestoreResult{}, err
	}
	defer os.RemoveAll(dir)
	bundle := filepath.Join(dir, "latest.bundle")
	f, err := os.OpenFile(bundle, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := s.S3.Get(ctx, key, f); err != nil {
		f.Close()
		return RestoreResult{}, err
	}
	if err := f.Close(); err != nil {
		return RestoreResult{}, err
	}
	sum, size, err := gitmirror.HashFile(bundle)
	if err != nil {
		return RestoreResult{}, err
	}
	if sum != wantSum || size != obj.Size {
		return RestoreResult{}, fmt.Errorf("downloaded bundle failed checksum verification (sha256 %s, expected %s)", sum, wantSum)
	}
	got, err := s.Git.RestoreFromBundle(ctx, bundle, output, obj.Metadata[storage.MetaHeadRef], obj.Metadata[storage.MetaRefsDigest])
	if err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Output: output, Source: FromS3, Refs: got.RefCount, Head: got.Head, Fingerprint: got.Fingerprint, SHA256: sum}, nil
}
