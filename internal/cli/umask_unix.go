//go:build unix

package cli

import "syscall"

// restrictUmask makes everything git-keepr creates (mirrors, bundles, state)
// private to the service user.
func restrictUmask() { syscall.Umask(0o077) }
