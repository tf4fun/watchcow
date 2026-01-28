package cgi

import (
	"context"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httputil"
	"net/url"
	"strings"
)

// RunCGI runs the CGI handler that proxies requests to the Unix socket server.
// It uses Go's standard library cgi and httputil packages.
func RunCGI(socketPath string) {
	// Create reverse proxy to Unix socket
	proxy := newUnixSocketProxy(socketPath)

	// Use standard CGI handler
	cgi.Serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip CGI prefix from path
		// PATH_INFO: /cgi/ThirdParty/watchcow.dashboard/index.cgi/containers/xxx
		// We need: /containers/xxx
		path := r.URL.Path
		if idx := strings.Index(path, "index.cgi"); idx != -1 {
			path = path[idx+len("index.cgi"):]
			if path == "" {
				path = "/"
			}
		}
		r.URL.Path = path
		r.RequestURI = path
		if r.URL.RawQuery != "" {
			r.RequestURI = path + "?" + r.URL.RawQuery
		}

		// Forward to Unix socket server
		proxy.ServeHTTP(w, r)
	}))
}

// newUnixSocketProxy creates a reverse proxy that connects to a Unix socket.
func newUnixSocketProxy(socketPath string) *httputil.ReverseProxy {
	// Target URL (host doesn't matter for Unix socket, but required for URL parsing)
	target, _ := url.Parse("http://localhost")

	proxy := httputil.NewSingleHostReverseProxy(target)

	// Override transport to use Unix socket
	proxy.Transport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}

	// Custom error handler for when daemon is not running
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(serviceUnavailableHTML))
	}

	return proxy
}

// serviceUnavailableHTML is shown when the daemon is not running
const serviceUnavailableHTML = `<!DOCTYPE html>
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
</html>`
