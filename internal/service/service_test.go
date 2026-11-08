package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amirkode/git-keepr/internal/config"
	"github.com/amirkode/git-keepr/internal/credentials"
	"github.com/amirkode/git-keepr/internal/gitmirror"
	"github.com/amirkode/git-keepr/internal/state"
	"github.com/amirkode/git-keepr/internal/storage"
	"github.com/amirkode/git-keepr/internal/storage/memory"
	"github.com/amirkode/git-keepr/internal/testutil"
)

const testToken = "dummy-test-token-XYZ123"

func TestMain(m *testing.M) {
	credentials.MaybeRunAskpass()
	os.Exit(m.Run())
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	t      *testing.T
	src    *testutil.Repo
	cfg    *config.Config
	svc    *Service
	mem    *memory.Store
	repo   config.Repository
	logs   *syncBuf
	argv   *syncBuf
	clones *int
}

func newCfg(t *testing.T, root string, repos ...config.Repository) *config.Config {
	t.Setenv("GITKEEPR_TEST_PAT", testToken)
	return &config.Config{
		Version:      1,
		Scheduler:    config.Scheduler{Interval: time.Hour, MaxConcurrency: 2, OnStart: true},
		Credentials:  map[string]config.Credential{"personal": {Type: "github_pat", TokenEnv: "GITKEEPR_TEST_PAT"}},
		Repositories: repos,
		Storage: config.Storage{
			Local: config.LocalStorage{Root: root},
			S3:    config.S3Storage{Enabled: true, Bucket: "b", Prefix: "repositories/"},
		},
	}
}

func newService(t *testing.T, cfg *config.Config, store storage.Store, allow string) (*Service, *syncBuf, *syncBuf, *int) {
	g := gitmirror.New(t.TempDir())
	g.AllowProtocols = allow
	argv, logs := &syncBuf{}, &syncBuf{}
	clones := new(int)
	var mu sync.Mutex
	g.Trace = func(a []string) {
		mu.Lock()
		defer mu.Unlock()
		argv.Write([]byte(strings.Join(a, " ") + "\n"))
		for _, x := range a {
			if x == "clone" {
				*clones++
			}
		}
	}
	svc := New(cfg, g, store, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return svc, argv, logs, clones
}

func newEnv(t *testing.T) *env {
	t.Helper()
	src := testutil.Populated(t)
	repo := config.Repository{ID: "github/test/one", URL: src.URL(), Credential: "personal", Every: time.Hour}
	cfg := newCfg(t, filepath.Join(t.TempDir(), "data"), repo)
	mem := memory.New()
	svc, argv, logs, clones := newService(t, cfg, mem, "file")
	return &env{t: t, src: src, cfg: cfg, svc: svc, mem: mem, repo: repo, logs: logs, argv: argv, clones: clones}
}

func (e *env) sync() Result { e.t.Helper(); return e.svc.SyncRepo(context.Background(), e.repo) }

func (e *env) state() state.Repo {
	e.t.Helper()
	st, err := state.Load(e.svc.Layout().StateFile(e.repo.ID), e.repo.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) key() string { return e.cfg.S3Key(e.repo.ID) }

func (e *env) fsck() {
	e.t.Helper()
	if err := e.svc.Git.Fsck(context.Background(), e.svc.Layout().MirrorDir(e.repo.ID)); err != nil {
		e.t.Fatalf("mirror fsck: %v", err)
	}
}

func requireOK(t *testing.T, r Result) {
	t.Helper()
	if !r.OK() {
		t.Fatalf("sync failed: %v", r.Err)
	}
}

func TestFirstSyncPublishesAndUnchangedSkipsUpload(t *testing.T) {
	e := newEnv(t)
	r := e.sync()
	requireOK(t, r)
	if r.GitStatus != StatusOK || r.S3Status != StatusOK || r.Bytes == 0 {
		t.Fatalf("unexpected result %+v", r)
	}
	if e.mem.Puts() != 1 {
		t.Fatalf("puts = %d", e.mem.Puts())
	}
	if e.key() != "repositories/github/test/one/latest.bundle" {
		t.Fatalf("key = %s", e.key())
	}
	st := e.state()
	if st.Git.Status != state.StatusOK || st.S3.Status != state.StatusOK || st.S3.PublishedFingerprint != st.Git.RefFingerprint || st.S3.SHA256 == "" {
		t.Fatalf("state = %+v", st)
	}
	if fi, _ := os.Stat(e.svc.Layout().StateFile(e.repo.ID)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file perm %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(e.svc.Layout().MirrorDir(e.repo.ID)); fi.Mode().Perm()&0o007 != 0 {
		t.Logf("note: mirror dir mode %v (process umask governs this)", fi.Mode().Perm())
	}

	// unchanged upstream: incremental fetch, no second upload
	r = e.sync()
	requireOK(t, r)
	if r.S3Status != StatusUnchanged || e.mem.Puts() != 1 || r.Bytes != 0 {
		t.Fatalf("expected unchanged, got %+v puts=%d", r, e.mem.Puts())
	}
	if *e.clones != 1 {
		t.Fatalf("repository was cloned %d times, want 1", *e.clones)
	}

	// upstream change: new bundle
	e.src.Commit("z.txt", "z")
	r = e.sync()
	requireOK(t, r)
	if r.S3Status != StatusOK || e.mem.Puts() != 2 {
		t.Fatalf("expected republish, got %+v puts=%d", r, e.mem.Puts())
	}
}

func TestMissingOrInconsistentS3ObjectIsRepublished(t *testing.T) {
	e := newEnv(t)
	requireOK(t, e.sync())

	e.mem.Delete(e.key())
	r := e.sync()
	requireOK(t, r)
	if r.S3Status != StatusOK || e.mem.Puts() != 2 {
		t.Fatalf("missing object must be republished: %+v puts=%d", r, e.mem.Puts())
	}
	if _, ok := e.mem.Bytes(e.key()); !ok {
		t.Fatal("object still missing")
	}

	// recorded metadata no longer matches (tampered / replaced object)
	e.mem.SetMetadata(e.key(), map[string]string{storage.MetaSHA256: "deadbeef"})
	r = e.sync()
	requireOK(t, r)
	if r.S3Status != StatusOK || e.mem.Puts() != 3 {
		t.Fatalf("inconsistent object must be republished: %+v puts=%d", r, e.mem.Puts())
	}
}

func TestS3FailureThenRecoveryKeepsLocalMirrorAndRetries(t *testing.T) {
	e := newEnv(t)
	e.mem.PutErr = errors.New("simulated S3 outage")

	r := e.sync()
	if r.OK() || r.S3Err == nil || r.GitErr != nil {
		t.Fatalf("expected S3-only failure, got %+v", r)
	}
	if r.GitStatus != StatusOK || r.S3Status != StatusError {
		t.Fatalf("statuses: %+v", r)
	}
	e.fsck() // local mirror usable
	st := e.state()
	if st.Git.Status != state.StatusOK || st.S3.Status != state.StatusError || st.S3.PublishedFingerprint != "" || st.S3.LastError == "" {
		t.Fatalf("failed S3 must not advance published state: %+v", st.S3)
	}

	// restart simulation: fresh Service, same data dir, upstream unchanged, S3 healthy again
	e.mem.PutErr = nil
	svc2, _, _, clones2 := newService(t, e.cfg, e.mem, "file")
	r = svc2.SyncRepo(context.Background(), e.repo)
	requireOK(t, r)
	if r.S3Status != StatusOK || e.mem.Puts() != 1 {
		t.Fatalf("retry must publish even though refs are unchanged: %+v puts=%d", r, e.mem.Puts())
	}
	if *clones2 != 0 {
		t.Fatal("retrying S3 must not re-clone the repository")
	}
	if st := e.state(); st.S3.Status != state.StatusOK || st.S3.PublishedFingerprint != st.Git.RefFingerprint || st.S3.LastError != "" {
		t.Fatalf("state after recovery: %+v", st.S3)
	}
}

func TestFailedUpdateKeepsLastCompletedObject(t *testing.T) {
	e := newEnv(t)
	requireOK(t, e.sync())
	before, _ := e.mem.Bytes(e.key())
	oldFP := e.state().S3.PublishedFingerprint

	e.src.Commit("n.txt", "n")
	e.mem.PutErr = errors.New("upload interrupted")
	r := e.sync()
	if r.OK() || r.S3Status != StatusError {
		t.Fatalf("expected S3 failure: %+v", r)
	}
	after, ok := e.mem.Bytes(e.key())
	if !ok || !bytes.Equal(before, after) {
		t.Fatal("previous completed S3 object must remain untouched")
	}
	st := e.state()
	if st.S3.PublishedFingerprint != oldFP || st.Git.RefFingerprint == oldFP {
		t.Fatal("git fingerprint advanced, published fingerprint must not")
	}
	e.mem.PutErr = nil
	requireOK(t, e.sync())
	if now, _ := e.mem.Bytes(e.key()); bytes.Equal(now, before) {
		t.Fatal("recovery should have published the new bundle")
	}
}

func TestSourceUnavailableStillRetriesPendingS3FromLocalMirror(t *testing.T) {
	e := newEnv(t)
	e.mem.PutErr = errors.New("outage")
	e.sync()
	e.mem.PutErr = nil
	os.RemoveAll(e.src.Dir) // source gone / account lost

	r := e.sync()
	if r.GitErr == nil || r.GitStatus != StatusError || r.OK() {
		t.Fatalf("git failure must be reported: %+v", r)
	}
	if r.S3Status != StatusOK || e.mem.Puts() != 1 {
		t.Fatalf("pending S3 publish should proceed from the local mirror: %+v", r)
	}
	e.fsck()
	if st := e.state(); st.Git.Status != state.StatusError || st.Git.LastSuccess.IsZero() || st.Git.RefFingerprint == "" {
		t.Fatalf("last successful fetch info must be retained: %+v", st.Git)
	}
}

func TestTwoRepositoriesFailureIsolation(t *testing.T) {
	good := testutil.Populated(t)
	rA := config.Repository{ID: "github/test/good", URL: good.URL(), Credential: "personal", Every: time.Hour}
	rB := config.Repository{ID: "github/test/broken", URL: "file://" + filepath.Join(t.TempDir(), "does-not-exist"), Credential: "personal", Every: time.Hour}
	cfg := newCfg(t, filepath.Join(t.TempDir(), "data"), rB, rA)
	mem := memory.New()
	svc, _, _, _ := newService(t, cfg, mem, "file")

	res := svc.SyncMany(context.Background(), cfg.Repositories)
	if res[0].RepoID != rB.ID || res[0].OK() || res[0].GitStatus != StatusError {
		t.Fatalf("broken repo should fail: %+v", res[0])
	}
	if !res[1].OK() || res[1].S3Status != StatusOK {
		t.Fatalf("good repo must be unaffected: %+v", res[1])
	}
	if _, ok := mem.Bytes(cfg.S3Key(rA.ID)); !ok {
		t.Fatal("good repo has no S3 bundle")
	}
	if _, ok := mem.Bytes(cfg.S3Key(rB.ID)); ok {
		t.Fatal("broken repo must not have an S3 bundle")
	}
	if _, err := os.Stat(svc.Layout().MirrorDir(rB.ID)); err == nil {
		t.Fatal("failed first clone must not leave a mirror directory")
	}
	if ents, _ := filepath.Glob(filepath.Join(svc.Layout().Root, "mirrors", "github", "test", "*")); len(ents) != 1 {
		t.Fatalf("unexpected leftovers in mirrors: %v", ents)
	}
}

func TestConcurrentSyncsNeverShareAMirror(t *testing.T) {
	e := newEnv(t)
	other, _, _, _ := newService(t, e.cfg, e.mem, "file") // second "process" (e.g. sync --once vs serve)

	// lock held elsewhere -> busy, nothing touched
	lock, err := state.TryLock(e.svc.Layout().LockFile(e.repo.ID))
	if err != nil {
		t.Fatal(err)
	}
	r := e.sync()
	if !r.Busy || r.OK() {
		t.Fatalf("expected busy: %+v", r)
	}
	if _, err := os.Stat(e.svc.Layout().MirrorDir(e.repo.ID)); err == nil {
		t.Fatal("busy sync must not touch the mirror")
	}
	lock.Release()

	var wg sync.WaitGroup
	results := make([]Result, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc := e.svc
			if i%2 == 1 {
				svc = other
			}
			results[i] = svc.SyncRepo(context.Background(), e.repo)
		}()
	}
	wg.Wait()
	okCount := 0
	for _, r := range results {
		switch {
		case r.OK():
			okCount++
		case !r.Busy:
			t.Fatalf("only success or busy are acceptable: %+v", r)
		}
	}
	if okCount == 0 {
		t.Fatal("at least one sync must succeed")
	}
	e.fsck()
	if e.mem.Puts() != 1 {
		t.Fatalf("puts = %d, serialized syncs of an unchanged repo publish once", e.mem.Puts())
	}
}

func TestEmptyRepository(t *testing.T) {
	src := testutil.NewRepo(t)
	repo := config.Repository{ID: "github/test/empty", URL: src.URL(), Credential: "personal", Every: time.Hour}
	cfg := newCfg(t, filepath.Join(t.TempDir(), "data"), repo)
	mem := memory.New()
	svc, _, logs, _ := newService(t, cfg, mem, "file")

	r := svc.SyncRepo(context.Background(), repo)
	requireOK(t, r)
	if r.GitStatus != StatusEmpty || r.S3Status != StatusEmpty || mem.Puts() != 0 {
		t.Fatalf("empty repo: %+v puts=%d", r, mem.Puts())
	}
	if !isDir(svc.Layout().MirrorDir(repo.ID)) {
		t.Fatal("local mirror should exist for an empty repo")
	}
	st, _ := state.Load(svc.Layout().StateFile(repo.ID), repo.ID)
	if st.S3.Status != state.StatusEmpty || !strings.Contains(st.S3.Message, "first commit") || len(mem.Keys()) != 0 {
		t.Fatalf("state = %+v", st.S3)
	}
	if !strings.Contains(logs.String(), "git_status=empty") {
		t.Fatalf("log summary should report empty status:\n%s", logs.String())
	}

	src.Commit("a", "1")
	r = svc.SyncRepo(context.Background(), repo)
	requireOK(t, r)
	if r.GitStatus != StatusOK || r.S3Status != StatusOK || mem.Puts() != 1 {
		t.Fatalf("after first commit: %+v", r)
	}
}

func TestDefaultBranchRenameRepublishes(t *testing.T) {
	e := newEnv(t)
	requireOK(t, e.sync())
	e.src.Git("branch", "-m", "main", "trunk")
	r := e.sync()
	requireOK(t, r)
	if st := e.state(); st.Git.HeadRef != "refs/heads/trunk" {
		t.Fatalf("HEAD = %s", st.Git.HeadRef)
	}
	if r.S3Status != StatusOK || e.mem.Puts() != 2 {
		t.Fatalf("default-branch change must republish: %+v", r)
	}
	obj, _ := e.mem.Head(context.Background(), e.key())
	if obj.Metadata[storage.MetaHeadRef] != "refs/heads/trunk" {
		t.Fatalf("head-ref metadata = %q", obj.Metadata[storage.MetaHeadRef])
	}
}

func TestMissingTokenIsAClearGitError(t *testing.T) {
	e := newEnv(t)
	e.svc.LookupEnv = func(string) (string, bool) { return "", false }
	r := e.sync()
	if r.OK() || r.GitErr == nil || !strings.Contains(r.GitErr.Error(), "GITKEEPR_TEST_PAT") {
		t.Fatalf("expected error naming the variable: %+v", r)
	}
	if e.state().Git.Status != state.StatusError {
		t.Fatal("state should record the failure")
	}
}

func TestCredentialHygieneEndToEnd(t *testing.T) {
	src := testutil.Populated(t)
	root := t.TempDir()
	testutil.BareCopy(t, src, root, "repo")
	srv := testutil.HTTPBackend(t, root, testToken)
	repo := config.Repository{ID: "github/test/secure", URL: srv.URL + "/repo.git", Credential: "personal", Every: time.Hour}
	cfg := newCfg(t, filepath.Join(t.TempDir(), "data"), repo)
	mem := memory.New()
	svc, argv, logs, _ := newService(t, cfg, mem, "http")

	requireOK(t, svc.SyncRepo(context.Background(), repo))
	src.Commit("n", "n")
	testutil.Run(t, "", "-C", filepath.Join(root, "repo.git"), "fetch", "--quiet", src.Dir, "+refs/*:refs/*")
	requireOK(t, svc.SyncRepo(context.Background(), repo))

	// wrong token path: failure text must not leak either
	svc.LookupEnv = func(string) (string, bool) { return "dummy-WRONG-token-777", true }
	r := svc.SyncRepo(context.Background(), repo)
	if r.OK() {
		t.Fatal("wrong token should fail")
	}

	haystacks := map[string]string{"argv": argv.String(), "logs": logs.String(), "key": cfg.S3Key(repo.ID)}
	for _, k := range mem.Keys() {
		b, _ := mem.Bytes(k)
		haystacks["s3 object "+k] = string(b)
		o, _ := mem.Head(context.Background(), k)
		for mk, mv := range o.Metadata {
			haystacks["s3 meta "+mk] = mv
		}
	}
	filepath.WalkDir(cfg.Storage.Local.Root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			haystacks["file "+p] = string(b)
		}
		return nil
	})
	if len(haystacks) < 20 {
		t.Fatalf("scan looks too small: %d", len(haystacks))
	}
	for name, h := range haystacks {
		for _, secret := range []string{testToken, "dummy-WRONG-token-777"} {
			if strings.Contains(h, secret) {
				t.Errorf("secret %q found in %s", secret, name)
			}
		}
	}
	if !strings.Contains(logs.String(), "repo_id=github/test/secure") {
		t.Fatal("logs should identify repositories by id")
	}
}

func TestOfflineRestoreFromLocalAndS3(t *testing.T) {
	e := newEnv(t)
	requireOK(t, e.sync())
	wantRefs := testutil.Run(t, e.svc.Layout().MirrorDir(e.repo.ID), "for-each-ref", "--format=%(refname) %(objectname)")

	// the source (and any access to it) is gone
	os.RemoveAll(e.src.Dir)
	e.svc.LookupEnv = func(string) (string, bool) { return "", false } // restore needs no PAT

	for _, from := range []string{FromLocal, FromS3} {
		out := filepath.Join(t.TempDir(), "restored.git")
		res, err := e.svc.Restore(context.Background(), e.repo, from, out)
		if err != nil {
			t.Fatalf("%s: %v", from, err)
		}
		if res.Refs != 4 || res.Head != "refs/heads/main" {
			t.Fatalf("%s: %+v", from, res)
		}
		if got := testutil.Run(t, out, "for-each-ref", "--format=%(refname) %(objectname)"); got != wantRefs {
			t.Fatalf("%s: refs differ\n got: %s\nwant: %s", from, got, wantRefs)
		}
		testutil.Run(t, out, "fsck", "--no-progress")
		if n := testutil.Run(t, out, "rev-list", "--all", "--count"); n != "5" {
			t.Fatalf("%s: commits = %s", from, n)
		}
		if _, err := e.svc.Restore(context.Background(), e.repo, from, out); !errors.Is(err, gitmirror.ErrExists) {
			t.Fatalf("%s: must not overwrite: %v", from, err)
		}
	}
}

func TestRestoreFromS3RejectsBadChecksumAndMissingMetadata(t *testing.T) {
	e := newEnv(t)
	requireOK(t, e.sync())
	good, _ := e.mem.Bytes(e.key())
	meta := func() map[string]string { o, _ := e.mem.Head(context.Background(), e.key()); return o.Metadata }()

	// bit-rot: same length, different content
	bad := append([]byte(nil), good...)
	bad[len(bad)/2] ^= 0xff
	e.mem.Corrupt(e.key(), bad)
	out := filepath.Join(t.TempDir(), "r.git")
	if _, err := e.svc.Restore(context.Background(), e.repo, FromS3, out); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum failure, got %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("failed restore must not create output")
	}

	// no checksum metadata -> refuse
	e.mem.Corrupt(e.key(), good)
	e.mem.SetMetadata(e.key(), map[string]string{})
	if _, err := e.svc.Restore(context.Background(), e.repo, FromS3, out); err == nil {
		t.Fatal("unverifiable bundle must be refused")
	}

	// missing object
	e.mem.Delete(e.key())
	if _, err := e.svc.Restore(context.Background(), e.repo, FromS3, out); err == nil {
		t.Fatal("missing bundle must fail")
	}
	_ = meta
	if _, err := e.svc.Restore(context.Background(), e.repo, "tape", out); err == nil {
		t.Fatal("unknown source")
	}
}

// blockingGit serves HTTP requests that never complete, simulating a hung network.
func blockingGit(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCancelDuringFirstCloneLeavesNoPartialMirror(t *testing.T) {
	srv := blockingGit(t)
	repo := config.Repository{ID: "github/test/hung", URL: srv.URL + "/repo.git", Credential: "personal", Every: time.Hour}
	cfg := newCfg(t, filepath.Join(t.TempDir(), "data"), repo)
	svc, _, _, _ := newService(t, cfg, memory.New(), "http")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- svc.SyncRepo(ctx, repo) }()
	time.Sleep(400 * time.Millisecond)
	cancel()
	select {
	case r := <-done:
		if r.OK() {
			t.Fatal("cancelled sync must not report success")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("sync did not stop after cancellation")
	}
	var leftovers []string
	filepath.WalkDir(filepath.Join(cfg.Storage.Local.Root, "mirrors"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() || (err == nil && strings.Contains(d.Name(), ".git")) {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Fatalf("partial mirror left behind: %v", leftovers)
	}
}

func TestCancelDuringS3UploadPublishesNothingAndKeepsMirror(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{})
	e.mem.BeforePut = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- e.svc.SyncRepo(ctx, e.repo) }()
	<-started
	cancel()
	r := <-done
	if r.OK() || r.S3Status != StatusError {
		t.Fatalf("expected S3 error: %+v", r)
	}
	if len(e.mem.Keys()) != 0 {
		t.Fatal("no object may be visible after an interrupted upload")
	}
	e.fsck()
	if st := e.state(); st.S3.PublishedFingerprint != "" || st.Git.Status != state.StatusOK {
		t.Fatalf("state after cancel: %+v", st)
	}
	if ents, _ := os.ReadDir(e.svc.Layout().StagingDir()); len(ents) != 0 {
		t.Fatalf("staging not cleaned: %v", ents)
	}
	// next run publishes
	e.mem.BeforePut = nil
	requireOK(t, e.sync())
	if e.mem.Puts() != 1 {
		t.Fatal("recovery publish expected")
	}
}

func TestS3DisabledNeedsNoStore(t *testing.T) {
	src := testutil.Populated(t)
	repo := config.Repository{ID: "github/test/local", URL: src.URL(), Credential: "personal", Every: time.Hour}
	cfg := newCfg(t, filepath.Join(t.TempDir(), "data"), repo)
	cfg.Storage.S3.Enabled = false
	svc, _, _, _ := newService(t, cfg, nil, "file")
	r := svc.SyncRepo(context.Background(), repo)
	requireOK(t, r)
	if r.S3Status != StatusDisabled || r.GitStatus != StatusOK {
		t.Fatalf("%+v", r)
	}
	out := filepath.Join(t.TempDir(), "o.git")
	if _, err := svc.Restore(context.Background(), repo, FromLocal, out); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore(context.Background(), repo, FromS3, filepath.Join(t.TempDir(), "x.git")); err == nil {
		t.Fatal("s3 restore without s3 must fail")
	}
}

func TestServeSyncsOnStartRepeatsAndStopsOnCancel(t *testing.T) {
	e := newEnv(t)
	e.cfg.Repositories[0].Every = 60 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.svc.Serve(ctx); close(done) }()

	waitFor(t, func() bool { return e.mem.Puts() == 1 })
	e.src.Commit("later.txt", "later") // picked up by a later scheduled run, no external trigger
	waitFor(t, func() bool {
		e.mem.Head(ctx, e.key())
		return e.state().S3.Status == state.StatusOK && e.mem.Puts() == 2
	})
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not exit after cancellation")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}
