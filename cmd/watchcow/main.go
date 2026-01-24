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
	handler := slog.NewTextHandler(os.Stdout, opts)
	logger := slog.New(handler)
	slog.SetDefault(logger)

	slog.Info("WatchCow - fnOS App Generator for Docker")
	slog.Info("========================================")

	// Create context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Create and start Docker monitor
	monitor, err := docker.NewMonitor()
	if err != nil {
		slog.Error("Failed to create Docker monitor", "error", err)
		os.Exit(1)
	}
	defer monitor.Stop()

	// Start monitoring
	go monitor.Start(ctx)

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
}
