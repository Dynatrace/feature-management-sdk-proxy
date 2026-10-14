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
	"golang.org/x/time/rate"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
)

// Admission result reasons — used in log entries and metric labels.
const (
	AdmitReasonRateLimit = "rate_limit"
	AdmitReasonFanoutCap = "fanout_cap"
)

// Config holds per-flag-set isolation parameters for the Manager.
// All rate/capacity fields treat 0 as "no limit".
type Config struct {
	// FanoutCapPerKey is the maximum number of downstream SDK client SSE connections
	// per SDK key public ID. 0 means no cap.
	FanoutCapPerKey int
	// SubscribeRatePerSec is the token-bucket rate for new SSE subscribe admits per key.
	// 0 means no rate limit.
	SubscribeRatePerSec int
	// SubscribeBurst is the burst size for the per-key subscribe rate bucket.
	SubscribeBurst int
}

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
	cfg      Config

	lifecycle func(Event) bool // optional; set via SetLifecycleHandler
	refetcher Refetcher        // optional; set via SetRefetcher

	// refetches coalesces event-driven cache refreshes per key.
	refetches  sync.Map // keyID -> *refetchState
	refetchWG  sync.WaitGroup
	baseCtx    context.Context
	baseCancel context.CancelFunc

	mu    sync.RWMutex
	conns map[string]*keyState

	// subscriberCounts tracks the number of active downstream connections per
	// keyID. Incremented by AdmitSubscriber, decremented by DecrementSubscribers.
	subscriberCounts sync.Map // keyID -> *atomic.Int64
	// subscribeLimiters holds the per-key token-bucket rate limiters for subscribe
	// admission. Created lazily by AdmitSubscriber; deleted on Close.
	subscribeLimiters sync.Map // keyID -> *rate.Limiter

	shutdownOnce sync.Once
}

// Refetcher refreshes the cached config for a key from upstream. Implemented
// by *cache.Refresher.
type Refetcher interface {
	RefreshKey(ctx context.Context, keyID string, hint cache.RefetchHint) cache.RefreshOutcome
}

// refetchTimeout bounds a single event-driven cache refresh.
const refetchTimeout = 30 * time.Second

// refetchState holds the latest unprocessed change notification for a key.
type refetchState struct {
	mu      sync.Mutex
	running bool
	pending *pendingRefetch
}

type pendingRefetch struct {
	evt  Event
	hint cache.RefetchHint
	sc   trace.SpanContext
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
func New(httpClient *http.Client, inst *metrics.Instruments, logger *slog.Logger, cfg Config) *Manager {
	srv := eventsource.NewServer()
	srv.BufferSize = 16 // match previous channel buffer size

	baseCtx, baseCancel := context.WithCancel(context.Background())
	return &Manager{
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
		client:     httpClient,
		inst:       inst,
		logger:     logger,
		tracer:     tracenoop.NewTracerProvider().Tracer("sse"),
		esServer:   srv,
		conns:      make(map[string]*keyState),
		cfg:        cfg,
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
			slog.String("sdk_key_public_id", sdkkey.PublicID(sdkKey)),
		)
		return
	}
	m.ensureConn(ctx, sdkkey.PublicID(sdkKey), sseURL, inactivitySec)
}

// OnRefreshed implements cache.RefreshObserver. It is called by the background
// refresher when a 200 response updates the cached payload (never on 304).
// keyID is sdkkey.PublicID(sdkKey).
func (m *Manager) OnRefreshed(keyID string, entry cache.Entry) {
	m.NotifyPolledChange(keyID, entry)

	sseURL, inactivitySec, ok := ExtractSSEURL(entry.Payload)
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
		slog.String("sdk_key_public_id", keyID),
	)
	if state.cancel != nil {
		state.cancel()
	}
	m.startLocked(context.Background(), keyID, sseURL, inactivitySec, state)
}

// NotifyPolledChange publishes a synthesized change notification to keyID's
// subscribers for a config change retrieved by polling, but only while the
// upstream SSE connection for keyID is not streaming: when it is, upstream
// announces changes itself.
func (m *Manager) NotifyPolledChange(keyID string, entry cache.Entry) {
	m.mu.RLock()
	state, ok := m.conns[keyID]
	streaming := ok && state.streaming.Load()
	m.mu.RUnlock()
	if streaming || !m.HasSubscribers(keyID) {
		return
	}
	m.logger.Info("sse: upstream not streaming, notifying subscribers of polled config change",
		slog.String("sdk_key_public_id", keyID),
		slog.String("etag", entry.ETag),
	)
	m.publish(keyID, refetchEvent(keyID, entry), trace.SpanContext{}, "synthesized")
}

// AdmitSubscriber gates on per-key subscribe rate and fan-out cap; increments
// the subscriber count on success. Returns (true, "") on admission, or
// (false, reason) where reason is AdmitReasonRateLimit or AdmitReasonFanoutCap.
// The matching decrement must be called via DecrementSubscribers on disconnect.
func (m *Manager) AdmitSubscriber(keyID string) (bool, string) {
	if m.cfg.SubscribeRatePerSec > 0 {
		limVal, _ := m.subscribeLimiters.LoadOrStore(keyID, rate.NewLimiter(
			rate.Limit(m.cfg.SubscribeRatePerSec), m.cfg.SubscribeBurst,
		))
		if !limVal.(*rate.Limiter).Allow() {
			return false, AdmitReasonRateLimit
		}
	}

	val, _ := m.subscriberCounts.LoadOrStore(keyID, &atomic.Int64{})
	counter := val.(*atomic.Int64)
	if m.cfg.FanoutCapPerKey > 0 {
		for {
			current := counter.Load()
			if current >= int64(m.cfg.FanoutCapPerKey) {
				return false, AdmitReasonFanoutCap
			}
			if counter.CompareAndSwap(current, current+1) {
				break
			}
		}
	} else {
		counter.Add(1)
	}
	return true, ""
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

// SetRefetcher makes the Manager refresh a key's cached config before
// forwarding an upstream change notification, so SDK clients that refetch get the
// new config. Must be called before the first OnConfigCached call.
func (m *Manager) SetRefetcher(r Refetcher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refetcher = r
}

// onUpstreamEvent forwards evt, first refreshing the cache when it is a
// change notification and a Refetcher is set.
func (m *Manager) onUpstreamEvent(keyID string, evt Event, sc trace.SpanContext) {
	m.mu.RLock()
	r := m.refetcher
	m.mu.RUnlock()
	hint, isRefetch := parseRefetch(evt)
	if r == nil || !isRefetch {
		m.publish(keyID, evt, sc, "forwarded")
		return
	}

	v, _ := m.refetches.LoadOrStore(keyID, &refetchState{})
	st := v.(*refetchState)
	st.mu.Lock()
	st.pending = &pendingRefetch{evt: evt, hint: hint, sc: sc} // newer events supersede queued ones
	if st.running {
		st.mu.Unlock()
		return
	}
	st.running = true
	st.mu.Unlock()
	m.refetchWG.Go(func() { m.drainRefetches(keyID, st, r) })
}

// drainRefetches refreshes the cache and then forwards the event, repeating
// once more if further events arrived meanwhile. Events received during a
// refresh are coalesced into one refresh and one forwarded event.
func (m *Manager) drainRefetches(keyID string, st *refetchState, r Refetcher) {
	for {
		st.mu.Lock()
		p := st.pending
		st.pending = nil
		if p == nil {
			st.running = false
			st.mu.Unlock()
			return
		}
		st.mu.Unlock()

		ctx, cancel := context.WithTimeout(m.baseCtx, refetchTimeout)
		outcome := r.RefreshKey(ctx, keyID, p.hint)
		cancel()
		if m.baseCtx.Err() != nil {
			return
		}
		// An evicted key has been closed (subscribers disconnected); forward in
		// every other case: on failure clients refetch the stale entry, as before.
		if outcome != cache.RefreshEvicted {
			m.publish(keyID, p.evt, p.sc, "forwarded")
		}
	}
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
	}

	// Force-disconnect all downstream clients for this channel.
	m.esServer.Unregister(keyID, true)

	// Clean up per-key rate limiter so the next client for a recycled key
	// starts with a fresh bucket rather than a stale one.
	m.subscribeLimiters.Delete(keyID)

	delete(m.conns, keyID)
	m.logger.Info("sse: lifecycle invalidation closed connection",
		slog.String("sdk_key_public_id", keyID),
	)
}

// Shutdown cancels all upstream connections, disconnects all downstream
// clients, and waits for goroutines to drain or until ctx is cancelled.
// It is safe to call multiple times; only the first call performs work.
func (m *Manager) Shutdown(ctx context.Context) error {
	var err error
	m.shutdownOnce.Do(func() {
		m.baseCancel()
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
			m.refetchWG.Wait()
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
	m.mu.RLock()
	defer m.mu.RUnlock()
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
			onLifecycle:        m.lifecycle,
			setStreaming:       func(v bool) { state.streaming.Store(v) },
		}
		// onEvent references c.connectSpanCtx which is populated inside c.run
		// after the setup span ends, so it is always set before any event fires.
		c.onEvent = func(evt Event) {
			m.onUpstreamEvent(keyID, evt, c.connectSpanCtx)
		}
		c.run(connCtx)
	})
}

// publish fans out an event to all downstream subscribers of keyID via the
// eventsource.Server. It emits a short-lived sse.event.forward span per event
// (not per connection), linked to the upstream connect span when there is one.
// result is "forwarded" for upstream events, "synthesized" for polled changes.
func (m *Manager) publish(keyID string, evt Event, connectSC trace.SpanContext, result string) {
	spanOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("sdk_key_public_id", keyID),
			attribute.String("sse.event.type", evt.EventType),
			attribute.String("result", result),
		),
	}
	if connectSC.IsValid() {
		spanOpts = append(spanOpts, trace.WithLinks(trace.Link{SpanContext: connectSC}))
	}
	_, span := m.tracer.Start(context.Background(), "sse.event.forward", spanOpts...)

	m.esServer.Publish([]string{keyID}, publishableEvent{evt: evt})

	m.inst.SSEUpstreamEvents.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", keyID),
		attribute.String("result", result),
	))
	span.End()
}
