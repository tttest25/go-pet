package main

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestHTTPServerTimeouts(t *testing.T) {
	s := newHTTPServer("127.0.0.1:0", http.NotFoundHandler(), slog.Default())
	if s.ReadHeaderTimeout != 5*time.Second || s.ReadTimeout != 15*time.Second ||
		s.WriteTimeout != 20*time.Second || s.IdleTimeout != 60*time.Second {
		t.Fatalf("header/read/write/idle=%v/%v/%v/%v want 5s/15s/20s/60s",
			s.ReadHeaderTimeout, s.ReadTimeout, s.WriteTimeout, s.IdleTimeout)
	}
}

func TestHTTPSlowRequestBody(t *testing.T) {
	entered := make(chan struct{})
	readResult := make(chan error, 1)
	s := newHTTPServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		_, err := io.ReadAll(r.Body)
		readResult <- err
	}), slog.Default())
	if s.ReadTimeout != 0 {
		s.ReadTimeout = 100 * time.Millisecond
	}
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	served := make(chan error, 1)
	go func() { served <- s.Serve(listener) }()
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not enter body read")
	}
	select {
	case err := <-readResult:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("server body read error=%v, want timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server body read exceeded watchdog; no server timeout")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}
