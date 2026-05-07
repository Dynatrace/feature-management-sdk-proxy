// Package cache provides in-memory flagd config storage with dynamic
// per-key refresh intervals driven by SSE connection state.
//
// When an upstream SSE connection is active for a key, the Refresher polls
// at RefreshIntervalConnected (10 min) — SSE delivers change notifications so
// polling is only a safety net. When SSE is disconnected or disabled, it polls
// at RefreshIntervalDisconnected (10 s) so flag changes are still delivered promptly.
//
// Callers wire a RefreshIntervalProvider (typically sse.Manager) via
// Refresher.SetIntervalProvider to enable per-key interval selection.
// Without a provider, the Refresher always uses RefreshIntervalDisconnected.
package cache
