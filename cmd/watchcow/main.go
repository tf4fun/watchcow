package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"watchcow/internal/cgi"
	"watchcow/internal/docker"
	"watchcow/internal/server"
)

func main() {
	// Check if running as CGI (via symlink like index.cgi -> watchcow)
	execName := filepath.Base(os.Args[0])
	if strings.HasSuffix(execName, ".cgi") || strings.Contains(execName, "cgi") {
		runCGIMode()
		return
	}

	runDaemonMode()
}

// runCGIMode handles CGI requests for redirect functionality
func runCGIMode() {
	handler := cgi.NewCGIHandler()
	handler.HandleCGI()
}

// runDaemonMode runs the Docker monitoring daemon
func runDaemonMode() {
	// Parse command line flags
	debug := flag.Bool("debug", false, "Enable debug mode")
	flag.Parse()

	// Configure slog
	var logLevel slog.Level
	if *debug {
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
	socketPath := server.GetSocketPath()
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
