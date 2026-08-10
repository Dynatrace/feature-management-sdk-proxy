package staticsource

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
)

// reload result labels for proxy.static.reload.total.
const (
	reloadLoaded     = "loaded"
	reloadUnchanged  = "unchanged"
	reloadParseError = "parse_error"
	reloadReadError  = "read_error"
)

// Watcher loads the static config file into a Store and polls it for changes,
// rebuilding and atomically swapping the snapshot when the file content changes.
//
// A read or parse failure during a reload is fail-safe: the last good snapshot
// is retained, an error is logged, and a metric is recorded. The initial load
// (LoadInitial) is not fail-safe — it must succeed for the proxy to start.
type Watcher struct {
	store    *Store
	path     string
	interval time.Duration
	maxBytes int64
	inst     *metrics.Instruments
	logger   *slog.Logger

	// change-detection state, only touched from the single Run/LoadInitial goroutine.
	haveLast    bool
	lastModTime time.Time
	lastSize    int64
	lastHash    [sha256.Size]byte
}

// NewWatcher constructs a Watcher for the file at path. interval is the poll
// period; maxBytes caps how much of the file is read (a larger file is an error,
// same policy as the CDN response cap).
func NewWatcher(store *Store, path string, interval time.Duration, maxBytes int64, inst *metrics.Instruments, logger *slog.Logger) *Watcher {
	return &Watcher{
		store:    store,
		path:     path,
		interval: interval,
		maxBytes: maxBytes,
		inst:     inst,
		logger:   logger,
	}
}

// LoadInitial reads, parses, and installs the first snapshot synchronously.
// It returns an error if the file cannot be read or parsed; the caller must
// treat that as fatal (the proxy has no config to serve).
func (w *Watcher) LoadInitial(ctx context.Context) error {
	raw, fi, err := w.read()
	if err != nil {
		return err
	}
	res, err := build(raw)
	if err != nil {
		return fmt.Errorf("static: parse %s: %w", w.path, err)
	}
	w.store.install(res.entries)
	w.remember(fi, sha256.Sum256(raw))
	w.logLoaded(ctx, res, "initial load")
	w.recordReload(ctx, reloadLoaded)
	w.recordFlagSets(ctx, len(res.entries))
	return nil
}

// Run polls the backing file at the configured interval and reloads on change.
// It blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.checkAndReload(ctx)
		}
	}
}

// checkAndReload performs one poll cycle: a cheap stat pre-check, then a content
// hash comparison, then a rebuild + atomic swap only when the content changed.
func (w *Watcher) checkAndReload(ctx context.Context) {
	fi, err := os.Stat(w.path)
	if err != nil {
		w.logger.ErrorContext(ctx, "static: stat backing file failed, keeping last-good config",
			slog.String("path", w.path), slog.String("error", err.Error()))
		w.recordReload(ctx, reloadReadError)
		return
	}
	if w.haveLast && fi.ModTime().Equal(w.lastModTime) && fi.Size() == w.lastSize {
		w.recordReload(ctx, reloadUnchanged)
		return
	}

	raw, fi, err := w.read()
	if err != nil {
		w.logger.ErrorContext(ctx, "static: read backing file failed, keeping last-good config",
			slog.String("path", w.path), slog.String("error", err.Error()))
		w.recordReload(ctx, reloadReadError)
		return
	}
	sum := sha256.Sum256(raw)
	if w.haveLast && sum == w.lastHash {
		// mtime/size changed but content is identical (e.g. touch, or an atomic
		// rename that swapped the inode). No rebuild; just refresh stat state.
		w.remember(fi, sum)
		w.recordReload(ctx, reloadUnchanged)
		return
	}

	res, err := build(raw)
	if err != nil {
		w.logger.ErrorContext(ctx, "static: reload parse error, keeping last-good config",
			slog.String("path", w.path), slog.String("error", err.Error()))
		w.recordReload(ctx, reloadParseError)
		return
	}
	w.store.install(res.entries)
	w.remember(fi, sum)
	w.logLoaded(ctx, res, "reload")
	w.recordReload(ctx, reloadLoaded)
	w.recordFlagSets(ctx, len(res.entries))
}

// read opens the backing file and reads up to maxBytes+1 bytes, returning an
// error if the file exceeds the cap. It also returns the FileInfo used to seed
// change-detection state.
func (w *Watcher) read() ([]byte, os.FileInfo, error) {
	f, err := os.Open(w.path)
	if err != nil {
		return nil, nil, fmt.Errorf("static: open %s: %w", w.path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("static: stat %s: %w", w.path, err)
	}

	raw, err := io.ReadAll(io.LimitReader(f, w.maxBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("static: read %s: %w", w.path, err)
	}
	if int64(len(raw)) > w.maxBytes {
		return nil, nil, fmt.Errorf("static: %s exceeds %d-byte limit", w.path, w.maxBytes)
	}
	return raw, fi, nil
}

// remember stores the file's change-detection fingerprint.
func (w *Watcher) remember(fi os.FileInfo, sum [sha256.Size]byte) {
	w.haveLast = true
	w.lastModTime = fi.ModTime()
	w.lastSize = fi.Size()
	w.lastHash = sum
}

// logLoaded emits a structured log of a successful (re)load. Flag-set
// identifiers are logged only as public IDs; the full keys (which carry the
// private segment) are never logged. Per-set public IDs are emitted at DEBUG.
func (w *Watcher) logLoaded(ctx context.Context, res buildResult, phase string) {
	w.logger.InfoContext(ctx, "static: config "+phase+" complete",
		slog.Int("flag_sets", len(res.entries)),
		slog.Int("unassigned_flags", res.unassigned),
	)
	if res.unassigned > 0 {
		w.logger.WarnContext(ctx, "static: some flags have no flagSetId and will not be served",
			slog.Int("unassigned_flags", res.unassigned),
		)
	}
	if w.logger.Enabled(ctx, slog.LevelDebug) {
		for setID := range res.entries {
			w.logger.DebugContext(ctx, "static: flag set available",
				slog.String("sdk_key_public_id", sdkkey.PublicID(setID)),
			)
		}
	}
}

func (w *Watcher) recordReload(ctx context.Context, result string) {
	if w.inst == nil {
		return
	}
	w.inst.StaticReloadTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func (w *Watcher) recordFlagSets(ctx context.Context, n int) {
	if w.inst == nil {
		return
	}
	w.inst.StaticFlagSets.Record(ctx, int64(n))
}
