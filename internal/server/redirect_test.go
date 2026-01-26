package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRedirectHandler_Base64WithPadding tests backward compatibility with base64 URLs that have '=' padding
// This is a real URL that was reported as failing: the base64 string ends with '=' which can cause issues
// URL: /cgi/ThirdParty/watchcow.nginx/index.cgi/redirect/eyJoIjoiaHR0cHM6Ly93d3cuYmlsaWJpbGkuY29tIiwicCI6IjI3ODkwIn0=/index.html
func TestRedirectHandler_Base64WithPadding(t *testing.T) {
	handler := NewRedirectHandler()

	// This is the exact base64 string from the reported issue (with '=' padding)
	// Decodes to: {"h":"https://www.bilibili.com","p":"27890"}
	base64WithPadding := "eyJoIjoiaHR0cHM6Ly93d3cuYmlsaWJpbGkuY29tIiwicCI6IjI3ODkwIn0="
	path := "/index.html"

	req := httptest.NewRequest("GET", "/"+base64WithPadding+path, nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
		t.Errorf("response body: %s", w.Body.String())
	}

	body := w.Body.String()
	// Verify the response contains expected redirect data
	// Note: the template uses JS escaping, so "https://" becomes "https:\/\/"
	if !strings.Contains(body, "bilibili.com") {
		t.Errorf("response should contain redirect host 'bilibili.com', got: %s", body[:min(500, len(body))])
	}
	if !strings.Contains(body, "27890") {
		t.Errorf("response should contain container port '27890'")
	}
	if !strings.Contains(body, "/index.html") {
		t.Errorf("response should contain path '/index.html'")
	}
}

// TestRedirectHandler_Base64WithoutPadding tests the new preferred format without '=' padding
func TestRedirectHandler_Base64WithoutPadding(t *testing.T) {
	handler := NewRedirectHandler()

	// Same JSON but encoded with RawURLEncoding (no padding)
	// {"h":"https://www.bilibili.com","p":"27890"}
	base64WithoutPadding := "eyJoIjoiaHR0cHM6Ly93d3cuYmlsaWJpbGkuY29tIiwicCI6IjI3ODkwIn0"
	path := "/index.html"

	req := httptest.NewRequest("GET", "/"+base64WithoutPadding+path, nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
		t.Errorf("response body: %s", w.Body.String())
	}

	body := w.Body.String()
	// Note: the template uses JS escaping
	if !strings.Contains(body, "bilibili.com") {
		t.Errorf("response should contain redirect host 'bilibili.com'")
	}
	if !strings.Contains(body, "27890") {
		t.Errorf("response should contain container port '27890'")
	}
}

// TestRedirectHandler_RootPath tests redirect with root path (no trailing path)
func TestRedirectHandler_RootPath(t *testing.T) {
	handler := NewRedirectHandler()

	// {"h":"example.com","p":"8080"}
	base64Str := "eyJoIjoiZXhhbXBsZS5jb20iLCJwIjoiODA4MCJ9"

	req := httptest.NewRequest("GET", "/"+base64Str, nil)
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
	if !strings.Contains(body, "8080") {
		t.Errorf("response should contain container port '8080'")
	}
}

// TestRedirectHandler_WithQueryString tests redirect with query string
func TestRedirectHandler_WithQueryString(t *testing.T) {
	handler := NewRedirectHandler()

	// {"h":"example.com","p":"8080"}
	base64Str := "eyJoIjoiZXhhbXBsZS5jb20iLCJwIjoiODA4MCJ9"

	req := httptest.NewRequest("GET", "/"+base64Str+"/api/data?foo=bar&baz=123", nil)
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

// TestRedirectHandler_InvalidBase64 tests error handling for invalid base64
func TestRedirectHandler_InvalidBase64(t *testing.T) {
	handler := NewRedirectHandler()

	req := httptest.NewRequest("GET", "/not-valid-base64!!!/path", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Invalid base64") {
		t.Errorf("response should contain 'Invalid base64' error message")
	}
}

// TestRedirectHandler_InvalidJSON tests error handling for valid base64 but invalid JSON
func TestRedirectHandler_InvalidJSON(t *testing.T) {
	handler := NewRedirectHandler()

	// Base64 of "not json"
	base64Str := "bm90IGpzb24"

	req := httptest.NewRequest("GET", "/"+base64Str+"/path", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Invalid JSON") {
		t.Errorf("response should contain 'Invalid JSON' error message")
	}
}

// TestRedirectHandler_MissingHost tests error handling for missing 'h' field
func TestRedirectHandler_MissingHost(t *testing.T) {
	handler := NewRedirectHandler()

	// {"p":"8080"} - missing 'h' field
	base64Str := "eyJwIjoiODA4MCJ9"

	req := httptest.NewRequest("GET", "/"+base64Str+"/path", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Missing redirect host") {
		t.Errorf("response should contain 'Missing redirect host' error message")
	}
}

// TestRedirectHandler_MissingPort tests error handling for missing 'p' field
func TestRedirectHandler_MissingPort(t *testing.T) {
	handler := NewRedirectHandler()

	// {"h":"example.com"} - missing 'p' field
	base64Str := "eyJoIjoiZXhhbXBsZS5jb20ifQ"

	req := httptest.NewRequest("GET", "/"+base64Str+"/path", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Missing container port") {
		t.Errorf("response should contain 'Missing container port' error message")
	}
}

// TestRedirectHandler_HostWithPath tests redirect host that includes a path
func TestRedirectHandler_HostWithPath(t *testing.T) {
	handler := NewRedirectHandler()

	// {"h":"https://example.com/api/v1","p":"8080"} encoded with RawURLEncoding
	base64Str := "eyJoIjoiaHR0cHM6Ly9leGFtcGxlLmNvbS9hcGkvdjEiLCJwIjoiODA4MCJ9"

	req := httptest.NewRequest("GET", "/"+base64Str+"/users", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	// Verify response is the redirect HTML page (contains the script)
	if !strings.Contains(body, "Redirecting") {
		t.Errorf("response should be redirect page")
	}
	// Verify redirect base is present (might be JS escaped)
	if !strings.Contains(body, "example.com") {
		t.Errorf("response should contain redirect base 'example.com'")
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

// TestDecodeBase64 tests the decodeBase64 function with various padding scenarios
func TestDecodeBase64(t *testing.T) {
	// {"h":"https://www.bilibili.com","p":"27890"}
	expectedJSON := `{"h":"https://www.bilibili.com","p":"27890"}`

	tests := []struct {
		name   string
		input  string // base64 encoded string
		expect string // expected decoded string
	}{
		{
			name:   "with padding (=)",
			input:  "eyJoIjoiaHR0cHM6Ly93d3cuYmlsaWJpbGkuY29tIiwicCI6IjI3ODkwIn0=",
			expect: expectedJSON,
		},
		{
			name:   "without padding (stripped =)",
			input:  "eyJoIjoiaHR0cHM6Ly93d3cuYmlsaWJpbGkuY29tIiwicCI6IjI3ODkwIn0",
			expect: expectedJSON,
		},
		{
			name:   "simple string with 2 padding",
			input:  "YWI", // "ab" without padding (should be "YWI=")
			expect: "ab",
		},
		{
			name:   "simple string with 1 padding",
			input:  "YWJj", // "abc" no padding needed
			expect: "abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoded, err := decodeBase64(tt.input)
			if err != nil {
				t.Fatalf("decodeBase64 failed: %v", err)
			}
			if string(decoded) != tt.expect {
				t.Errorf("expected %q, got %q", tt.expect, string(decoded))
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
