package gitmirror

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirkode/git-keepr/internal/credentials"
	"github.com/amirkode/git-keepr/internal/testutil"
)

func TestMain(m *testing.M) {
	credentials.MaybeRunAskpass() // the test binary doubles as the askpass helper
	os.Exit(m.Run())
}

func newGit(t *testing.T) (*Git, *[][]string) {
	t.Helper()
	var traced [][]string
	g := New(t.TempDir())
	g.AllowProtocols = "file"
	g.Trace = func(a []string) { traced = append(traced, a) }
	return g, &traced
}

func refs(t *testing.T, dir string) string {
	t.Helper()
	return testutil.Run(t, dir, "for-each-ref", "--format=%(refname) %(objectname)")
}

func TestInitialMirrorPreservesFullHistory(t *testing.T) {
	ctx := context.Background()
	src := testutil.Populated(t)
	g, traced := newGit(t)
	dest := filepath.Join(t.TempDir(), "m.git")

	if err := g.CloneMirror(ctx, src.URL(), dest, nil); err != nil {
		t.Fatal(err)
	}
	// every branch and tag, same object ids
	if got, want := refs(t, dest), refs(t, src.Dir); !strings.Contains(got, "refs/heads/feature") || !strings.Contains(got, "refs/tags/v1.0.0") || !strings.Contains(got, "refs/tags/light") {
		t.Fatalf("missing refs in mirror:\n%s", got)
	} else {
		for _, line := range strings.Split(want, "\n") {
			if strings.HasPrefix(line, "refs/heads/") || strings.HasPrefix(line, "refs/tags/") {
				if !strings.Contains(got, line) {
					t.Fatalf("mirror lacks %q", line)
				}
			}
		}
	}
	// full history: all 5 commits, not shallow
	if n := testutil.Run(t, dest, "rev-list", "--all", "--count"); n != "5" {
		t.Fatalf("expected 5 commits, got %s", n)
	}
	if v := testutil.Run(t, dest, "rev-parse", "--is-shallow-repository"); v != "false" {
		t.Fatal("mirror must not be shallow")
	}
	for _, a := range *traced {
		for _, arg := range a {
			if strings.HasPrefix(arg, "--depth") || arg == "--shallow-since" {
				t.Fatalf("shallow flag used: %v", a)
			}
		}
	}
	if u := testutil.Run(t, dest, "config", "remote.origin.url"); u != src.URL() {
		t.Fatalf("origin url = %s", u)
	}
}

func TestIncrementalFetchDoesNotReclone(t *testing.T) {
	ctx := context.Background()
	src := testutil.Populated(t)
	g, traced := newGit(t)
	dest := filepath.Join(t.TempDir(), "m.git")
	if err := g.CloneMirror(ctx, src.URL(), dest, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := g.Snapshot(ctx, dest)
	marker := filepath.Join(dest, "marker")
	os.WriteFile(marker, []byte("x"), 0o600)

	newCommit := src.Commit("c.txt", "later")
	src.Git("tag", "v2.0.0")
	*traced = nil
	if err := g.Fetch(ctx, dest, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("mirror was recreated instead of fetched")
	}
	for _, a := range *traced {
		if len(a) > 5 && a[5] == "clone" {
			t.Fatalf("unexpected clone: %v", a)
		}
	}
	after, _ := g.Snapshot(ctx, dest)
	if after.Fingerprint == before.Fingerprint || after.RefCount != before.RefCount+1 {
		t.Fatalf("snapshot did not change as expected: %+v -> %+v", before, after)
	}
	if got := testutil.Run(t, dest, "rev-parse", "refs/heads/main"); got != newCommit {
		t.Fatalf("main not updated: %s != %s", got, newCommit)
	}
	again, _ := g.Snapshot(ctx, dest)
	if again.Fingerprint != after.Fingerprint {
		t.Fatal("fingerprint must be deterministic")
	}
}

func TestForcePushAndDeletedBranchSemantics(t *testing.T) {
	ctx := context.Background()
	src := testutil.NewRepo(t)
	src.Commit("a", "1")
	src.Git("checkout", "--quiet", "-b", "topic")
	src.Commit("t", "topic work")
	src.Git("checkout", "--quiet", "main")
	oldMain := src.Commit("a", "2-to-be-rewritten")
	g, _ := newGit(t)
	dest := filepath.Join(t.TempDir(), "m.git")
	if err := g.CloneMirror(ctx, src.URL(), dest, nil); err != nil {
		t.Fatal(err)
	}

	// upstream deletes a branch and force-rewrites main
	src.Git("branch", "-D", "topic")
	src.Git("reset", "--quiet", "--hard", "HEAD~1")
	rewritten := src.Commit("a", "rewritten")
	if err := g.Fetch(ctx, dest, nil); err != nil {
		t.Fatal(err)
	}

	// --no-prune: the deleted branch is still preserved locally (not a deletion-synced mirror)
	if !g.HasRef(ctx, dest, "refs/heads/topic") {
		t.Fatal("deleted upstream branch must remain in the mirror (--no-prune)")
	}
	// main follows the force-push
	if got := testutil.Run(t, dest, "rev-parse", "refs/heads/main"); got != rewritten {
		t.Fatal("main should follow the rewritten history")
	}
	// the rewritten-away commit is no longer reachable from any ref ...
	if all := testutil.Run(t, dest, "rev-list", "--all"); strings.Contains(all, oldMain) {
		t.Fatal("old commit should be unreachable after the force-push")
	}
	// ... so it is NOT part of a bundle/restore: reachable history is the guarantee, nothing more.
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	if err := g.CreateBundle(ctx, dest, bundle); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "restored.git")
	if _, err := g.RestoreFromBundle(ctx, bundle, out, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := runErr(out, "cat-file", "-e", oldMain+"^{commit}"); err == nil {
		t.Fatal("force-pushed-away commit must not be guaranteed in the restored backup")
	}
	if !g.HasRef(ctx, out, "refs/heads/topic") {
		t.Fatal("preserved deleted branch should survive into the bundle")
	}
}

func runErr(dir string, args ...string) error {
	g := New(os.TempDir())
	_, err := g.run(context.Background(), dir, nil, args...)
	return err
}

func TestDefaultBranchRename(t *testing.T) {
	ctx := context.Background()
	src := testutil.Populated(t)
	g, _ := newGit(t)
	dest := filepath.Join(t.TempDir(), "m.git")
	if err := g.CloneMirror(ctx, src.URL(), dest, nil); err != nil {
		t.Fatal(err)
	}
	if h, err := g.SourceHead(ctx, dest, nil); err != nil || h != "refs/heads/main" {
		t.Fatalf("source head = %q, %v", h, err)
	}
	src.Git("branch", "-m", "main", "trunk")
	if err := g.Fetch(ctx, dest, nil); err != nil {
		t.Fatal(err)
	}
	h, err := g.SourceHead(ctx, dest, nil)
	if err != nil || h != "refs/heads/trunk" {
		t.Fatalf("source head after rename = %q, %v", h, err)
	}
	if err := g.SetHead(ctx, dest, h); err != nil {
		t.Fatal(err)
	}
	snap, _ := g.Snapshot(ctx, dest)
	if snap.Head != "refs/heads/trunk" {
		t.Fatalf("mirror HEAD = %s", snap.Head)
	}
}

func TestEmptyRepository(t *testing.T) {
	ctx := context.Background()
	src := testutil.NewRepo(t)
	g, _ := newGit(t)
	dest := filepath.Join(t.TempDir(), "m.git")
	if err := g.CloneMirror(ctx, src.URL(), dest, nil); err != nil {
		t.Fatalf("empty repo must clone: %v", err)
	}
	snap, err := g.Snapshot(ctx, dest)
	if err != nil || !snap.Empty() {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if h, err := g.SourceHead(ctx, dest, nil); err != nil || h != "" {
		t.Fatalf("empty source head = %q, %v", h, err)
	}
	if err := g.CreateBundle(ctx, dest, filepath.Join(t.TempDir(), "x.bundle")); err == nil {
		t.Fatal("git cannot bundle an empty repo; callers must check Snapshot.Empty()")
	}
	// after the first commit the same mirror becomes publishable
	src.Commit("a", "1")
	if err := g.Fetch(ctx, dest, nil); err != nil {
		t.Fatal(err)
	}
	if snap, _ := g.Snapshot(ctx, dest); snap.Empty() {
		t.Fatal("mirror should have refs after first commit")
	}
}

func TestBundleRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := testutil.Populated(t)
	g, _ := newGit(t)
	dest := filepath.Join(t.TempDir(), "m.git")
	if err := g.CloneMirror(ctx, src.URL(), dest, nil); err != nil {
		t.Fatal(err)
	}
	snap, _ := g.Snapshot(ctx, dest)
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	if err := g.CreateBundle(ctx, dest, bundle); err != nil {
		t.Fatal(err)
	}
	if err := g.VerifyBundle(ctx, dest, bundle); err != nil {
		t.Fatal(err)
	}
	sum, size, err := HashFile(bundle)
	if err != nil || len(sum) != 64 || size == 0 {
		t.Fatalf("hash: %s %d %v", sum, size, err)
	}

	// restore once with the recorded HEAD hint, once relying on inference
	for name, head := range map[string]string{"hint": snap.Head, "inferred": ""} {
		out := filepath.Join(t.TempDir(), "restored.git")
		rs, err := g.RestoreFromBundle(ctx, bundle, out, head, snap.RefsDigest)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rs.Fingerprint != snap.Fingerprint {
			t.Fatalf("%s: restored fingerprint differs (HEAD %q vs %q)", name, rs.Head, snap.Head)
		}
		if n := testutil.Run(t, out, "rev-list", "--all", "--count"); n != "5" {
			t.Fatalf("%s: commits = %s", name, n)
		}
		if testutil.Run(t, out, "remote") != "" {
			t.Fatalf("%s: restored repo should have no remotes", name)
		}
		// a working clone from the restored repo has the files
		wc := filepath.Join(t.TempDir(), "wc")
		testutil.Run(t, "", "clone", "--quiet", out, wc)
		if b, _ := os.ReadFile(filepath.Join(wc, "b.txt")); string(b) != "three" {
			t.Fatalf("%s: checkout content wrong", name)
		}
		// restoring again onto an existing path is refused
		if _, err := g.RestoreFromBundle(ctx, bundle, out, "", ""); !errors.Is(err, ErrExists) {
			t.Fatalf("%s: expected ErrExists, got %v", name, err)
		}
	}
}

func TestRestoreRejectsCorruptBundleAndWrongDigest(t *testing.T) {
	ctx := context.Background()
	src := testutil.Populated(t)
	g, _ := newGit(t)
	dest := filepath.Join(t.TempDir(), "m.git")
	g.CloneMirror(ctx, src.URL(), dest, nil)
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	g.CreateBundle(ctx, dest, bundle)
	data, _ := os.ReadFile(bundle)

	outDir := t.TempDir()
	bad := filepath.Join(t.TempDir(), "bad.bundle")
	os.WriteFile(bad, data[:len(data)/2], 0o600)
	out := filepath.Join(outDir, "r.git")
	if _, err := g.RestoreFromBundle(ctx, bad, out, "", ""); err == nil {
		t.Fatal("truncated bundle must be rejected")
	}
	if _, err := g.RestoreFromBundle(ctx, bundle, out, "", strings.Repeat("a", 64)); err == nil {
		t.Fatal("digest mismatch must be rejected")
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 0 {
		t.Fatalf("failed restore left files behind: %v", entries)
	}
}

func TestRestoreFromMirror(t *testing.T) {
	ctx := context.Background()
	src := testutil.Populated(t)
	g, _ := newGit(t)
	dest := filepath.Join(t.TempDir(), "m.git")
	g.CloneMirror(ctx, src.URL(), dest, nil)
	snap, _ := g.Snapshot(ctx, dest)
	out := filepath.Join(t.TempDir(), "r.git")
	rs, err := g.RestoreFromMirror(ctx, dest, out, snap.RefsDigest)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Fingerprint != snap.Fingerprint {
		t.Fatal("fingerprint mismatch after copy")
	}
	if testutil.Run(t, out, "remote") != "" {
		t.Fatal("restored mirror must not keep the origin remote")
	}
	if _, err := g.RestoreFromMirror(ctx, dest, out, ""); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}
	if _, err := g.RestoreFromMirror(ctx, filepath.Join(t.TempDir(), "nope.git"), filepath.Join(t.TempDir(), "o.git"), ""); err == nil {
		t.Fatal("missing mirror must fail")
	}
}

func TestAuthenticatedCloneAndFetchOverHTTP(t *testing.T) {
	ctx := context.Background()
	const token = "dummy-pat-0123456789"
	src := testutil.Populated(t)
	root := t.TempDir()
	testutil.BareCopy(t, src, root, "repo")
	srv := testutil.HTTPBackend(t, root, token)

	g := New(t.TempDir())
	g.AllowProtocols = "http"
	var argvs []string
	g.Trace = func(a []string) { argvs = append(argvs, strings.Join(a, " ")) }

	dest := filepath.Join(t.TempDir(), "m.git")
	url := srv.URL + "/repo.git"
	if err := g.CloneMirror(ctx, url, dest, &credentials.Auth{Token: token}); err != nil {
		t.Fatalf("authenticated clone failed: %v", err)
	}
	src.Commit("n.txt", "new")
	testutil.Run(t, "", "-C", filepath.Join(root, "repo.git"), "fetch", "--quiet", src.Dir, "+refs/*:refs/*")
	if err := g.Fetch(ctx, dest, &credentials.Auth{Token: token}); err != nil {
		t.Fatalf("authenticated fetch failed: %v", err)
	}

	// credential hygiene
	for _, a := range argvs {
		if strings.Contains(a, token) {
			t.Fatalf("token present in argument vector: %s", a)
		}
	}
	filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, _ := d.Info(); info.Size() < 1<<20 {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), token) {
					t.Errorf("token found on disk in %s", p)
				}
			}
		}
		return nil
	})

	// wrong token: fails, never prompts, and the error text does not contain either secret
	bad := "dummy-wrong-token-9999"
	err := g.Fetch(ctx, dest, &credentials.Auth{Token: bad})
	if err == nil {
		t.Fatal("wrong token must fail")
	}
	if strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), token) {
		t.Fatalf("secret in error: %v", err)
	}
	// no credentials at all: must fail promptly instead of prompting
	if err := g.Fetch(ctx, dest, nil); err == nil {
		t.Fatal("unauthenticated fetch must fail")
	}
}

func TestChildEnvironmentIsScrubbed(t *testing.T) {
	t.Setenv("GITHUB_PAT_OTHER", "dummy-other-owner-token")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "dummy-aws-secret")
	g := New(t.TempDir())
	env := strings.Join(g.environ(&credentials.Auth{Token: "dummy-selected"}), "\n")
	for _, leak := range []string{"dummy-other-owner-token", "dummy-aws-secret", "AWS_"} {
		if strings.Contains(env, leak) {
			t.Fatalf("child env leaks %q", leak)
		}
	}
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GIT_ALLOW_PROTOCOL=https", "GIT_CONFIG_NOSYSTEM=1"} {
		if !strings.Contains(env, want) {
			t.Fatalf("child env lacks %q", want)
		}
	}
}

func TestSnapshotCountsAndHeadInFingerprint(t *testing.T) {
	ctx := context.Background()
	src := testutil.Populated(t)
	g, _ := newGit(t)
	dest := filepath.Join(t.TempDir(), "m.git")
	g.CloneMirror(ctx, src.URL(), dest, nil)
	s1, _ := g.Snapshot(ctx, dest)
	if s1.RefCount != 4 { // main, feature, v1.0.0, light
		t.Fatalf("ref count = %d", s1.RefCount)
	}
	if err := g.SetHead(ctx, dest, "refs/heads/feature"); err != nil {
		t.Fatal(err)
	}
	s2, _ := g.Snapshot(ctx, dest)
	if s2.RefsDigest != s1.RefsDigest || s2.Fingerprint == s1.Fingerprint {
		t.Fatal("HEAD change must alter Fingerprint but not RefsDigest")
	}
}

// Regression: production runners only allow https; restoring from a local
// bundle must still work offline.
func TestRestoreWorksWithProductionProtocolRestrictions(t *testing.T) {
	ctx := context.Background()
	src := testutil.Populated(t)
	setup, _ := newGit(t)
	dest := filepath.Join(t.TempDir(), "m.git")
	if err := setup.CloneMirror(ctx, src.URL(), dest, nil); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	if err := setup.CreateBundle(ctx, dest, bundle); err != nil {
		t.Fatal(err)
	}
	prod := New(t.TempDir())
	if prod.AllowProtocols != "https" {
		t.Fatalf("production default must be https, got %q", prod.AllowProtocols)
	}
	if _, err := prod.RestoreFromBundle(ctx, bundle, filepath.Join(t.TempDir(), "r.git"), "", ""); err != nil {
		t.Fatalf("restore under production restrictions: %v", err)
	}
	// ...while network access to non-https sources stays forbidden
	if err := prod.CloneMirror(ctx, src.URL(), filepath.Join(t.TempDir(), "x.git"), nil); err == nil {
		t.Fatal("file:// clone must be rejected by the production protocol allow-list")
	}
}
