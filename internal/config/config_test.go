package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
version: 1
scheduler:
  default_interval: 6h
  max_concurrency: 2
  sync_on_start: true
credentials:
  personal: {type: github_pat, token_env: GITHUB_PAT_PERSONAL}
  work: {type: github_pat, token_env: GITHUB_PAT_WORK}
repositories:
  - id: github/example/backend
    url: https://github.com/example/backend.git
    credential: personal
    interval: 1h
  - id: github/example/frontend
    url: https://github.com/example/frontend
    credential: personal
storage:
  local: {root: /data}
  s3:
    enabled: true
    bucket: git-keepr-backups
    prefix: repositories
    region: auto
    endpoint: https://acct.r2.cloudflarestorage.com
    path_style: true
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Repositories[0].Every != time.Hour || c.Repositories[1].Every != 6*time.Hour {
		t.Fatalf("intervals: %v %v", c.Repositories[0].Every, c.Repositories[1].Every)
	}
	if c.Repositories[1].URL != "https://github.com/example/frontend.git" {
		t.Fatalf("url not normalized: %s", c.Repositories[1].URL)
	}
	if got := c.S3Key("github/example/backend"); got != "repositories/github/example/backend/latest.bundle" {
		t.Fatalf("s3 key: %s", got)
	}
	if !c.Scheduler.OnStart {
		t.Fatal("sync_on_start should be true")
	}
}

func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("config.example.yaml invalid: %v", err)
	}
}

func TestInvalid(t *testing.T) {
	repl := func(old, new string) string { return strings.Replace(validYAML, old, new, 1) }
	cases := map[string]struct {
		yaml string
		want string
	}{
		"http url":       {repl("https://github.com/example/backend.git", "http://github.com/example/backend.git"), "url"},
		"userinfo":       {repl("https://github.com/example/backend.git", "https://tok@github.com/example/backend.git"), "url"},
		"port":           {repl("https://github.com/example/backend.git", "https://github.com:8443/example/backend.git"), "url"},
		"query":          {repl("https://github.com/example/backend.git", "https://github.com/example/backend.git?x=1"), "url"},
		"fragment":       {repl("https://github.com/example/backend.git", "https://github.com/example/backend.git#f"), "url"},
		"other host":     {repl("https://github.com/example/backend.git", "https://gitlab.com/example/backend.git"), "url"},
		"file scheme":    {repl("https://github.com/example/backend.git", "file:///etc"), "url"},
		"traversal id":   {repl("github/example/backend", "github/../backend"), "id"},
		"absolute id":    {repl("github/example/backend", "/etc/passwd"), "id"},
		"backslash id":   {repl("github/example/backend", `github\example\backend`), "id"},
		"dup id":         {repl("github/example/frontend", "github/example/backend"), "collides"},
		"dup id case":    {repl("github/example/frontend", "github/Example/Backend"), "collides"},
		"unknown cred":   {repl("credential: personal\n    interval", "credential: nope\n    interval"), "unknown credential"},
		"bad duration":   {repl("interval: 1h", "interval: soon"), "invalid duration"},
		"tiny duration":  {repl("interval: 1h", "interval: 5s"), "minimum"},
		"bad version":    {repl("version: 1", "version: 2"), "version"},
		"bad env name":   {repl("GITHUB_PAT_WORK", "BAD NAME"), "token_env"},
		"relative root":  {repl("root: /data", "root: data"), "absolute"},
		"http endpoint":  {repl("https://acct", "http://acct"), "https"},
		"no region":      {repl("region: auto", "region: ''"), "region"},
		"bad bucket":     {repl("git-keepr-backups", "Bad_Bucket"), "bucket"},
		"prefix dotdot":  {repl("prefix: repositories", "prefix: ../x"), "prefix"},
		"unknown key":    {repl("version: 1", "version: 1\nsurprise: true"), "surprise"},
		"token inline":   {repl("token_env: GITHUB_PAT_WORK}", "token_env: GITHUB_PAT_WORK, token: ghp_x}"), "token"},
		"no repos":       {"version: 1\nstorage: {local: {root: /data}}\n", "at least one repository"},
		"wrong cred typ": {repl("type: github_pat, token_env: GITHUB_PAT_WORK", "type: ssh, token_env: GITHUB_PAT_WORK"), "type"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestS3DisabledSkipsS3Validation(t *testing.T) {
	y := strings.Replace(validYAML, "enabled: true", "enabled: false", 1)
	y = strings.Replace(y, "bucket: git-keepr-backups", "bucket: ''", 1)
	if _, err := Parse([]byte(y)); err != nil {
		t.Fatalf("disabled s3 should not be validated: %v", err)
	}
}

func TestCheckEnvironment(t *testing.T) {
	c, _ := Parse([]byte(validYAML))
	env := map[string]string{"GITHUB_PAT_PERSONAL": "  "}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	errs := c.CheckEnvironment(c.Repositories, lookup)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "GITHUB_PAT_PERSONAL") {
		t.Fatalf("expected one error for blank token, got %v", errs)
	}
	env["GITHUB_PAT_PERSONAL"] = "dummy-token"
	if errs := c.CheckEnvironment(c.Repositories, lookup); len(errs) != 0 {
		t.Fatalf("unexpected errors %v", errs)
	}
	// 'work' is unreferenced by any repo, so its missing env var is not an error.
}

func TestCheckStorage(t *testing.T) {
	c, _ := Parse([]byte(validYAML))
	c.Storage.Local.Root = filepath.Join(t.TempDir(), "nested", "root")
	if err := c.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o600)
	c.Storage.Local.Root = filepath.Join(f, "sub")
	if err := c.CheckStorage(); err == nil {
		t.Fatal("expected error for unwritable root")
	}
}

func TestURLNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/o/r":       "https://github.com/o/r.git",
		"https://github.com/o/r.git":   "https://github.com/o/r.git",
		"https://github.com/o/r.x.git": "https://github.com/o/r.x.git",
	} {
		got, err := NormalizeURL(in)
		if err != nil || got != want {
			t.Errorf("%s -> %q, %v (want %q)", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "https://github.com/o", "https://github.com/o/r/extra", "https://github.com//r", "https://github.com/../r", "https://github.com/o/..", "https://www.github.com/o/r", "https://github.com/o/r/"} {
		if _, err := NormalizeURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
