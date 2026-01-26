package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"watchcow/internal/cgi"
	"watchcow/internal/docker"
	"watchcow/internal/server"
)

// fallbackSocketPath is used when TRIM_PKGVAR is not set
const fallbackSocketPath = "/tmp/watchcow/watchcow.sock"

// getDefaultSocketPath returns socket path based on environment
func getDefaultSocketPath() string {
	if pkgVar := os.Getenv("TRIM_PKGVAR"); pkgVar != "" {
		return filepath.Join(pkgVar, "watchcow.sock")
	}
	return fallbackSocketPath
}

func main() {
	// Define flags
	mode := flag.String("mode", "server", "Run mode: server or cgi")
	socketPath := flag.String("socket", "", "Unix socket path (default: $TRIM_PKGVAR/watchcow.sock or /tmp/watchcow/watchcow.sock)")
	debug := flag.Bool("debug", false, "Enable debug mode")
	flag.Parse()

	// Use default socket path if not specified
	actualSocketPath := *socketPath
	if actualSocketPath == "" {
		actualSocketPath = getDefaultSocketPath()
	}

	switch *mode {
	case "server":
		runServerMode(actualSocketPath, *debug)
	case "cgi":
		runCGIMode(actualSocketPath)
	default:
		fmt.Fprintf(os.Stderr, "Unknown mode: %s (use 'server' or 'cgi')\n", *mode)
		os.Exit(1)
	}
}

// runCGIMode handles CGI requests for redirect functionality
func runCGIMode(socketPath string) {
	handler := cgi.NewCGIHandler(socketPath)
	handler.HandleCGI()
}

// runServerMode runs the Docker monitoring daemon with HTTP server
func runServerMode(socketPath string, debug bool) {
	// Configure slog
	var logLevel slog.Level
	if debug {
		logLevel = slog.LevelDebug
	} else {
		logLevel = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: logLevel,
	}
	logHandler := slog.NewTextHandler(os.Stdout, opts)
	logger := slog.New(logHandler)
	slog.SetDefault(logger)

	slog.Info("WatchCow - fnOS App Generator for Docker")
	slog.Info("========================================")

	// Create context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Step 1: Create Docker monitor first
	monitor, err := docker.NewMonitor()
	if err != nil {
		slog.Error("Failed to create Docker monitor", "error", err)
		os.Exit(1)
	}

	// Step 2: Create HTTP handler and router
	redirectHandler := server.NewRedirectHandler()
	router := server.NewRouter(redirectHandler)

	// Step 3: Create server with monitor injected
	srv := server.New(socketPath, router, monitor)

	// Step 4: Start server (which will start monitor after socket is ready)
	go func() {
		if err := srv.Start(ctx); err != nil {
			slog.Error("Server error", "error", err)
			cancel()
		}
	}()

	// Wait for server to be ready
	<-srv.Ready()

	slog.Info("Monitoring started (Press Ctrl+C to stop)")
	slog.Info("")
	slog.Info("To enable fnOS app generation for a container, add these labels:")
	slog.Info("  watchcow.enable: \"true\"")
	slog.Info("  watchcow.display_name: \"Your App Name\"")
	slog.Info("  watchcow.service_port: \"8080\"")
	slog.Info("")

	// Wait for shutdown signal
	<-sigChan
	slog.Info("Shutting down...")

	// Stop server (which stops monitor too)
	srv.Stop(context.Background())
}
