package server

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"watchcow/internal/app"
	"watchcow/web"
)

// RedirectHandler handles redirect requests via HTTP
type RedirectHandler struct {
	registry *app.Registry
}

// NewRedirectHandler creates a new redirect handler with registry access
func NewRedirectHandler(registry *app.Registry) *RedirectHandler {
	return &RedirectHandler{registry: registry}
}

// validQueryStringPattern matches safe query string format: key=value(&key=value)*
// Only allows URL-safe characters to prevent XSS
var validQueryStringPattern = regexp.MustCompile(`^([a-zA-Z0-9_~.%-]+=[a-zA-Z0-9_~.%/-]*(&[a-zA-Z0-9_~.%-]+=[a-zA-Z0-9_~.%/-]*)*)?$`)

// sanitizeQueryString validates and returns query string, empty if invalid
func sanitizeQueryString(qs string) string {
	if validQueryStringPattern.MatchString(qs) {
		return qs
	}
	return ""
}

// parsedRedirect holds parsed redirect URL components
type parsedRedirect struct {
	Base  string // scheme + host[:port], e.g., "https://example.com" or "example.com:8080"
	Path  string // path component, e.g., "/api/v1"
	Query string // query string without '?', e.g., "x=1&y=2"
}

// parseRedirectHost parses redirect host which may contain path and query
// Uses url.Parse for robust URL parsing
func parseRedirectHost(host string) parsedRedirect {
	result := parsedRedirect{}

	// Add scheme if missing for url.Parse to work correctly
	urlStr := host
	hasScheme := strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://")
	if !hasScheme {
		urlStr = "http://" + host // temporary scheme for parsing
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		// Fallback: treat entire string as base
		result.Base = host
		return result
	}

	// Build base (scheme + host)
	if hasScheme {
		result.Base = u.Scheme + "://" + u.Host
	} else {
		result.Base = u.Host // without scheme
	}

	result.Path = u.Path
	result.Query = sanitizeQueryString(u.RawQuery)

	return result
}

// redirectTemplateData holds all data for the redirect page template
type redirectTemplateData struct {
	// Redirect host components (parsed from config)
	RedirectBase  string // scheme + host[:port]
	RedirectPath  string // base path from redirect config
	RedirectQuery string // query string from redirect config
	// Container info
	ContainerPort string
	// Request components
	Path        string // path from request
	QueryString string // query string from request
	// Assets
	BulmaCSS template.CSS // Bulma CSS content
}

// ServeHTTP implements http.Handler for redirect requests
// Expected path format: /redirect/<appname>/<entry>[/<path...>]
// Use "_" for default entry (empty name)
func (h *RedirectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip the /redirect prefix (chi Mount doesn't strip it automatically)
	pathInfo := strings.TrimPrefix(r.URL.Path, "/redirect/")

	// Parse: <appname>/<entry>[/<path...>]
	parts := strings.SplitN(pathInfo, "/", 3)
	if len(parts) < 2 {
		h.outputError(w, http.StatusBadRequest, "Invalid path format, expected: /<appname>/<entry>[/<path>]")
		return
	}

	appName := parts[0]
	entryName := parts[1]
	path := "/"
	if len(parts) == 3 && parts[2] != "" {
		path = "/" + parts[2]
	}

	// Convert "_" back to empty string for default entry
	if entryName == "_" {
		entryName = ""
	}

	// Look up app in registry
	appInstance := h.registry.Get(appName)
	if appInstance == nil {
		h.outputError(w, http.StatusNotFound, fmt.Sprintf("App not found: %s", appName))
		return
	}

	// Look up entry
	entry := appInstance.GetEntry(entryName)
	if entry == nil {
		h.outputError(w, http.StatusNotFound, fmt.Sprintf("Entry not found: %s (app: %s)", entryName, appName))
		return
	}

	// Check if entry has redirect configured
	if entry.Redirect == "" {
		h.outputError(w, http.StatusBadRequest, fmt.Sprintf("Entry does not have redirect configured: %s", entryName))
		return
	}

	h.outputHTML(w, entry.Redirect, entry.Port, path, sanitizeQueryString(r.URL.RawQuery))
}

// outputError outputs an error page
func (h *RedirectHandler) outputError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<html><body><h1>Error</h1><p>%s</p></body></html>", msg)
}

// outputHTML outputs the redirect HTML page with JavaScript
func (h *RedirectHandler) outputHTML(w http.ResponseWriter, redirectHost, containerPort, path, queryString string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	// Parse redirect host to extract base, path, and query
	parsed := parseRedirectHost(redirectHost)

	// Load CSS from embedded assets
	cssBytes, err := web.Assets.ReadFile("css/bulma.min.css")
	if err != nil {
		fmt.Fprintf(w, "<!-- CSS load error: %v -->", err)
		return
	}

	// Load template from embedded assets
	tmplBytes, err := web.Assets.ReadFile("templates/redirect.tmpl")
	if err != nil {
		fmt.Fprintf(w, "<!-- Template load error: %v -->", err)
		return
	}

	// Create template with js escape function
	funcMap := template.FuncMap{
		"js": template.JSEscapeString,
	}
	tmpl, err := template.New("redirect").Funcs(funcMap).Parse(string(tmplBytes))
	if err != nil {
		fmt.Fprintf(w, "<!-- Template parse error: %v -->", err)
		return
	}

	data := redirectTemplateData{
		RedirectBase:  parsed.Base,
		RedirectPath:  parsed.Path,
		RedirectQuery: parsed.Query,
		ContainerPort: containerPort,
		Path:          path,
		QueryString:   queryString,
		BulmaCSS:      template.CSS(cssBytes),
	}

	if err := tmpl.Execute(w, data); err != nil {
		fmt.Fprintf(w, "<!-- Template error: %v -->", err)
	}
}
