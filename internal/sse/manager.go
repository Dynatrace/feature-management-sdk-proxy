package sse

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

// Manager maintains exactly one upstream SSE connection per SDK key hash and
// fans out received events to subscribed SDK clients.
//
// Call OnConfigCached after a fresh config payload is stored in the cache.
// Call OnRefreshed after each background 200 refresh (not 304).
// Never call either from the snapshot load path — SSE is only established
// after the proxy confirms upstream reachability via a live HTTP response.
type Manager struct {
	client    *http.Client
	inst      *metrics.Instruments
	logger    *slog.Logger
	lifecycle func(Event) bool // optional; set via SetLifecycleHandler

	mu    sync.RWMutex
	conns map[string]*keyState
}

type keyState struct {
	sseURL             string
	inactivityDelaySec int
	cancel             context.CancelFunc
	wg                 sync.WaitGroup
	subsMu             sync.Mutex
	subs               []*subscriber
	closed             bool // set by Close under subsMu; checked by Subscribe under subsMu
	// streaming is accessed atomically (no mutex required); all other fields
	// that need protection are covered by cancel/wg (main conn lifecycle) or subsMu.
	streaming atomic.Bool // true only while the upstream read loop is actively reading
}

type subscriber struct {
	ch chan Event
}

// New constructs a Manager. httpClient must not have an overall Timeout set —
// SSE connections are long-lived. Use ResponseHeaderTimeout on the Transport.
func New(httpClient *http.Client, inst *metrics.Instruments, logger *slog.Logger) *Manager {
	return &Manager{
		client: httpClient,
		inst:   inst,
		logger: logger,
		conns:  make(map[string]*keyState),
	}
}

// OnConfigCached is called by the proxy handler after a successful cold fetch
// (200) populates the cache for sdkKey. It starts an upstream SSE connection
// if one is not already running for this key. If the SSE URL changed, the old
// connection is cancelled and a new one is started.
func (m *Manager) OnConfigCached(ctx context.Context, sdkKey string, payload []byte) {
	sseURL, inactivitySec, ok := ExtractSSEURL(payload)
	if !ok {
		m.logger.Debug("sse: no SSE entry in config, skipping upstream connection",
			slog.String("key", cache.Key(sdkKey)),
		)
		return
	}
	m.ensureConn(ctx, cache.Key(sdkKey), sseURL, inactivitySec)
}

// OnRefreshed implements cache.RefreshObserver. It is called by the background
// refresher when a 200 response updates the cached payload (never on 304).
// keyHash is cache.Key(sdkKey).
func (m *Manager) OnRefreshed(keyHash string, payload []byte) {
	sseURL, inactivitySec, ok := ExtractSSEURL(payload)
	if !ok {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	state, exists := m.conns[keyHash]
	if !exists || state.sseURL == sseURL {
		return // no change
	}

	m.logger.Info("sse: SSE URL changed, restarting upstream connection",
		slog.String("key", keyHash),
	)
	if state.cancel != nil {
		state.cancel()
	}
	m.inst.SSEUpstreamReconnects.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("sdk_key_hash", keyHash),
		attribute.String("reason", "url_change"),
	))
	// Old goroutine exits asynchronously; brief overlap is acceptable.
	m.startLocked(context.Background(), keyHash, sseURL, inactivitySec, state)
}

// Subscribe returns a channel that receives events for the given key hash and
// an unsubscribe function. The caller must invoke unsubscribe when done.
// If no upstream connection exists for keyHash yet, the channel will receive
// events once OnConfigCached is called for that key.
func (m *Manager) Subscribe(keyHash string) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	sub := &subscriber{ch: ch}

	m.mu.Lock()
	state, ok := m.conns[keyHash]
	if !ok {
		state = &keyState{}
		m.conns[keyHash] = state
	}
	m.mu.Unlock()

	state.subsMu.Lock()
	if state.closed {
		// Close ran between m.mu.Unlock above and this lock; return a
		// pre-closed channel so the handler exits immediately.
		state.subsMu.Unlock()
		closed := make(chan Event)
		close(closed)
		return closed, func() {}
	}
	state.subs = append(state.subs, sub)
	state.subsMu.Unlock()

	unsubscribe := func() {
		state.subsMu.Lock()
		defer state.subsMu.Unlock()
		for i, s := range state.subs {
			if s == sub {
				state.subs = append(state.subs[:i], state.subs[i+1:]...)
				return
			}
		}
	}
	return ch, unsubscribe
}

// SetLifecycleHandler registers a function to be called for each upstream SSE
// event before fan-out. When it returns true the event is a lifecycle event
// and is not forwarded to SDK subscribers. Must be called before the first
// OnConfigCached call.
func (m *Manager) SetLifecycleHandler(fn func(Event) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lifecycle = fn
}

// Close cancels the upstream SSE connection for keyHash, closes all subscriber
// channels, and removes the state from the manager. It is safe to call on a
// key that has no active connection (no-op). Close does not wait for the
// upstream goroutine to exit; cancellation is async.
func (m *Manager) Close(keyHash string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.conns[keyHash]
	if !ok {
		return
	}

	if state.cancel != nil {
		state.cancel()
		m.inst.SSEUpstreamReconnects.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("sdk_key_hash", keyHash),
			attribute.String("reason", "lifecycle_invalidated"),
		))
	}

	state.subsMu.Lock()
	state.closed = true
	for _, sub := range state.subs {
		close(sub.ch)
	}
	state.subs = nil
	state.subsMu.Unlock()

	delete(m.conns, keyHash)
	m.logger.Info("sse: lifecycle invalidation closed connection",
		slog.String("key", keyHash),
	)
}

// Shutdown cancels all upstream connections and waits for them to drain, or
// until ctx is cancelled.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	states := make([]*keyState, 0, len(m.conns))
	for _, state := range m.conns {
		if state.cancel != nil {
			state.cancel()
		}
		states = append(states, state)
	}
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		for _, state := range states {
			state.wg.Wait()
			state.subsMu.Lock()
			for _, sub := range state.subs {
				close(sub.ch)
			}
			state.subs = nil
			state.subsMu.Unlock()
		}
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HasSubscribers implements cache.Liveness. It returns true when at least one
// SDK client has an active SSE subscription for keyHash. An active subscriber
// is treated as a liveness signal — the entry must not be evicted while clients
// are connected.
func (m *Manager) HasSubscribers(keyHash string) bool {
	m.mu.RLock()
	state, ok := m.conns[keyHash]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	state.subsMu.Lock()
	defer state.subsMu.Unlock()
	return len(state.subs) > 0
}

// RefreshInterval implements cache.RefreshIntervalProvider. It returns
// cache.RefreshIntervalConnected when the upstream SSE read loop for keyHash is
// actively streaming, and cache.RefreshIntervalDisconnected otherwise.
func (m *Manager) RefreshInterval(keyHash string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.conns[keyHash]
	if ok && state.streaming.Load() {
		return cache.RefreshIntervalConnected
	}
	return cache.RefreshIntervalDisconnected
}

func (m *Manager) ensureConn(ctx context.Context, keyHash, sseURL string, inactivitySec int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, exists := m.conns[keyHash]
	if exists && state.cancel != nil {
		if state.sseURL == sseURL {
			return // already running with same URL
		}
		state.cancel()
		m.inst.SSEUpstreamReconnects.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("sdk_key_hash", keyHash),
			attribute.String("reason", "url_change"),
		))
		// Old goroutine exits asynchronously.
	}

	if !exists {
		state = &keyState{}
		m.conns[keyHash] = state
	}

	m.startLocked(ctx, keyHash, sseURL, inactivitySec, state)
}

func (m *Manager) startLocked(ctx context.Context, keyHash, sseURL string, inactivitySec int, state *keyState) {
	connCtx, cancel := context.WithCancel(ctx)
	state.sseURL = sseURL
	state.inactivityDelaySec = inactivitySec
	state.cancel = cancel
	state.streaming.Store(false) // reset on (re)start

	state.wg.Go(func() {
		c := &conn{
			sseURL:             sseURL,
			inactivityDelaySec: inactivitySec,
			keyHash:            keyHash,
			client:             m.client,
			logger:             m.logger,
			inst:               m.inst,
			onEvent: func(evt Event) {
				m.dispatch(keyHash, evt)
			},
			onLifecycle:  m.lifecycle,
			setStreaming: func(v bool) { state.streaming.Store(v) },
		}
		c.run(connCtx)
	})
}

// dispatch fans out an event to all subscribers of keyHash.
func (m *Manager) dispatch(keyHash string, evt Event) {
	m.mu.Lock()
	state, ok := m.conns[keyHash]
	m.mu.Unlock()
	if !ok {
		return
	}

	state.subsMu.Lock()
	subs := make([]*subscriber, len(state.subs))
	copy(subs, state.subs)
	state.subsMu.Unlock()

	if len(subs) == 0 {
		m.inst.SSEUpstreamEvents.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("sdk_key_hash", keyHash),
			attribute.String("result", "dropped_no_subscribers"),
		))
		return
	}

	for _, sub := range subs {
		select {
		case sub.ch <- evt:
			m.inst.SSEUpstreamEvents.Add(context.Background(), 1, metric.WithAttributes(
				attribute.String("sdk_key_hash", keyHash),
				attribute.String("result", "forwarded"),
			))
		default:
			m.inst.SSEUpstreamEvents.Add(context.Background(), 1, metric.WithAttributes(
				attribute.String("sdk_key_hash", keyHash),
				attribute.String("result", "dropped_slow_subscriber"),
			))
			m.logger.Warn("sse: dropped event for slow subscriber",
				slog.String("key", keyHash),
			)
		}
	}
}
