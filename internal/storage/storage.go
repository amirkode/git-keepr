// Package storage defines the small destination interface used for the remote
// copy of a backup. Only S3-compatible storage implements it in v0.1.
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Head/Get when the object does not exist.
var ErrNotFound = errors.New("object not found")

// Metadata keys recorded with every published bundle.
const (
	MetaSHA256      = "sha256"
	MetaFingerprint = "ref-fingerprint"
	MetaRefsDigest  = "refs-digest"
	MetaHeadRef     = "head-ref"
	MetaVersion     = "git-keepr-version"
)

// Object describes a stored object.
type Object struct {
	Key      string
	Size     int64
	Metadata map[string]string
}

// Store is a bucket-like destination holding one current bundle per repository.
type Store interface {
	// Put uploads the file at path to key, replacing any existing object only
	// once the upload has fully completed.
	Put(ctx context.Context, key, path string, meta map[string]string) error
	// Head returns object information or ErrNotFound.
	Head(ctx context.Context, key string) (Object, error)
	// Get streams the object to w.
	Get(ctx context.Context, key string, w io.Writer) error
}
