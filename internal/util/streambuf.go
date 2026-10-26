package util

import "sync"

// StreamBufferSize is the read buffer used to pump SSE/short-lived streams.
const StreamBufferSize = 32 * 1024

var streamBufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, StreamBufferSize)
		return &buffer
	},
}

// GetStreamBuffer borrows a pooled SSE transfer buffer. The returned pointer is
// owned by the caller until PutStreamBuffer is called.
func GetStreamBuffer() *[]byte {
	return streamBufferPool.Get().(*[]byte)
}

// PutStreamBuffer returns a buffer obtained from GetStreamBuffer.
func PutStreamBuffer(buffer *[]byte) {
	if buffer == nil {
		return
	}
	streamBufferPool.Put(buffer)
}
