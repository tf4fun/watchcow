package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// RedirectHandler handles redirect requests via HTTP
type RedirectHandler struct{}

// NewRedirectHandler creates a new redirect handler
func NewRedirectHandler() *RedirectHandler {
	return &RedirectHandler{}
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

// decodeBase64 decodes a base64 string, automatically adding padding if needed.
// This handles URLs where '=' padding was stripped by URL processing.
// Supports both URL-safe (-_) and standard (+/) alphabets.
func decodeBase64(s string) ([]byte, error) {
	// Add padding if needed (base64 length should be multiple of 4)
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}

	// Try URL-safe encoding first (uses - and _ instead of + and /)
	data, err := base64.URLEncoding.DecodeString(s)
	if err == nil {
		return data, nil
	}

	// Fall back to standard encoding (uses + and /)
	return base64.StdEncoding.DecodeString(s)
}

// redirectParams holds the decoded parameters from base64 JSON
type redirectParams struct {
	Host string `json:"h"` // redirect host (e.g., https://example.com)
	Port string `json:"p"` // container port
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
}

// ServeHTTP implements http.Handler for redirect requests
// Expected path format: /<base64_json>[/<path...>]
func (h *RedirectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pathInfo := strings.TrimPrefix(r.URL.Path, "/")

	// Parse: <base64_json>[/<path...>]
	var base64Part, path string
	slashIdx := strings.Index(pathInfo, "/")
	if slashIdx != -1 {
		base64Part = pathInfo[:slashIdx]
		path = pathInfo[slashIdx:]
	} else {
		base64Part = pathInfo
		path = "/"
	}

	// Decode base64 - add padding if needed for compatibility with URLs where '=' was stripped
	jsonBytes, err := decodeBase64(base64Part)
	if err != nil {
		h.outputError(w, http.StatusBadRequest, "Invalid base64 encoding: "+err.Error())
		return
	}

	// Parse JSON
	var params redirectParams
	if err := json.Unmarshal(jsonBytes, &params); err != nil {
		h.outputError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v (decoded: %s)", err, string(jsonBytes)))
		return
	}

	if params.Host == "" {
		h.outputError(w, http.StatusBadRequest, "Missing redirect host (h)")
		return
	}
	if params.Port == "" {
		h.outputError(w, http.StatusBadRequest, "Missing container port (p)")
		return
	}

	h.outputHTML(w, params.Host, params.Port, path, sanitizeQueryString(r.URL.RawQuery))
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

	// Create template with js escape function
	funcMap := template.FuncMap{
		"js": template.JSEscapeString,
	}
	tmpl := template.Must(template.New("redirect").Funcs(funcMap).Parse(redirectPageTemplate))
	data := redirectTemplateData{
		RedirectBase:  parsed.Base,
		RedirectPath:  parsed.Path,
		RedirectQuery: parsed.Query,
		ContainerPort: containerPort,
		Path:          path,
		QueryString:   queryString,
	}

	if err := tmpl.Execute(w, data); err != nil {
		fmt.Fprintf(w, "<!-- Template error: %v -->", err)
	}
}

const redirectPageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Redirecting...</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            display: flex;
            justify-content: center;
            align-items: center;
            height: 100vh;
            margin: 0;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
        }
        .container {
            text-align: center;
            padding: 2rem;
        }
        .spinner {
            width: 50px;
            height: 50px;
            border: 4px solid rgba(255,255,255,0.3);
            border-top-color: white;
            border-radius: 50%;
            animation: spin 1s linear infinite;
            margin: 0 auto 1rem;
        }
        @keyframes spin {
            to { transform: rotate(360deg); }
        }
        .status {
            font-size: 0.9rem;
            opacity: 0.8;
            margin-top: 1rem;
        }
        .error {
            color: #ff6b6b;
            background: rgba(0,0,0,0.2);
            padding: 1rem;
            border-radius: 8px;
            margin-top: 1rem;
            display: none;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="spinner"></div>
        <h2>Detecting network...</h2>
        <p class="status" id="status">Checking if you're on the local network...</p>
        <div class="error" id="error"></div>
    </div>

    <script>
    (function() {
        // Redirect host components (from config, may include path and query)
        const REDIRECT_BASE = '{{.RedirectBase | js}}';   // e.g., "https://example.com" or "example.com:8080"
        const REDIRECT_PATH = '{{.RedirectPath}}';        // e.g., "/api/v1" (sanitized)
        const REDIRECT_QUERY = '{{.RedirectQuery}}';      // e.g., "x=1" (sanitized)
        // Container info
        const CONTAINER_PORT = '{{.ContainerPort | js}}';
        // Request components
        const PATH = '{{.Path}}';                         // e.g., "/path2" (sanitized)
        const QUERY_STRING = '{{.QueryString}}';          // e.g., "y=2" (sanitized)

        const statusEl = document.getElementById('status');
        const errorEl = document.getElementById('error');

        function setStatus(msg) {
            statusEl.textContent = msg;
        }

        function showError(msg) {
            errorEl.textContent = msg;
            errorEl.style.display = 'block';
        }

        function redirectTo(url) {
            setStatus('Redirecting to ' + url + '...');
            window.location.replace(url);
        }

        // Merge two paths: /path1 + /path2 = /path1/path2
        function mergePaths(basePath, extraPath) {
            if (!basePath && !extraPath) return '/';
            if (!basePath) return extraPath;
            if (!extraPath || extraPath === '/') return basePath;
            // Remove trailing slash from base, keep leading slash on extra
            const base = basePath.endsWith('/') ? basePath.slice(0, -1) : basePath;
            const extra = extraPath.startsWith('/') ? extraPath : '/' + extraPath;
            return base + extra;
        }

        // Merge two query strings: x=1 + y=2 = x=1&y=2
        function mergeQueryStrings(q1, q2) {
            if (!q1 && !q2) return '';
            if (!q1) return q2;
            if (!q2) return q1;
            return q1 + '&' + q2;
        }

        // Build local URL using current hostname with container port
        function buildLocalURL() {
            const hostname = window.location.hostname;
            const protocol = window.location.protocol;
            let url = protocol + '//' + hostname + ':' + CONTAINER_PORT + PATH;
            if (QUERY_STRING) {
                url += '?' + QUERY_STRING;
            }
            return url;
        }

        // Build external URL with path and query merging
        function buildExternalURL() {
            let base = REDIRECT_BASE;
            // Add protocol if missing
            if (!base.startsWith('http://') && !base.startsWith('https://')) {
                base = window.location.protocol + '//' + base;
            }
            // Merge paths
            const mergedPath = mergePaths(REDIRECT_PATH, PATH);
            // Merge query strings
            const mergedQuery = mergeQueryStrings(REDIRECT_QUERY, QUERY_STRING);

            let url = base + mergedPath;
            if (mergedQuery) {
                url += '?' + mergedQuery;
            }
            return url;
        }

        // Check if we're on a private/local network
        function isPrivateIP(ip) {
            // Check for private IP ranges
            // 10.0.0.0/8
            if (ip.startsWith('10.')) return true;
            // 172.16.0.0/12
            if (ip.startsWith('172.')) {
                const second = parseInt(ip.split('.')[1], 10);
                if (second >= 16 && second <= 31) return true;
            }
            // 192.168.0.0/16
            if (ip.startsWith('192.168.')) return true;
            // localhost
            if (ip === '127.0.0.1' || ip === 'localhost') return true;
            return false;
        }

        // Try to detect if we're on local network by checking hostname
        function isLocalHostname() {
            const hostname = window.location.hostname;

            // Check if it's an IP and if so, check if private
            const ipv4Pattern = /^(\d{1,3}\.){3}\d{1,3}$/;
            if (ipv4Pattern.test(hostname)) {
                return isPrivateIP(hostname);
            }

            // localhost
            if (hostname === 'localhost' || hostname === '127.0.0.1') {
                return true;
            }

            // .local domain (mDNS)
            if (hostname.endsWith('.local')) {
                return true;
            }

            // If no TLD (no dot), likely internal hostname
            if (!hostname.includes('.')) {
                return true;
            }

            return false;
        }

        // Try to connect to local port to verify accessibility
        async function checkLocalAccess() {
            const localURL = buildLocalURL();
            setStatus('Testing local connection...');

            try {
                const controller = new AbortController();
                const timeoutId = setTimeout(() => controller.abort(), 3000);

                // Try a simple fetch with no-cors to check if port is open
                await fetch(localURL, {
                    method: 'HEAD',
                    mode: 'no-cors',
                    signal: controller.signal
                });

                clearTimeout(timeoutId);
                return true;
            } catch (err) {
                // fetch with no-cors throws on network error but not on successful connect
                // So if we get here, it might mean the fetch was aborted or network error
                if (err.name === 'AbortError') {
                    return false;
                }
                // Other errors might still mean connection was made
                return false;
            }
        }

        // Main logic
        async function main() {
            // First, quick check based on hostname
            if (isLocalHostname()) {
                setStatus('Local network detected, verifying access...');

                // Try to verify local access
                const localAccessible = await checkLocalAccess();

                if (localAccessible || isLocalHostname()) {
                    // On local network, redirect to container port
                    redirectTo(buildLocalURL());
                    return;
                }
            }

            // Not on local network or local access failed, redirect to external host
            setStatus('External network detected');
            redirectTo(buildExternalURL());
        }

        // Start the detection
        main().catch(function(err) {
            showError('Detection failed: ' + err.message + '. Redirecting to external host...');
            setTimeout(function() {
                redirectTo(buildExternalURL());
            }, 2000);
        });
    })();
    </script>
</body>
</html>
`
