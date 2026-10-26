package util

import (
	"crypto/rand"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// alphabet matches the reference utils/id.ts: lowercase letters and digits, so
// generated tokens look and behave the same.
const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// randomString returns n characters drawn uniformly (modulo alphabet length)
// from a cryptographically random byte stream.
func randomString(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("util: crypto/rand failed: " + err.Error())
	}
	var b strings.Builder
	b.Grow(n)
	for _, v := range buf {
		b.WriteByte(alphabet[int(v)%len(alphabet)])
	}
	return b.String()
}

// NewRequestID returns a fresh "req_" identifier, matching newRequestId().
func NewRequestID() string {
	return "req_" + randomString(16)
}

var (
	fastRequestIDPrefix = randomString(4)
	fastRequestIDSeq    atomic.Uint64
)

// NewFastRequestID returns a cheap, process-unique request id for
// high-frequency paths (the /free tunnel) where a crypto/rand draw per packet
// is wasted work. The prefix separates processes; the monotonic nanosecond
// timestamp and counter keep ids ordered and unique within this one.
func NewFastRequestID() string {
	var buffer [40]byte
	out := buffer[:0]
	out = append(out, "req_"...)
	out = append(out, fastRequestIDPrefix...)
	out = append(out, '-')
	out = strconv.AppendUint(out, uint64(time.Now().UnixNano()), 36)
	out = append(out, '-')
	out = strconv.AppendUint(out, fastRequestIDSeq.Add(1), 36)
	return string(out)
}

// RandomToken returns a random token of the given length (default 24 in the
// reference). Session tokens use 40.
func RandomToken(length int) string {
	return randomString(length)
}

// NewAPISecret returns the 24-character body of a new client API key (the
// caller prefixes "sk-").
func NewAPISecret() string {
	return randomString(24)
}
