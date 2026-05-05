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

	// CaseEmptyPersistenceCold is the startup case when persistence is configured,
	// the backend is reachable, but the store is empty. Proxy starts cold and warms
	// its cache from upstream — valid on first deploy or after a store wipe.
	CaseEmptyPersistenceCold

	// CaseBackendUnreachable is the startup case when persistence is configured
	// but the backend could not be contacted. Proxy exits non-zero so the
	// orchestrator surfaces the failure.
	CaseBackendUnreachable
)

// Classify maps startup state to a StartupCase. persistConfigured reports
// whether a snapshot backend is configured; loaded is the count returned by
// LoadAll; listErrored reports whether the backend List call itself failed
// (distinguishes an unreachable backend from a reachable-but-empty store);
// oldest is the minimum FetchedAt across loaded entries (ignored when loaded==0);
// now and maxAge define the freshness boundary.
func Classify(persistConfigured bool, loaded int, listErrored bool, oldest, now time.Time, maxAge time.Duration) StartupCase {
	if !persistConfigured {
		return CaseColdNoPersistence
	}
	if loaded == 0 {
		if listErrored {
			return CaseBackendUnreachable
		}
		return CaseEmptyPersistenceCold
	}
	if now.Sub(oldest) < maxAge {
		return CaseSnapshotFresh
	}
	return CaseSnapshotStale
}
