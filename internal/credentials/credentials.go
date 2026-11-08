// Package credentials turns a PAT held in memory into non-interactive Git
// authentication without ever writing it to disk, a URL or an argument vector.
//
// Git is pointed at an askpass helper (the git-keepr binary itself, re-executed
// in askpass mode). The token travels only through the environment of the
// short-lived git child process.
package credentials

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const (
	// EnvAskpassMode switches the git-keepr binary into askpass-helper mode.
	EnvAskpassMode = "GIT_KEEPR_ASKPASS"
	// EnvAskpassUser / EnvAskpassToken carry the answers to git's prompts.
	EnvAskpassUser  = "GIT_KEEPR_ASKPASS_USER"
	EnvAskpassToken = "GIT_KEEPR_ASKPASS_TOKEN"
	// Username is the fixed HTTP basic-auth user name used with PATs.
	Username = "x-access-token"
)

// Auth is the credential for one Git invocation.
type Auth struct {
	Token string
}

// Env returns the environment entries that make the askpass helper answer
// prompts for this credential. helperPath is the executable git should call.
func (a *Auth) Env(helperPath string) []string {
	if a == nil {
		return nil
	}
	return []string{
		"GIT_ASKPASS=" + helperPath,
		"SSH_ASKPASS=" + helperPath,
		EnvAskpassMode + "=1",
		EnvAskpassUser + "=" + Username,
		EnvAskpassToken + "=" + a.Token,
	}
}

// RunAskpass implements the helper side: git calls it with the prompt text as
// the only argument and reads the answer from stdout.
func RunAskpass(args []string, getenv func(string) string, out io.Writer) error {
	if len(args) < 1 {
		return errors.New("askpass: missing prompt argument")
	}
	prompt := strings.ToLower(args[0])
	switch {
	case strings.HasPrefix(prompt, "username"):
		_, err := fmt.Fprintln(out, getenv(EnvAskpassUser))
		return err
	case strings.HasPrefix(prompt, "password"):
		tok := getenv(EnvAskpassToken)
		if tok == "" {
			return errors.New("askpass: no token available")
		}
		_, err := fmt.Fprintln(out, tok)
		return err
	default:
		return errors.New("askpass: unsupported prompt")
	}
}

// MaybeRunAskpass is called first thing in main (and TestMain of packages that
// run authenticated git). When the process was started as an askpass helper it
// answers the prompt and exits; otherwise it returns immediately.
func MaybeRunAskpass() {
	if os.Getenv(EnvAskpassMode) != "1" {
		return
	}
	if err := RunAskpass(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

var userinfoRe = regexp.MustCompile(`(?i)(https?://)[^/\s@]+@`)

// Redact removes secrets from text destined for logs or state files: every
// supplied secret value and any user-info embedded in URLs.
func Redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return userinfoRe.ReplaceAllString(s, "${1}***@")
}
