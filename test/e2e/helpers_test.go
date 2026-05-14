//go:build integration

// Behavior tested here is documented in docs/operations.md — update that doc when adding or changing tests.

package e2e

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// eventually polls cond every interval until it returns true or deadline passes.
// Fails the test with msg if the deadline is reached.
func eventually(t *testing.T, cond func() bool, timeout, interval time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("timed out after %s: %s", timeout, msg)
}

// drainBody reads and closes the response body, returning the content as a string.
func drainBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("drainBody: %v", err)
	}
	return string(b)
}

// assertStatus fails the test if resp.StatusCode != want.
func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body := drainBody(t, resp)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, want, body)
	}
}

// connectSSE opens a streaming GET to url and returns a channel that emits
// complete SSE frames. The goroutine exits when ctx is cancelled or the
// connection is closed. The returned cancel cancels the request context.
func connectSSE(t *testing.T, url string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan string, 32)

	go func() {
		defer close(ch)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()

		var sb strings.Builder
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			sb.WriteString(line)
			sb.WriteByte('\n')
			if line == "" {
				// blank line terminates a frame
				frame := sb.String()
				sb.Reset()
				select {
				case ch <- frame:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, cancel
}

// receiveEvent waits up to timeout for an event containing substr on ch.
// Frames that don't contain substr are discarded.
func receiveEvent(t *testing.T, ch <-chan string, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case frame, ok := <-ch:
			if !ok {
				t.Fatalf("SSE channel closed before receiving event containing %q", substr)
			}
			if strings.Contains(frame, substr) {
				return frame
			}
		case <-deadline:
			t.Fatalf("timed out waiting for SSE event containing %q", substr)
			return ""
		}
	}
}
