package state

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLayoutPaths(t *testing.T) {
	l := Layout{Root: "/data"}
	if l.MirrorDir("github/o/r") != "/data/mirrors/github/o/r.git" ||
		l.StateFile("github/o/r") != "/data/state/github/o/r.json" ||
		l.LockFile("github/o/r") != "/data/locks/github/o/r.lock" ||
		l.StagingDir() != "/data/staging" {
		t.Fatal("unexpected layout")
	}
}

func TestSaveLoadRoundTripAndPerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state", "github", "o", "r.json")
	r := New("github/o/r")
	r.Git = Git{Status: StatusOK, LastSuccess: time.Now().UTC(), RefFingerprint: "abc", RefCount: 3}
	r.S3.PublishedFingerprint = "abc"
	if err := Save(p, r); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v", st.Mode().Perm())
	}
	got, err := Load(p, "github/o/r")
	if err != nil {
		t.Fatal(err)
	}
	if got.Git.RefFingerprint != "abc" || got.S3.PublishedFingerprint != "abc" || got.Git.RefCount != 3 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	r, err := Load(p, "github/o/r")
	if err != nil || r.Git.Status != StatusNever {
		t.Fatalf("missing: %+v %v", r, err)
	}
	os.WriteFile(p, []byte("{not json"), 0o600)
	r, err = Load(p, "github/o/r")
	if err == nil || r.Git.Status != StatusNever {
		t.Fatalf("corrupt should warn and reset: %+v %v", r, err)
	}
	if _, err := os.Stat(p + ".corrupt"); err != nil {
		t.Fatal("corrupt file should be preserved aside")
	}
}

func TestTryLockExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "locks", "a.lock")
	l1, err := TryLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(p); err != ErrLocked {
		t.Fatalf("second lock should fail with ErrLocked, got %v", err)
	}
	l1.Release()
	l2, err := TryLock(p)
	if err != nil {
		t.Fatalf("lock should be free after release: %v", err)
	}
	l2.Release()
}

func TestTryLockMutualExclusionUnderContention(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.lock")
	var inside, violations, acquired int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				l, err := TryLock(p)
				if err != nil {
					continue
				}
				atomic.AddInt32(&acquired, 1)
				if atomic.AddInt32(&inside, 1) != 1 {
					atomic.AddInt32(&violations, 1)
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt32(&inside, -1)
				l.Release()
			}
		}()
	}
	wg.Wait()
	if violations != 0 || acquired == 0 {
		t.Fatalf("violations=%d acquired=%d", violations, acquired)
	}
}
