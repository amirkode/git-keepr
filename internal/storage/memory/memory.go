// Package memory is an in-memory storage.Store with failure injection, used by tests.
package memory

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"

	"github.com/amirkode/git-keepr/internal/storage"
)

type object struct {
	data []byte
	meta map[string]string
}

// Store is safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	objs map[string]object

	// PutErr, if non-nil, makes every Put fail before storing anything.
	PutErr error
	// BeforePut, if set, runs at the start of Put; a non-nil error aborts the upload.
	BeforePut func(ctx context.Context) error
	// HeadErr, if non-nil, makes Head fail with that error.
	HeadErr error

	puts int
}

func New() *Store { return &Store{objs: map[string]object{}} }

func (s *Store) Put(ctx context.Context, key, path string, meta map[string]string) error {
	s.mu.Lock()
	hook, perr := s.BeforePut, s.PutErr
	s.mu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	if perr != nil {
		return perr
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m := map[string]string{}
	for k, v := range meta {
		m[k] = v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = object{data: data, meta: m}
	s.puts++
	return nil
}

func (s *Store) Head(ctx context.Context, key string) (storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.HeadErr != nil {
		return storage.Object{}, s.HeadErr
	}
	o, ok := s.objs[key]
	if !ok {
		return storage.Object{}, storage.ErrNotFound
	}
	return storage.Object{Key: key, Size: int64(len(o.data)), Metadata: o.meta}, nil
}

func (s *Store) Get(ctx context.Context, key string, w io.Writer) error {
	s.mu.Lock()
	o, ok := s.objs[key]
	s.mu.Unlock()
	if !ok {
		return storage.ErrNotFound
	}
	_, err := io.Copy(w, bytes.NewReader(o.data))
	return err
}

// Delete removes an object (simulating external loss).
func (s *Store) Delete(key string) { s.mu.Lock(); delete(s.objs, key); s.mu.Unlock() }

// Bytes returns the object's content.
func (s *Store) Bytes(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objs[key]
	return o.data, ok
}

// Keys lists stored keys.
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var k []string
	for key := range s.objs {
		k = append(k, key)
	}
	return k
}

// Corrupt replaces the stored bytes but keeps the recorded metadata.
func (s *Store) Corrupt(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objs[key]
	o.data = data
	s.objs[key] = o
}

// SetMetadata replaces stored metadata (simulating tampering).
func (s *Store) SetMetadata(key string, meta map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objs[key]
	o.meta = meta
	s.objs[key] = o
}

// Puts returns how many uploads completed.
func (s *Store) Puts() int { s.mu.Lock(); defer s.mu.Unlock(); return s.puts }
