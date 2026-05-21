package sse

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/launchdarkly/eventsource"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
)

// Manager maintains exactly one upstream SSE connection per SDK key hash and
// fans out received events to subscribed SDK clients via an eventsource.Server.
//
// Call OnConfigCached after a fresh config payload is stored in the cache.
// Call OnRefreshed after each background 200 refresh (not 304).
// Never call either from the snapshot load path — SSE is only established
// after the proxy confirms upstream reachability via a live HTTP response.
type Manager struct {
	client   *http.Client
	inst     *metrics.Instruments
	logger   *slog.Logger
	tracer   trace.Tracer
	esServer *eventsource.Server // downstream fan-out server

	lifecycle func(Event) bool // optional; set via SetLifecycleHandler

	mu    sync.RWMutex
	conns map[string]*keyState

	// subscriberCounts tracks the number of active downstream connections per
	// keyID. Managed by the Handler (increment on connect, decrement on disconnect).
	subscriberCounts sync.Map // keyID -> *atomic.Int64

	shutdownOnce sync.Once
}

type keyState struct {
	sseURL             string
	inactivityDelaySec int
	cancel             context.CancelFunc
	wg                 sync.WaitGroup
	// streaming is accessed atomically; true only while the upstream read loop
	// is actively reading.
	streaming atomic.Bool
}

// New constructs a Manager. httpClient must not have an overall Timeout set —
// SSE connections are long-lived. Use ResponseHeaderTimeout on the Transport.
func New(httpClient *http.Client, inst *metrics.Instruments, logger *slog.Logger) *Manager {
	srv := eventsource.NewServer()
	srv.BufferSize = 16 // match previous channel buffer size

	return &Manager{
		client:   httpClient,
		inst:     inst,
		logger:   logger,
		tracer:   tracenoop.NewTracerProvider().Tracer("sse"),
		esServer: srv,
		conns:    make(map[string]*keyState),
	}
}

// SetTracer wires a TracerProvider's tracer into the Manager so upstream connect
// attempts emit sse.upstream.connect spans. Must be called before OnConfigCached.
func (m *Manager) SetTracer(t trace.Tracer) {
	m.tracer = t
}

// Server returns the underlying eventsource.Server used for downstream fan-out.
// Used by the Handler to serve SSE connections to SDK clients.
func (m *Manager) Server() *eventsource.Server {
	return m.esServer
}

// OnConfigCached is called by the proxy handler after a successful cold fetch
// (200) populates the cache for sdkKey. It starts an upstream SSE connection
// if one is not already running for this key. If the SSE URL changed, the old
// connection is cancelled and a new one is started.
func (m *Manager) OnConfigCached(ctx context.Context, sdkKey string, payload []byte) {
	sseURL, inactivitySec, ok := ExtractSSEURL(payload)
	if !ok {
		m.logger.Debug("sse: no SSE entry in config, skipping upstream connection",
			slog.String("key", cache.PublicID(sdkKey)),
		)
		return
	}
	m.ensureConn(ctx, cache.PublicID(sdkKey), sseURL, inactivitySec)
}

// OnRefreshed implements cache.RefreshObserver. It is called by the background
// refresher when a 200 response updates the cached payload (never on 304).
// keyID is cache.PublicID(sdkKey).
func (m *Manager) OnRefreshed(keyID string, payload []byte) {
	sseURL, inactivitySec, ok := ExtractSSEURL(payload)
	if !ok {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	state, exists := m.conns[keyID]
	if !exists || state.sseURL == sseURL {
		return // no change
	}

	m.logger.Info("sse: SSE URL changed, restarting upstream connection",
		slog.String("key", keyID),
	)
	if state.cancel != nil {
		state.cancel()
	}
	m.inst.SSEUpstreamReconnects.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", keyID),
		attribute.String("reason", "url_change"),
	))
	m.startLocked(context.Background(), keyID, sseURL, inactivitySec, state)
}

// IncrementSubscribers increments the subscriber count for a keyID.
// Called by the Handler when a client connects.
func (m *Manager) IncrementSubscribers(keyID string) {
	val, _ := m.subscriberCounts.LoadOrStore(keyID, &atomic.Int64{})
	val.(*atomic.Int64).Add(1)
}

// DecrementSubscribers decrements the subscriber count for a keyID.
// Called by the Handler when a client disconnects.
func (m *Manager) DecrementSubscribers(keyID string) {
	if val, ok := m.subscriberCounts.Load(keyID); ok {
		val.(*atomic.Int64).Add(-1)
	}
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

// Close cancels the upstream SSE connection for keyID, disconnects all
// downstream subscribers via eventsource.Server.Unregister, and removes state.
// Safe to call on a key that has no active connection (no-op).
func (m *Manager) Close(keyID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.conns[keyID]
	if !ok {
		return
	}

	if state.cancel != nil {
		state.cancel()
		m.inst.SSEUpstreamReconnects.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("sdk_key_public_id", keyID),
			attribute.String("reason", "lifecycle_invalidated"),
		))
	}

	// Force-disconnect all downstream clients for this channel.
	m.esServer.Unregister(keyID, true)

	delete(m.conns, keyID)
	m.logger.Info("sse: lifecycle invalidation closed connection",
		slog.String("key", keyID),
	)
}

// Shutdown cancels all upstream connections, disconnects all downstream
// clients, and waits for goroutines to drain or until ctx is cancelled.
// It is safe to call multiple times; only the first call performs work.
func (m *Manager) Shutdown(ctx context.Context) error {
	var err error
	m.shutdownOnce.Do(func() {
		m.mu.Lock()
		states := make([]*keyState, 0, len(m.conns))
		for _, state := range m.conns {
			if state.cancel != nil {
				state.cancel()
			}
			states = append(states, state)
		}
		m.mu.Unlock()

		// Close the downstream eventsource server in a separate goroutine
		// because eventsource.Server.Close() sends on an unbuffered channel
		// and can block if the internal run loop is processing a publish.
		closeDone := make(chan struct{})
		go func() {
			m.esServer.Close()
			close(closeDone)
		}()
		select {
		case <-closeDone:
		case <-ctx.Done():
			err = ctx.Err()
			return
		}

		done := make(chan struct{})
		go func() {
			for _, state := range states {
				state.wg.Wait()
			}
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}

// HasSubscribers implements cache.Liveness. It returns true when at least one
// SDK client has an active SSE subscription for keyID.
func (m *Manager) HasSubscribers(keyID string) bool {
	if val, ok := m.subscriberCounts.Load(keyID); ok {
		return val.(*atomic.Int64).Load() > 0
	}
	return false
}

// RefreshInterval implements cache.RefreshIntervalProvider. It returns
// cache.RefreshIntervalConnected when the upstream SSE read loop for keyID is
// actively streaming, and cache.RefreshIntervalDisconnected otherwise.
func (m *Manager) RefreshInterval(keyID string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.conns[keyID]
	if ok && state.streaming.Load() {
		return cache.RefreshIntervalConnected
	}
	return cache.RefreshIntervalDisconnected
}

func (m *Manager) ensureConn(ctx context.Context, keyID, sseURL string, inactivitySec int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, exists := m.conns[keyID]
	if exists && state.cancel != nil {
		if state.sseURL == sseURL {
			return // already running with same URL
		}
		state.cancel()
		m.inst.SSEUpstreamReconnects.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("sdk_key_public_id", keyID),
			attribute.String("reason", "url_change"),
		))
	}

	if !exists {
		state = &keyState{}
		m.conns[keyID] = state
	}

	m.startLocked(ctx, keyID, sseURL, inactivitySec, state)
}

func (m *Manager) startLocked(ctx context.Context, keyID, sseURL string, inactivitySec int, state *keyState) {
	connCtx, cancel := context.WithCancel(ctx)
	state.sseURL = sseURL
	state.inactivityDelaySec = inactivitySec
	state.cancel = cancel
	state.streaming.Store(false)

	state.wg.Go(func() {
		c := &conn{
			sseURL:             sseURL,
			inactivityDelaySec: inactivitySec,
			keyID:              keyID,
			client:             m.client,
			logger:             m.logger,
			inst:               m.inst,
			tracer:             m.tracer,
			onEvent: func(evt Event) {
				m.publish(keyID, evt)
			},
			onLifecycle:  m.lifecycle,
			setStreaming: func(v bool) { state.streaming.Store(v) },
		}
		c.run(connCtx)
	})
}

// publish fans out an event to all downstream subscribers of keyID via the
// eventsource.Server.
func (m *Manager) publish(keyID string, evt Event) {
	m.esServer.Publish([]string{keyID}, publishableEvent{evt: evt})

	m.inst.SSEUpstreamEvents.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", keyID),
		attribute.String("result", "forwarded"),
	))
}
