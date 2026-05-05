package snapshot

import (
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	now := time.Now()
	maxAge := 24 * time.Hour

	// a timestamp just within maxAge
	fresh := now.Add(-maxAge + time.Minute)
	// a timestamp exactly at the boundary (age == maxAge → stale)
	boundary := now.Add(-maxAge)
	// a timestamp clearly beyond maxAge
	stale := now.Add(-maxAge - time.Minute)

	tests := []struct {
		name              string
		persistConfigured bool
		loaded            int
		listErrored       bool
		oldest            time.Time
		want              StartupCase
	}{
		{
			name:              "no persistence → cold",
			persistConfigured: false,
			loaded:            0,
			oldest:            time.Time{},
			want:              CaseColdNoPersistence,
		},
		{
			name:              "no persistence with phantom loaded count → still cold",
			persistConfigured: false,
			loaded:            5,
			oldest:            fresh,
			want:              CaseColdNoPersistence,
		},
		{
			name:              "persistence + 0 loaded + reachable store → cold start",
			persistConfigured: true,
			loaded:            0,
			listErrored:       false,
			oldest:            time.Time{},
			want:              CaseEmptyPersistenceCold,
		},
		{
			name:              "persistence + 0 loaded + backend unreachable → fatal",
			persistConfigured: true,
			loaded:            0,
			listErrored:       true,
			oldest:            time.Time{},
			want:              CaseBackendUnreachable,
		},
		{
			name:              "persistence + loaded + fresh snapshot",
			persistConfigured: true,
			loaded:            3,
			oldest:            fresh,
			want:              CaseSnapshotFresh,
		},
		{
			name:              "persistence + loaded + age exactly == maxAge → stale",
			persistConfigured: true,
			loaded:            1,
			oldest:            boundary,
			want:              CaseSnapshotStale,
		},
		{
			name:              "persistence + loaded + stale snapshot",
			persistConfigured: true,
			loaded:            2,
			oldest:            stale,
			want:              CaseSnapshotStale,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.persistConfigured, tt.loaded, tt.listErrored, tt.oldest, now, maxAge)
			if got != tt.want {
				t.Errorf("Classify() = %v, want %v", got, tt.want)
			}
		})
	}
}
