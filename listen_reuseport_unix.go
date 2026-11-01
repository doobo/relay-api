//go:build unix

package main

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// listenReusePort binds n listeners to addr with SO_REUSEPORT so the kernel can
// spread accepts across accept loops (and, with several processes, across
// cores). n <= 1 keeps the historical single-listener path and skips the socket
// control call entirely.
func listenReusePort(addr string, n int) ([]net.Listener, error) {
	if n <= 1 {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		return []net.Listener{listener}, nil
	}
	listeners := make([]net.Listener, 0, n)
	for index := 0; index < n; index++ {
		listener, err := listenReusePortOnce(addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return nil, err
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

func listenReusePortOnce(addr string) (net.Listener, error) {
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var setErr error
		if err := raw.Control(func(fd uintptr) {
			setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return setErr
	}}
	return config.Listen(context.Background(), "tcp", addr)
}
