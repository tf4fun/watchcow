package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"watchcow/internal/app"
)

// createTestRegistry creates a registry with test apps for testing
func createTestRegistry() *app.Registry {
	registry := app.NewRegistry()

	// Add test app: watchcow.nginx with redirect
	registry.Register(&app.App{
		AppName:     "watchcow.nginx",
		DisplayName: "Nginx",
		ContainerID: "abc123",
		Entries: []app.Entry{
			{
				Name:     "",
				Title:    "Nginx",
				Port:     "27890",
				Redirect: "https://www.bilibili.com",
			},
		},
	})

	// Add test app: watchcow.testapp with multiple entries
	registry.Register(&app.App{
		AppName:     "watchcow.testapp",
		DisplayName: "Test App",
		ContainerID: "def456",
		Entries: []app.Entry{
			{
				Name:     "",
				Title:    "Default",
				Port:     "8080",
				Redirect: "https://example.com",
			},
			{
				Name:     "admin",
				Title:    "Admin Panel",
				Port:     "8081",
				Redirect: "https://admin.example.com",
			},
		},
	})

	// Add test app without redirect
	registry.Register(&app.App{
		AppName:     "watchcow.noredirect",
		DisplayName: "No Redirect",
		ContainerID: "ghi789",
		Entries: []app.Entry{
			{
				Name:  "",
				Title: "Default",
				Port:  "9000",
				// No redirect configured
			},
		},
	})

	return registry
}

// TestRedirectHandler_AppLookup tests redirect via app registry lookup
func TestRedirectHandler_AppLookup(t *testing.T) {
	registry := createTestRegistry()
	handler := NewRedirectHandler(registry)

	// Test: /redirect/watchcow.nginx/_/index.html (default entry)
	req := httptest.NewRequest("GET", "/redirect/watchcow.nginx/_/index.html", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
		t.Errorf("response body: %s", w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "bilibili.com") {
		t.Errorf("response should contain redirect host 'bilibili.com'")
	}
	if !strings.Contains(body, "27890") {
		t.Errorf("response should contain container port '27890'")
	}
	if !strings.Contains(body, "/index.html") {
		t.Errorf("response should contain path '/index.html'")
	}
}

// TestRedirectHandler_NamedEntry tests redirect with named entry
func TestRedirectHandler_NamedEntry(t *testing.T) {
	registry := createTestRegistry()
	handler := NewRedirectHandler(registry)

	// Test: /redirect/watchcow.testapp/admin/dashboard
	req := httptest.NewRequest("GET", "/redirect/watchcow.testapp/admin/dashboard", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "admin.example.com") {
		t.Errorf("response should contain redirect host 'admin.example.com'")
	}
	if !strings.Contains(body, "8081") {
		t.Errorf("response should contain container port '8081'")
	}
}

// TestRedirectHandler_RootPath tests redirect with root path
func TestRedirectHandler_RootPath(t *testing.T) {
	registry := createTestRegistry()
	handler := NewRedirectHandler(registry)

	// Test: /redirect/watchcow.testapp/_ (default entry, root path)
	req := httptest.NewRequest("GET", "/redirect/watchcow.testapp/_", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "example.com") {
		t.Errorf("response should contain redirect host 'example.com'")
	}
}

// TestRedirectHandler_WithQueryString tests redirect with query string
func TestRedirectHandler_WithQueryString(t *testing.T) {
	registry := createTestRegistry()
	handler := NewRedirectHandler(registry)

	req := httptest.NewRequest("GET", "/redirect/watchcow.testapp/_/api/data?foo=bar&baz=123", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "foo=bar") {
		t.Errorf("response should contain query string 'foo=bar'")
	}
}

// TestRedirectHandler_AppNotFound tests error when app not found
func TestRedirectHandler_AppNotFound(t *testing.T) {
	registry := createTestRegistry()
	handler := NewRedirectHandler(registry)

	req := httptest.NewRequest("GET", "/redirect/nonexistent.app/_/path", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "App not found") {
		t.Errorf("response should contain 'App not found' error message")
	}
}

// TestRedirectHandler_EntryNotFound tests error when entry not found
func TestRedirectHandler_EntryNotFound(t *testing.T) {
	registry := createTestRegistry()
	handler := NewRedirectHandler(registry)

	req := httptest.NewRequest("GET", "/redirect/watchcow.testapp/nonexistent/path", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Entry not found") {
		t.Errorf("response should contain 'Entry not found' error message")
	}
}

// TestRedirectHandler_NoRedirectConfigured tests error when entry has no redirect
func TestRedirectHandler_NoRedirectConfigured(t *testing.T) {
	registry := createTestRegistry()
	handler := NewRedirectHandler(registry)

	req := httptest.NewRequest("GET", "/redirect/watchcow.noredirect/_/path", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "does not have redirect configured") {
		t.Errorf("response should contain 'does not have redirect configured' error message")
	}
}

// TestRedirectHandler_InvalidPath tests error for invalid path format
func TestRedirectHandler_InvalidPath(t *testing.T) {
	registry := createTestRegistry()
	handler := NewRedirectHandler(registry)

	// Only appname, no entry
	req := httptest.NewRequest("GET", "/redirect/watchcow.testapp", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Invalid path format") {
		t.Errorf("response should contain 'Invalid path format' error message")
	}
}

// TestParseRedirectHost tests the parseRedirectHost function
func TestParseRedirectHost(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expectedBase  string
		expectedPath  string
		expectedQuery string
	}{
		{
			name:          "simple hostname",
			input:         "example.com",
			expectedBase:  "example.com",
			expectedPath:  "",
			expectedQuery: "",
		},
		{
			name:          "hostname with port",
			input:         "example.com:8080",
			expectedBase:  "example.com:8080",
			expectedPath:  "",
			expectedQuery: "",
		},
		{
			name:          "full URL with https",
			input:         "https://example.com",
			expectedBase:  "https://example.com",
			expectedPath:  "",
			expectedQuery: "",
		},
		{
			name:          "URL with path",
			input:         "https://example.com/api/v1",
			expectedBase:  "https://example.com",
			expectedPath:  "/api/v1",
			expectedQuery: "",
		},
		{
			name:          "URL with query",
			input:         "https://example.com/api?key=value",
			expectedBase:  "https://example.com",
			expectedPath:  "/api",
			expectedQuery: "key=value",
		},
		{
			name:          "hostname with path (no scheme)",
			input:         "example.com/path/to/resource",
			expectedBase:  "example.com",
			expectedPath:  "/path/to/resource",
			expectedQuery: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseRedirectHost(tt.input)
			if result.Base != tt.expectedBase {
				t.Errorf("Base: expected %q, got %q", tt.expectedBase, result.Base)
			}
			if result.Path != tt.expectedPath {
				t.Errorf("Path: expected %q, got %q", tt.expectedPath, result.Path)
			}
			if result.Query != tt.expectedQuery {
				t.Errorf("Query: expected %q, got %q", tt.expectedQuery, result.Query)
			}
		})
	}
}

// TestSanitizeQueryString tests the sanitizeQueryString function
func TestSanitizeQueryString(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "valid simple query",
			input:    "foo=bar",
			expected: "foo=bar",
		},
		{
			name:     "valid multiple params",
			input:    "foo=bar&baz=123",
			expected: "foo=bar&baz=123",
		},
		{
			name:     "valid with encoded chars",
			input:    "name=hello%20world",
			expected: "name=hello%20world",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "invalid with script",
			input:    "foo=<script>alert(1)</script>",
			expected: "",
		},
		{
			name:     "invalid with quotes",
			input:    "foo=\"bar\"",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeQueryString(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}
