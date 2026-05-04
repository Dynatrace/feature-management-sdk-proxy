package snapshot

import "time"

// StartupCase describes the proxy's startup data-availability situation.
type StartupCase int

const (
	// CaseColdNoPersistence is the startup case when persistence is not configured.
	// Proxy starts cold; upstream is assumed reachable and will serve on first SDK request.
	CaseColdNoPersistence StartupCase = iota

	// CaseSnapshotFresh is the startup case when persistence is configured,
	// snapshots were loaded, and the oldest entry is within SNAPSHOT_MAX_AGE.
	CaseSnapshotFresh

	// CaseSnapshotStale is the startup case when persistence is configured,
	// snapshots were loaded, but the oldest entry meets or exceeds SNAPSHOT_MAX_AGE.
	// Proxy serves from cache with a loud warning and metric.
	CaseSnapshotStale

	// CaseEmptyPersistence is the startup case when persistence is configured
	// but no snapshot entries were loaded. Proxy has no data source — it must
	// exit non-zero so the orchestrator restarts and surfaces the failure.
	CaseEmptyPersistence
)

// Classify maps startup state to a StartupCase. persistConfigured reports
// whether a snapshot backend is configured; loaded is the count returned by
// LoadAll; oldest is the minimum FetchedAt across loaded entries (ignored when
// loaded==0); now and maxAge define the freshness boundary.
func Classify(persistConfigured bool, loaded int, oldest, now time.Time, maxAge time.Duration) StartupCase {
	if !persistConfigured {
		return CaseColdNoPersistence
	}
	if loaded == 0 {
		return CaseEmptyPersistence
	}
	if now.Sub(oldest) < maxAge {
		return CaseSnapshotFresh
	}
	return CaseSnapshotStale
}
