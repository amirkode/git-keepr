package s3

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/amirkode/git-keepr/internal/storage"
	"github.com/amirkode/git-keepr/internal/storage/s3test"
)

func newStore(t *testing.T, srv *s3test.Server, bucket string) *Store {
	t.Helper()
	s, err := New(context.Background(), Options{
		Bucket: bucket, Region: "auto", Endpoint: srv.URL, PathStyle: true, MaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider("AKIATESTDUMMY", "dummy-secret", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeTmp(t *testing.T, content string) string {
	p := filepath.Join(t.TempDir(), "f.bundle")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPutHeadGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv := s3test.New(t, "bkt")
	s := newStore(t, srv, "bkt")
	key := "repositories/github/o/r/latest.bundle"

	if _, err := s.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	meta := map[string]string{storage.MetaSHA256: "abc123", storage.MetaHeadRef: "refs/heads/feat ü", storage.MetaFingerprint: "fp"}
	if err := s.Put(ctx, key, writeTmp(t, "bundle-bytes"), meta); err != nil {
		t.Fatal(err)
	}
	obj, err := s.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size != int64(len("bundle-bytes")) || obj.Metadata[storage.MetaSHA256] != "abc123" || obj.Metadata[storage.MetaHeadRef] != "refs/heads/feat ü" {
		t.Fatalf("unexpected object %+v", obj)
	}
	var sb strings.Builder
	if err := s.Get(ctx, key, &sb); err != nil || sb.String() != "bundle-bytes" {
		t.Fatalf("get: %q %v", sb.String(), err)
	}
	if err := s.Get(ctx, "missing", &sb); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for get, got %v", err)
	}
}

func TestFailedUploadKeepsPreviousObject(t *testing.T) {
	ctx := context.Background()
	srv := s3test.New(t, "bkt")
	s := newStore(t, srv, "bkt")
	s.Put(ctx, "k", writeTmp(t, "v1"), nil)

	srv.FailNextPuts(10)
	if err := s.Put(ctx, "k", writeTmp(t, "v2-longer"), nil); err == nil {
		t.Fatal("expected upload failure")
	}
	data, _, _ := srv.Object("k")
	if string(data) != "v1" {
		t.Fatalf("previous object was damaged: %q", data)
	}
}

func TestWrongBucketIsAnError(t *testing.T) {
	srv := s3test.New(t, "bkt")
	s := newStore(t, srv, "other")
	if err := s.Put(context.Background(), "k", writeTmp(t, "x"), nil); err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

func TestCheckCredentials(t *testing.T) {
	ctx := context.Background()
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	s, err := New(ctx, Options{Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckCredentials(ctx); err == nil {
		t.Fatal("expected missing-credential error")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATESTDUMMY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "dummy-secret")
	s, _ = New(ctx, Options{Bucket: "b", Region: "us-east-1"})
	if err := s.CheckCredentials(ctx); err != nil {
		t.Fatalf("credentials from env should resolve: %v", err)
	}
	var _ aws.CredentialsProvider = s.creds
}

func TestNewRequiresBucketAndRegion(t *testing.T) {
	if _, err := New(context.Background(), Options{Region: "x"}); err == nil {
		t.Fatal("bucket required")
	}
	if _, err := New(context.Background(), Options{Bucket: "x"}); err == nil {
		t.Fatal("region required")
	}
}
