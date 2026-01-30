package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// DashboardMounter is an interface for mounting dashboard routes.
type DashboardMounter interface {
	Mount(r chi.Router)
}

// NewRouter creates a new chi router with handlers mounted
func NewRouter(redirectHandler http.Handler, dashboardHandler DashboardMounter) chi.Router {
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Recoverer)

	// Mount redirect handler at /redirect
	// Path format: /redirect/<appname>/<entry>[/<path...>]
	r.Mount("/redirect", redirectHandler)

	// Mount dashboard handler at /
	if dashboardHandler != nil {
		r.Group(func(r chi.Router) {
			dashboardHandler.Mount(r)
		})
	}

	return r
}
