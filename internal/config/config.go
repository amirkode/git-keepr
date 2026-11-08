// Package config loads and validates the git-keepr YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// CredentialTypeGitHubPAT is the only credential type in v0.1.
	CredentialTypeGitHubPAT = "github_pat"

	DefaultInterval       = 6 * time.Hour
	DefaultMaxConcurrency = 2
	// MinInterval protects the upstream host from accidental hot loops.
	MinInterval = time.Minute
	// MaxConcurrencyLimit is a sanity bound for max_concurrency.
	MaxConcurrencyLimit = 64
)

// Config is the canonical v0.1 configuration schema.
type Config struct {
	Version      int                   `yaml:"version"`
	Scheduler    Scheduler             `yaml:"scheduler"`
	Credentials  map[string]Credential `yaml:"credentials"`
	Repositories []Repository          `yaml:"repositories"`
	Storage      Storage               `yaml:"storage"`
}

type Scheduler struct {
	DefaultInterval string `yaml:"default_interval"`
	MaxConcurrency  int    `yaml:"max_concurrency"`
	SyncOnStart     *bool  `yaml:"sync_on_start"`

	// Parsed values, filled by Load.
	Interval time.Duration `yaml:"-"`
	OnStart  bool          `yaml:"-"`
}

type Credential struct {
	Type     string `yaml:"type"`
	TokenEnv string `yaml:"token_env"`
}

type Repository struct {
	ID         string `yaml:"id"`
	URL        string `yaml:"url"`
	Credential string `yaml:"credential"`
	Interval   string `yaml:"interval"`

	// Every is the effective sync interval (own interval or the default).
	Every time.Duration `yaml:"-"`
}

type Storage struct {
	Local LocalStorage `yaml:"local"`
	S3    S3Storage    `yaml:"s3"`
}

type LocalStorage struct {
	Root string `yaml:"root"`
}

type S3Storage struct {
	Enabled   bool   `yaml:"enabled"`
	Bucket    string `yaml:"bucket"`
	Prefix    string `yaml:"prefix"`
	Region    string `yaml:"region"`
	Endpoint  string `yaml:"endpoint"`
	PathStyle bool   `yaml:"path_style"`
}

// ValidationError aggregates every problem found so operators can fix them in one pass.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid configuration:\n  - " + strings.Join(e.Problems, "\n  - ")
}

var (
	idSegmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	githubURLRe = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9][A-Za-z0-9._-]*)/([A-Za-z0-9._-]+)$`)
	envNameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	bucketRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// Load reads path, applies defaults and performs static validation (no
// environment, filesystem or network access).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse decodes YAML bytes, applies defaults and validates them statically.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &ValidationError{Problems: []string{"configuration file is empty"}}
		}
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) normalize() error {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }

	if c.Version != 1 {
		add("version: must be 1 (got %d)", c.Version)
	}

	// Scheduler.
	c.Scheduler.Interval = DefaultInterval
	if s := strings.TrimSpace(c.Scheduler.DefaultInterval); s != "" {
		d, err := parseInterval(s)
		if err != nil {
			add("scheduler.default_interval: %v", err)
		} else {
			c.Scheduler.Interval = d
		}
	}
	switch {
	case c.Scheduler.MaxConcurrency == 0:
		c.Scheduler.MaxConcurrency = DefaultMaxConcurrency
	case c.Scheduler.MaxConcurrency < 0 || c.Scheduler.MaxConcurrency > MaxConcurrencyLimit:
		add("scheduler.max_concurrency: must be between 1 and %d", MaxConcurrencyLimit)
	}
	c.Scheduler.OnStart = c.Scheduler.SyncOnStart == nil || *c.Scheduler.SyncOnStart

	// Credentials.
	for name, cred := range c.Credentials {
		if !idSegmentRe.MatchString(name) {
			add("credentials.%q: invalid credential name", name)
		}
		if cred.Type != CredentialTypeGitHubPAT {
			add("credentials.%s.type: must be %q", name, CredentialTypeGitHubPAT)
		}
		if !envNameRe.MatchString(cred.TokenEnv) {
			add("credentials.%s.token_env: must be a valid environment variable name", name)
		}
	}

	// Repositories.
	if len(c.Repositories) == 0 {
		add("repositories: at least one repository is required")
	}
	ids := map[string]int{}
	urls := map[string]string{}
	for i := range c.Repositories {
		r := &c.Repositories[i]
		where := fmt.Sprintf("repositories[%d]", i)
		if r.ID != "" {
			where = fmt.Sprintf("repositories[%d] (%s)", i, r.ID)
		}
		if err := ValidateID(r.ID); err != nil {
			add("%s: id: %v", where, err)
		} else {
			key := strings.ToLower(r.ID) // collide case-insensitively: safe on any filesystem
			if j, dup := ids[key]; dup {
				add("%s: id collides with repositories[%d]", where, j)
			}
			ids[key] = i
		}
		norm, err := NormalizeURL(r.URL)
		if err != nil {
			add("%s: url: %v", where, err)
		} else {
			r.URL = norm
			if other, dup := urls[strings.ToLower(norm)]; dup {
				add("%s: url is already registered as %s", where, other)
			}
			urls[strings.ToLower(norm)] = r.ID
		}
		if r.Credential == "" {
			add("%s: credential: required", where)
		} else if _, ok := c.Credentials[r.Credential]; !ok {
			add("%s: credential: unknown credential %q", where, r.Credential)
		}
		r.Every = c.Scheduler.Interval
		if s := strings.TrimSpace(r.Interval); s != "" {
			d, err := parseInterval(s)
			if err != nil {
				add("%s: interval: %v", where, err)
			} else {
				r.Every = d
			}
		}
	}

	// Storage.
	root := c.Storage.Local.Root
	switch {
	case root == "":
		add("storage.local.root: required")
	case !filepath.IsAbs(root):
		add("storage.local.root: must be an absolute path")
	default:
		c.Storage.Local.Root = filepath.Clean(root)
	}
	if s3 := &c.Storage.S3; s3.Enabled {
		if !bucketRe.MatchString(s3.Bucket) {
			add("storage.s3.bucket: %q is not a valid bucket name", s3.Bucket)
		}
		if strings.TrimSpace(s3.Region) == "" {
			add("storage.s3.region: required when s3 is enabled (use \"auto\" for Cloudflare R2)")
		}
		if s3.Endpoint != "" {
			u, err := url.Parse(s3.Endpoint)
			switch {
			case err != nil || u.Host == "":
				add("storage.s3.endpoint: not a valid URL")
			case u.Scheme != "https":
				add("storage.s3.endpoint: must use https")
			case u.User != nil || u.RawQuery != "" || u.Fragment != "":
				add("storage.s3.endpoint: must not contain credentials, query or fragment")
			}
		}
		pfx, err := normalizePrefix(s3.Prefix)
		if err != nil {
			add("storage.s3.prefix: %v", err)
		} else {
			s3.Prefix = pfx
		}
	}

	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

func parseInterval(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (examples: 30m, 6h)", s)
	}
	if d < MinInterval {
		return 0, fmt.Errorf("%q is below the minimum of %s", s, MinInterval)
	}
	return d, nil
}

func normalizePrefix(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return "", errors.New("must be relative and must not contain backslashes")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return "", errors.New("must not contain . or .. segments")
		}
	}
	return strings.TrimRight(p, "/") + "/", nil
}

// ValidateID checks a repository id of the form github/<owner>/<repo>.
func ValidateID(id string) error {
	if id == "" {
		return errors.New("required")
	}
	if strings.ContainsAny(id, "\\\x00") || strings.HasPrefix(id, "/") {
		return errors.New("must be provider/owner/repo, without backslashes or a leading slash")
	}
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return errors.New("must have exactly three segments: provider/owner/repo")
	}
	if parts[0] != "github" {
		return errors.New("provider must be \"github\" in v0.1")
	}
	for _, s := range parts {
		if s == "." || s == ".." || strings.Contains(s, "..") || !idSegmentRe.MatchString(s) {
			return fmt.Errorf("segment %q is not allowed", s)
		}
	}
	if strings.HasSuffix(parts[2], ".git") {
		return errors.New("repo segment must not end in .git")
	}
	return nil
}

// NormalizeURL accepts only https://github.com/<owner>/<repo>[.git] and
// returns it in canonical form with a single .git suffix.
func NormalizeURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("required")
	}
	base := strings.TrimSuffix(raw, ".git")
	m := githubURLRe.FindStringSubmatch(base)
	if m == nil {
		return "", errors.New("must look like https://github.com/<owner>/<repo>[.git] (no user info, port, query or fragment)")
	}
	for _, s := range m[1:] {
		if strings.Trim(s, ".") == "" {
			return "", errors.New("owner and repo must not be . or ..")
		}
	}
	return "https://github.com/" + m[1] + "/" + m[2] + ".git", nil
}

// Repo returns the repository with the given id.
func (c *Config) Repo(id string) (Repository, bool) {
	for _, r := range c.Repositories {
		if r.ID == id {
			return r, true
		}
	}
	return Repository{}, false
}

// RepoIDs returns configured ids in file order.
func (c *Config) RepoIDs() []string {
	out := make([]string, 0, len(c.Repositories))
	for _, r := range c.Repositories {
		out = append(out, r.ID)
	}
	return out
}

// S3Key is the deterministic object key of a repository's current bundle.
func (c *Config) S3Key(id string) string {
	return path.Join(c.Storage.S3.Prefix, id, "latest.bundle")
}

// TokenEnvFor returns the environment variable holding the repo's PAT.
func (c *Config) TokenEnvFor(r Repository) string { return c.Credentials[r.Credential].TokenEnv }

// CheckEnvironment verifies that the PAT variables for the given repositories
// are set and non-empty. Values are never included in messages.
func (c *Config) CheckEnvironment(repos []Repository, lookup func(string) (string, bool)) []error {
	var errs []error
	seen := map[string]bool{}
	for _, r := range repos {
		env := c.TokenEnvFor(r)
		if seen[env] {
			continue
		}
		seen[env] = true
		v, ok := lookup(env)
		if !ok || strings.TrimSpace(v) == "" {
			errs = append(errs, fmt.Errorf("credential %q: environment variable %s is not set or empty", r.Credential, env))
		}
	}
	return errs
}

// CheckStorage makes sure the local root can be created and written to, without touching Git.
func (c *Config) CheckStorage() error {
	root := c.Storage.Local.Root
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("storage.local.root %s: %w", root, err)
	}
	f, err := os.CreateTemp(root, ".write-test-*")
	if err != nil {
		return fmt.Errorf("storage.local.root %s is not writable: %w", root, err)
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// SortedCredentialNames is a helper for stable output.
func (c *Config) SortedCredentialNames() []string {
	names := make([]string, 0, len(c.Credentials))
	for n := range c.Credentials {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
