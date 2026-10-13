// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

// Listen opens a unix socket at path readable and writable by the owner
// only (0600). A stale socket file left by a dead bridge is removed; a
// socket another process still serves is an error.
func Listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%s is in use by another process", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	// The umask makes the socket 0600 from the moment it exists.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(true)
	return ln, nil
}

// Serve serves h on ln over HTTP/1.1 and unencrypted HTTP/2 until ctx is
// done, then shuts down gracefully (bounded by grace).
func Serve(ctx context.Context, ln net.Listener, h http.Handler, grace time.Duration) error {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler:           h,
		Protocols:         &protocols,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err := srv.Shutdown(sctx)
	if serr := <-errc; serr != nil && !errors.Is(serr, http.ErrServerClosed) && err == nil {
		err = serr
	}
	return err
}
