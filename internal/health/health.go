package health

import (
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// ReadyFunc reports whether the proxy has a usable data source.
// When snapshot persistence is not configured this always returns true once
// the HTTP listener is up. When snapshot persistence is configured, the
// closure should return true only after the snapshot has been loaded (or the
// load deadline has passed). Wire the appropriate closure in main.
type ReadyFunc func() bool

// Handlers holds the three Kubernetes probe handlers.
type Handlers struct {
	logger   *slog.Logger
	ready    ReadyFunc
	wasReady atomic.Bool // tracks false→true transition for one-time log
}

// New returns a Handlers wired with the given readiness function.
func New(ready ReadyFunc, logger *slog.Logger) *Handlers {
	return &Handlers{
		logger: logger,
		ready:  ready,
	}
}

// Live handles GET /livez. Returns 200 as long as the process is running.
func (h *Handlers) Live(c *gin.Context) {
	c.String(http.StatusOK, "ok")
}

// Ready handles GET /readyz. Returns 200 when ReadyFunc reports true, 503 otherwise.
func (h *Handlers) Ready(c *gin.Context) {
	if h.ready() {
		h.logTransition()
		c.String(http.StatusOK, "ok")
		return
	}
	c.String(http.StatusServiceUnavailable, "not ready\n")
}

// Health handles GET /healthz. Aggregates liveness and readiness: returns 200
// only when both are healthy (liveness is always true, so this mirrors /readyz).
func (h *Handlers) Health(c *gin.Context) {
	if h.ready() {
		h.logTransition()
		c.String(http.StatusOK, "ok")
		return
	}
	c.String(http.StatusServiceUnavailable, "not ready\n")
}

// logTransition logs the false→true readiness transition exactly once.
func (h *Handlers) logTransition() {
	if h.wasReady.CompareAndSwap(false, true) {
		h.logger.Info("health: proxy is ready")
	}
}
