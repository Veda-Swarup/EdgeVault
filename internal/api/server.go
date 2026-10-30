package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// NewServer wires up the HTTP mux and returns a configured http.Server.
// Using the standard library mux keeps dependencies minimal and is easy to
// explain in an interview.
func NewServer(h *Handler, addr string, log *slog.Logger) *http.Server {
	mux := http.NewServeMux()

	// Volume management
	mux.HandleFunc("/v1/volumes", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.handleCreateVolume(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// File operations — the path wildcard captures the full path including slashes.
	// We use a single catch-all and dispatch by method + suffix internally.
	mux.HandleFunc("/v1/volumes/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case strings.HasSuffix(path, "/state") && r.Method == http.MethodGet:
			h.handleState(w, r)

		case strings.HasSuffix(path, "/snapshots") && r.Method == http.MethodPost:
			h.handleSnapshot(w, r)

		case strings.HasSuffix(path, "/restore") && r.Method == http.MethodPost:
			h.handleRestore(w, r)

		case strings.HasSuffix(path, "/retention") && r.Method == http.MethodPut:
			h.handleRetention(w, r)

		case strings.HasSuffix(path, "/lease"):
			switch r.Method {
			case http.MethodPost:
				h.handleAcquireLease(w, r)
			case http.MethodDelete:
				h.handleReleaseLease(w, r)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}

		// GET /v1/volumes/{vol}/files/{path}/versions
		case strings.HasSuffix(path, "/versions") && r.Method == http.MethodGet:
			h.handleVersions(w, r)

		// GET/POST /v1/volumes/{vol}/files  (no trailing path after /files)
		case isFilesListPath(path):
			switch r.Method {
			case http.MethodGet:
				h.handleList(w, r)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}

		// PUT/GET/DELETE /v1/volumes/{vol}/files/{path}
		case strings.Contains(path, "/files/"):
			switch r.Method {
			case http.MethodPut:
				h.handlePut(w, r)
			case http.MethodGet:
				h.handleGet(w, r)
			case http.MethodDelete:
				h.handleDelete(w, r)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}

		default:
			http.NotFound(w, r)
		}
	})

	// Health / readiness probes
	mux.Handle("/metrics", h.metricsHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := h.store.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
	})

	return &http.Server{
		Addr:         addr,
		Handler:      logMiddleware(log, mux),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
}

func (h *Handler) metricsHandler() http.Handler {
	if h.metrics != nil {
		return h.metrics
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
	})
}

// isFilesListPath matches paths like /v1/volumes/myvol/files (no trailing /path).
func isFilesListPath(path string) bool {
	// ends with "/files" or "/files?"
	return strings.HasSuffix(path, "/files")
}

// logMiddleware logs each request with duration and status using structured logging.
func logMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

// Shutdown gracefully drains in-flight requests.
func Shutdown(ctx context.Context, srv *http.Server) error {
	return srv.Shutdown(ctx)
}
