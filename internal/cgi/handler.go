package cgi

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GetSocketPath returns the socket path based on environment
// Uses TRIM_PKGVAR if set, otherwise falls back to /tmp/watchcow
func GetSocketPath() string {
	pkgVar := os.Getenv("TRIM_PKGVAR")
	if pkgVar != "" {
		return filepath.Join(pkgVar, "watchcow.sock")
	}
	return "/tmp/watchcow/watchcow.sock"
}

// CGIHandler handles CGI requests by proxying to the Unix socket server
type CGIHandler struct {
	socketPath string
	client     *http.Client
}

// NewCGIHandler creates a new CGI handler that proxies to the daemon
func NewCGIHandler() *CGIHandler {
	socketPath := GetSocketPath()

	// Create HTTP client with Unix socket transport
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}

	return &CGIHandler{
		socketPath: socketPath,
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

// HandleCGI processes the CGI request by proxying to the Unix socket server
// Expected PATH_INFO format: /cgi/ThirdParty/<AppName>/index.cgi/redirect/<base64_json>[/<path...>]
func (h *CGIHandler) HandleCGI() {
	pathInfo := os.Getenv("PATH_INFO")
	if pathInfo == "" {
		h.outputError(http.StatusBadRequest, "PATH_INFO not set")
		return
	}

	// Find the path after "index.cgi/"
	// e.g., "/cgi/ThirdParty/app/index.cgi/redirect/<base64>/<path>" -> "redirect/<base64>/<path>"
	idx := strings.Index(pathInfo, "index.cgi/")
	if idx == -1 {
		h.outputError(http.StatusBadRequest, "Invalid CGI path format")
		return
	}
	requestPath := "/" + pathInfo[idx+len("index.cgi/"):]

	// Get query string
	queryString := os.Getenv("QUERY_STRING")

	// Build the full URL for the socket request
	requestURL := "http://localhost" + requestPath
	if queryString != "" {
		requestURL += "?" + queryString
	}

	// Create request
	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		h.outputError(http.StatusInternalServerError, "Failed to create request: "+err.Error())
		return
	}

	// Forward relevant headers
	if host := os.Getenv("HTTP_HOST"); host != "" {
		req.Host = host
	}

	// Execute request to Unix socket
	resp, err := h.client.Do(req)
	if err != nil {
		h.outputServiceUnavailable()
		return
	}
	defer resp.Body.Close()

	// Output CGI response headers
	for key, values := range resp.Header {
		for _, value := range values {
			fmt.Printf("%s: %s\n", key, value)
		}
	}
	fmt.Printf("Status: %d %s\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	fmt.Println()

	// Copy response body
	io.Copy(os.Stdout, resp.Body)
}

// outputError outputs a CGI error response
func (h *CGIHandler) outputError(status int, msg string) {
	fmt.Println("Content-Type: text/html; charset=utf-8")
	fmt.Printf("Status: %d %s\n", status, http.StatusText(status))
	fmt.Println()
	fmt.Printf("<html><body><h1>Error</h1><p>%s</p></body></html>\n", msg)
}

// outputServiceUnavailable outputs a 503 error when the daemon is not running
func (h *CGIHandler) outputServiceUnavailable() {
	fmt.Println("Content-Type: text/html; charset=utf-8")
	fmt.Println("Status: 503 Service Unavailable")
	fmt.Println()
	fmt.Printf(`<!DOCTYPE html>
<html>
<head>
    <title>Service Unavailable</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            display: flex;
            justify-content: center;
            align-items: center;
            height: 100vh;
            margin: 0;
            background: #f5f5f5;
            color: #333;
        }
        .container {
            text-align: center;
            padding: 2rem;
            background: white;
            border-radius: 8px;
            box-shadow: 0 2px 10px rgba(0,0,0,0.1);
        }
        h1 { color: #e74c3c; }
    </style>
</head>
<body>
    <div class="container">
        <h1>Service Unavailable</h1>
        <p>WatchCow service is not running.</p>
        <p>Please ensure the WatchCow daemon is started.</p>
    </div>
</body>
</html>
`)
}
