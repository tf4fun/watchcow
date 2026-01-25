package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter creates a new chi router with the redirect handler mounted
func NewRouter(redirectHandler http.Handler) chi.Router {
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Recoverer)

	// Mount redirect handler at /redirect
	// Path format: /redirect/<base64_json>[/<path...>]
	r.Mount("/redirect", redirectHandler)

	// Future extensibility:
	// r.Mount("/api/status", statusHandler)
	// r.Mount("/api/health", healthHandler)

	return r
}
