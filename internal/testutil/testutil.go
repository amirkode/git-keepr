// Package testutil provides local Git fixtures for tests: no network and no
// real GitHub account are ever needed.
package testutil

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is a non-bare working repository used as the "remote" source.
type Repo struct {
	t   testing.TB
	Dir string
}

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
}

// Run executes git in dir and fails the test on error.
func Run(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// NewRepo creates an empty repository with main as default branch.
func NewRepo(t testing.TB) *Repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	Run(t, dir, "init", "--quiet", "-b", "main")
	return &Repo{t: t, Dir: dir}
}

// Git runs a git command inside the repo.
func (r *Repo) Git(args ...string) string { r.t.Helper(); return Run(r.t, r.Dir, args...) }

// Commit writes file=content, commits and returns the new commit id.
func (r *Repo) Commit(file, content string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.Dir, file), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.Git("add", file)
	r.Git("commit", "--quiet", "-m", "update "+file+" "+content)
	return r.Git("rev-parse", "HEAD")
}

// URL is a file:// URL (forces the real transport instead of local hardlinks).
func (r *Repo) URL() string { return "file://" + r.Dir }

// Populated builds a repo with several commits, two branches and two tags.
func Populated(t testing.TB) *Repo {
	t.Helper()
	r := NewRepo(t)
	r.Commit("a.txt", "one")
	r.Commit("a.txt", "two")
	r.Commit("b.txt", "three")
	r.Git("tag", "-a", "v1.0.0", "-m", "release one")
	r.Git("tag", "light")
	r.Git("checkout", "--quiet", "-b", "feature")
	r.Commit("f.txt", "feature one")
	r.Commit("f.txt", "feature two")
	r.Git("checkout", "--quiet", "main")
	return r
}

// BareCopy clones src into <root>/<name>.git and returns its path.
func BareCopy(t testing.TB, src *Repo, root, name string) string {
	t.Helper()
	dst := filepath.Join(root, name+".git")
	Run(t, "", "clone", "--quiet", "--bare", src.Dir, dst)
	return dst
}

// HTTPBackend serves bare repositories under root over smart HTTP, requiring
// HTTP basic auth with the given token as password. It skips the test when
// git-http-backend is unavailable.
func HTTPBackend(t testing.TB, root, token string) *httptest.Server {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git not available")
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not available")
	}
	h := &cgi.Handler{
		Path:       backend,
		Root:       "",
		Env:        []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"},
		InheritEnv: []string{"PATH"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || pass != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}
