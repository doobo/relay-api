//go:build !linux && !darwin

package util

import "errors"

// ErrFileLimitUnsupported reports a platform without POSIX rlimits.
var ErrFileLimitUnsupported = errors.New("open-file limit is not supported on this platform")

// RaiseFileLimit is a no-op off Linux/macOS: Windows has no RLIMIT_NOFILE, and
// its handle limit is bounded by other means. It reports the sentinel error so
// the caller can log at debug level instead of claiming a change.
func RaiseFileLimit(uint64) (before, after uint64, err error) {
	return 0, 0, ErrFileLimitUnsupported
}
