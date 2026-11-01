//go:build !unix

package main

import "net"

// listenReusePort falls back to one listener on platforms without SO_REUSEPORT.
// main logs a warning when a larger LISTEN_REUSEPORT was requested.
func listenReusePort(addr string, _ int) ([]net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return []net.Listener{listener}, nil
}
