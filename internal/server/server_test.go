package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// waitListening polls until addr accepts TCP connections or the timeout expires.
func waitListening(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("server at %s did not start within %v", addr, timeout)
}

func TestServer_StartAndShutdown(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv, err := New(":0", mux, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr := srv.Addr()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	if err := waitListening(addr, time.Second); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(fmt.Sprintf("http://%s/ping", addr))
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Errorf("Start returned unexpected error: %v", err)
	}
}

func TestServer_ShutdownDrainsRequests(t *testing.T) {
	mux := http.NewServeMux()
	started := make(chan struct{})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	srv, err := New(":0", mux, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr := srv.Addr()

	go srv.Start() //nolint:errcheck

	if err := waitListening(addr, time.Second); err != nil {
		t.Fatal(err)
	}

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		resp, err := http.Get(fmt.Sprintf("http://%s/slow", addr))
		if err == nil {
			resp.Body.Close()
		}
	}()

	<-started
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request was not drained before shutdown returned")
	}
}
