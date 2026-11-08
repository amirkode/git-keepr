// Package gitmirror wraps the installed git executable: authenticated mirror
// clone/fetch, deterministic ref fingerprints, bundle creation/verification
// and offline restore. Git is always invoked with exec.CommandContext and an
// explicit argument vector, never through a shell.
package gitmirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/amirkode/git-keepr/internal/credentials"
)

// Git runs git commands with a scrubbed environment.
type Git struct {
	// Bin is the git executable (default "git").
	Bin string
	// Askpass is the executable git calls for credentials (default: this binary).
	Askpass string
	// AllowProtocols is passed as GIT_ALLOW_PROTOCOL (default "https").
	AllowProtocols string
	// Home is used as $HOME for git; user/system git configuration is ignored.
	Home string
	// Trace, when set, receives the full argument vector of every command (tests).
	Trace func(args []string)
}

// New returns a Git runner. home should be a private writable directory.
func New(home string) *Git {
	exe, _ := os.Executable()
	return &Git{Bin: "git", Askpass: exe, AllowProtocols: "https", Home: home}
}

var passthroughEnv = []string{
	"SSL_CERT_FILE", "SSL_CERT_DIR", "GIT_SSL_CAINFO",
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
}

// environ builds the child environment from scratch so unrelated secrets
// (other PATs, AWS keys) are never inherited by git.
func (g *Git) environ(auth *credentials.Auth) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + g.Home,
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ALLOW_PROTOCOL=" + g.AllowProtocols,
		"GIT_AUTHOR_NAME=git-keepr", "GIT_AUTHOR_EMAIL=git-keepr@localhost",
		"GIT_COMMITTER_NAME=git-keepr", "GIT_COMMITTER_EMAIL=git-keepr@localhost",
	}
	for _, k := range passthroughEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, auth.Env(g.Askpass)...)
}

// run executes `git <args...>`, returning stdout. Errors carry redacted stderr.
func (g *Git) run(ctx context.Context, dir string, auth *credentials.Auth, args ...string) (string, error) {
	// -c credential.helper= disables every persistent credential helper.
	full := append([]string{"-c", "credential.helper=", "-c", "gc.pruneExpire=never"}, args...)
	if g.Trace != nil {
		g.Trace(append([]string{g.Bin}, full...))
	}
	cmd := exec.CommandContext(ctx, g.Bin, full...)
	cmd.Dir = dir
	cmd.Env = g.environ(auth)
	configureProcess(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			reapGroup(cmd)
			return "", fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		var secrets []string
		if auth != nil {
			secrets = append(secrets, auth.Token)
		}
		msg := strings.TrimSpace(credentials.Redact(tail(stderr.String(), 2048), secrets...))
		return "", fmt.Errorf("git %s failed: %v: %s", args[0], err, msg)
	}
	return stdout.String(), nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// CloneMirror runs `git clone --mirror` (full history, never shallow) into dest.
func (g *Git) CloneMirror(ctx context.Context, url, dest string, auth *credentials.Auth) error {
	if _, err := g.run(ctx, "", auth, "clone", "--mirror", "--quiet", "--", url, dest); err != nil {
		return err
	}
	_, err := g.run(ctx, dest, nil, "config", "gc.pruneExpire", "never")
	return err
}

// Fetch updates an existing mirror without pruning, preserving fetched history.
func (g *Git) Fetch(ctx context.Context, dir string, auth *credentials.Auth) error {
	_, err := g.run(ctx, dir, auth, "fetch", "--no-prune", "--quiet", "origin")
	return err
}

// SourceHead asks the origin which branch its HEAD points at ("" when unknown, e.g. empty repo).
func (g *Git) SourceHead(ctx context.Context, dir string, auth *credentials.Auth) (string, error) {
	out, err := g.run(ctx, dir, auth, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: "); ok {
			ref, _, _ := strings.Cut(rest, "\t")
			return strings.TrimSpace(ref), nil
		}
	}
	return "", nil
}

// HasRef reports whether ref exists in the repository.
func (g *Git) HasRef(ctx context.Context, dir, ref string) bool {
	_, err := g.run(ctx, dir, nil, "rev-parse", "--verify", "--quiet", ref+"^{object}")
	return err == nil
}

// SetHead points the symbolic HEAD at ref.
func (g *Git) SetHead(ctx context.Context, dir, ref string) error {
	_, err := g.run(ctx, dir, nil, "symbolic-ref", "HEAD", ref)
	return err
}

// Snapshot is a deterministic description of the refs stored in a repository.
type Snapshot struct {
	RefCount int
	// Head is the branch HEAD points to ("" if unknown).
	Head string
	// Fingerprint covers sorted "refname objectid" lines plus HEAD's target.
	Fingerprint string
	// RefsDigest covers refs only (independent of HEAD).
	RefsDigest string
}

// Empty reports whether the repository has no refs, i.e. no reachable commits.
func (s Snapshot) Empty() bool { return s.RefCount == 0 }

// Snapshot computes the ref fingerprint from the repository itself, never from file times.
func (g *Git) Snapshot(ctx context.Context, dir string) (Snapshot, error) {
	out, err := g.run(ctx, dir, nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return Snapshot{}, err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	sort.Strings(lines)
	refs := sha256.New()
	for _, l := range lines {
		io.WriteString(refs, l+"\n")
	}
	s := Snapshot{RefCount: len(lines), RefsDigest: hex.EncodeToString(refs.Sum(nil))}
	if head, err := g.run(ctx, dir, nil, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		s.Head = strings.TrimSpace(head)
	}
	full := sha256.New()
	for _, l := range lines {
		io.WriteString(full, l+"\n")
	}
	io.WriteString(full, "HEAD -> "+s.Head+"\n")
	s.Fingerprint = hex.EncodeToString(full.Sum(nil))
	return s, nil
}

// CreateBundle writes a complete, self-contained bundle of every ref to out.
func (g *Git) CreateBundle(ctx context.Context, dir, out string) error {
	_, err := g.run(ctx, dir, nil, "bundle", "create", "--quiet", out, "--all")
	return err
}

// VerifyBundle runs `git bundle verify` against dir (any repository).
func (g *Git) VerifyBundle(ctx context.Context, dir, bundle string) error {
	_, err := g.run(ctx, dir, nil, "bundle", "verify", bundle)
	return err
}

// Fsck runs a full object-integrity check.
func (g *Git) Fsck(ctx context.Context, dir string) error {
	_, err := g.run(ctx, dir, nil, "fsck", "--no-progress")
	return err
}

// HashFile returns the SHA-256 and size of a file.
func HashFile(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ErrExists is returned by restore when the output path is already present.
var ErrExists = errors.New("output path already exists")

// RestoreFromBundle builds a new bare repository at out from a bundle file.
// headRef (optional) is the branch HEAD should point at; if empty it is
// inferred from the bundle. When wantRefsDigest is non-empty the restored refs
// must match it. Nothing appears at out unless every check passed.
func (g *Git) RestoreFromBundle(ctx context.Context, bundle, out, headRef, wantRefsDigest string) (Snapshot, error) {
	if _, err := os.Lstat(out); err == nil {
		return Snapshot{}, fmt.Errorf("%w: %s", ErrExists, out)
	}
	tmp, err := tempSibling(out)
	if err != nil {
		return Snapshot{}, err
	}
	defer os.RemoveAll(tmp)
	if _, err := g.run(ctx, "", nil, "init", "--bare", "--quiet", tmp); err != nil {
		return Snapshot{}, err
	}
	if err := g.VerifyBundle(ctx, tmp, bundle); err != nil {
		return Snapshot{}, fmt.Errorf("bundle failed verification: %w", err)
	}
	// Fetching from the local bundle file is a "file" transport, which the
	// network-facing default (https only) forbids; no network is involved here.
	local := *g
	local.AllowProtocols = "file"
	if _, err := local.run(ctx, tmp, nil, "fetch", "--quiet", bundle, "+refs/*:refs/*"); err != nil {
		return Snapshot{}, err
	}
	if headRef == "" {
		headRef, err = g.guessBundleHead(ctx, tmp, bundle)
		if err != nil {
			return Snapshot{}, err
		}
	}
	if headRef != "" {
		if err := g.SetHead(ctx, tmp, headRef); err != nil {
			return Snapshot{}, err
		}
	}
	return g.finishRestore(ctx, tmp, out, wantRefsDigest)
}

func (g *Git) guessBundleHead(ctx context.Context, dir, bundle string) (string, error) {
	out, err := g.run(ctx, dir, nil, "bundle", "list-heads", bundle)
	if err != nil {
		return "", err
	}
	var headOID string
	byOID := map[string][]string{}
	for _, l := range strings.Split(out, "\n") {
		oid, ref, ok := strings.Cut(strings.TrimSpace(l), " ")
		if !ok {
			continue
		}
		if ref == "HEAD" {
			headOID = oid
		} else if strings.HasPrefix(ref, "refs/heads/") {
			byOID[oid] = append(byOID[oid], ref)
		}
	}
	cands := byOID[headOID]
	sort.Strings(cands)
	for _, pref := range []string{"refs/heads/main", "refs/heads/master"} {
		for _, c := range cands {
			if c == pref {
				return c, nil
			}
		}
	}
	if len(cands) > 0 {
		return cands[0], nil
	}
	return "", nil
}

// RestoreFromMirror copies a local bare mirror to out (caller must hold the repo lock).
func (g *Git) RestoreFromMirror(ctx context.Context, src, out, wantRefsDigest string) (Snapshot, error) {
	if _, err := os.Lstat(out); err == nil {
		return Snapshot{}, fmt.Errorf("%w: %s", ErrExists, out)
	}
	if _, err := os.Stat(filepath.Join(src, "HEAD")); err != nil {
		return Snapshot{}, fmt.Errorf("no local mirror at %s: %w", src, err)
	}
	tmp, err := tempSibling(out)
	if err != nil {
		return Snapshot{}, err
	}
	defer os.RemoveAll(tmp)
	if err := copyTree(src, tmp); err != nil {
		return Snapshot{}, err
	}
	return g.finishRestore(ctx, tmp, out, wantRefsDigest)
}

func (g *Git) finishRestore(ctx context.Context, tmp, out, wantRefsDigest string) (Snapshot, error) {
	// A restored repository must not keep pointing at the (possibly lost) origin.
	_, _ = g.run(ctx, tmp, nil, "config", "--remove-section", "remote.origin")
	snap, err := g.Snapshot(ctx, tmp)
	if err != nil {
		return Snapshot{}, err
	}
	if wantRefsDigest != "" && snap.RefsDigest != wantRefsDigest {
		return Snapshot{}, fmt.Errorf("restored refs do not match the recorded backup (got digest %s, want %s)", snap.RefsDigest[:12], wantRefsDigest[:12])
	}
	if err := g.Fsck(ctx, tmp); err != nil {
		return Snapshot{}, fmt.Errorf("restored repository failed fsck: %w", err)
	}
	if _, err := os.Lstat(out); err == nil {
		return Snapshot{}, fmt.Errorf("%w: %s", ErrExists, out)
	}
	if err := os.Rename(tmp, out); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func tempSibling(out string) (string, error) {
	parent := filepath.Dir(out)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, "."+filepath.Base(out)+".restore-")
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		}
		return nil
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
