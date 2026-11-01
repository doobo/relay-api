// Package metrics holds the process-wide counters exposed by /admin/stats.
// They are intentionally cheap (atomics) because they sit on the relay path.
package metrics

import (
	"sync"
	"sync/atomic"
)

var (
	// TunnelActive is the number of /free handlers currently running.
	TunnelActive atomic.Int64
	// CopyBufferPromotions counts responses that moved from the 8 KiB copy
	// chunk to the configured large chunk. A high ratio means most traffic is
	// bulk; a low one means the small-buffer default is doing its job.
	CopyBufferPromotions atomic.Uint64
)

// http2Errors maps an http.HTTP2Config.CountError type to its counter.
var http2Errors sync.Map // map[string]*atomic.Uint64

// NoteHTTP2Error counts an HTTP/2 protocol error by type. Wire it to
// http.HTTP2Config.CountError on the server.
func NoteHTTP2Error(kind string) {
	counter, _ := http2Errors.LoadOrStore(kind, new(atomic.Uint64))
	counter.(*atomic.Uint64).Add(1)
}

// HTTP2Errors snapshots the HTTP/2 error counters.
func HTTP2Errors() map[string]uint64 {
	out := map[string]uint64{}
	http2Errors.Range(func(key, value any) bool {
		out[key.(string)] = value.(*atomic.Uint64).Load()
		return true
	})
	return out
}
