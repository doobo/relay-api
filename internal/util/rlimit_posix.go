//go:build linux || darwin

package util

import (
	"errors"
	"fmt"
	"syscall"
)

// ErrFileLimitUnsupported reports a platform without POSIX rlimits.
var ErrFileLimitUnsupported = errors.New("open-file limit is not supported on this platform")

// RaiseFileLimit raises the soft RLIMIT_NOFILE to target, capped by the hard
// limit, and returns the effective before/after values.
//
// The relay holds two file descriptors per active /free tunnel (client socket +
// upstream socket) and deliberately sets no Read/WriteTimeout, so long-lived
// tunnels accumulate descriptors. The common 1024 soft limit is reached at
// roughly 500 concurrent tunnels, after which accept() fails with EMFILE and
// every new request - including /v1 AI traffic - is refused. Raising the soft
// limit at startup removes that wall without needing a systemd override.
//
// A failure is never fatal: the caller logs it and keeps running, because a
// container may pin the limit beyond what the process can change.
func RaiseFileLimit(target uint64) (before, after uint64, err error) {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return 0, 0, fmt.Errorf("read RLIMIT_NOFILE: %w", err)
	}
	before = limit.Cur
	if limit.Cur >= target {
		return before, before, nil
	}
	wanted := target
	if wanted > limit.Max {
		wanted = limit.Max
	}
	limit.Cur = wanted
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return before, before, fmt.Errorf("raise RLIMIT_NOFILE to %d: %w", wanted, err)
	}
	return before, wanted, nil
}
