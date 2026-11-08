// Package state owns the on-disk layout, atomic per-repository status files
// and the process-level repository locks.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Status values recorded per destination.
const (
	StatusOK       = "ok"
	StatusError    = "error"
	StatusEmpty    = "empty"    // repository has no commits: nothing to bundle yet
	StatusNever    = "never"    // not attempted yet
	StatusDisabled = "disabled" // reported (not persisted) when the destination is off
)

// Layout maps repository ids onto the local storage root. Ids must already be
// validated by the config package (no traversal, no collisions).
type Layout struct{ Root string }

func (l Layout) MirrorDir(id string) string {
	return filepath.Join(l.Root, "mirrors", filepath.FromSlash(id)+".git")
}
func (l Layout) StateFile(id string) string {
	return filepath.Join(l.Root, "state", filepath.FromSlash(id)+".json")
}
func (l Layout) LockFile(id string) string {
	return filepath.Join(l.Root, "locks", filepath.FromSlash(id)+".lock")
}
func (l Layout) StagingDir() string { return filepath.Join(l.Root, "staging") }

// Init creates the top-level directories with restrictive permissions.
func (l Layout) Init() error {
	for _, d := range []string{"", "mirrors", "state", "staging", "locks"} {
		if err := os.MkdirAll(filepath.Join(l.Root, d), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Git is the result of the most recent local-mirror synchronization.
type Git struct {
	Status         string    `json:"status"`
	LastAttempt    time.Time `json:"last_attempt,omitzero"`
	LastSuccess    time.Time `json:"last_success,omitzero"`
	RefFingerprint string    `json:"ref_fingerprint,omitempty"`
	RefCount       int       `json:"ref_count"`
	HeadRef        string    `json:"head_ref,omitempty"`
	DurationMS     int64     `json:"duration_ms"`
	LastError      string    `json:"last_error,omitempty"`
}

// S3 is the result of the most recent S3 publication. PublishedFingerprint is
// only ever advanced after a verified upload, independently of Git.RefFingerprint.
type S3 struct {
	Status               string    `json:"status"`
	LastAttempt          time.Time `json:"last_attempt,omitzero"`
	LastSuccess          time.Time `json:"last_success,omitzero"`
	PublishedFingerprint string    `json:"published_fingerprint,omitempty"`
	Key                  string    `json:"key,omitempty"`
	SHA256               string    `json:"sha256,omitempty"`
	Size                 int64     `json:"size"`
	DurationMS           int64     `json:"duration_ms"`
	Message              string    `json:"message,omitempty"`
	LastError            string    `json:"last_error,omitempty"`
}

// Repo is the content of state/<id>.json.
type Repo struct {
	Version int    `json:"version"`
	RepoID  string `json:"repo_id"`
	Git     Git    `json:"git"`
	S3      S3     `json:"s3"`
}

// New returns an empty state for id.
func New(id string) Repo {
	return Repo{Version: 1, RepoID: id, Git: Git{Status: StatusNever}, S3: S3{Status: StatusNever}}
}

// Load reads a state file. A missing file yields a fresh state. A corrupt file
// is moved aside and a fresh state returned, which at worst causes a republish.
func Load(path, id string) (Repo, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(id), nil
	}
	if err != nil {
		return New(id), err
	}
	var r Repo
	if err := json.Unmarshal(data, &r); err != nil || r.RepoID != id {
		aside := path + ".corrupt"
		_ = os.Rename(path, aside)
		return New(id), fmt.Errorf("state file unreadable, moved to %s and starting fresh", aside)
	}
	return r, nil
}

// Save writes the state atomically (temp file in the same directory, fsync, rename).
func Save(path string, r Repo) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// WriteFileAtomic replaces path with data so readers never see a partial file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
