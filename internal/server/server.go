package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// MonitorInterface defines the interface for Docker monitor
type MonitorInterface interface {
	Start(ctx context.Context)
	Stop()
}

// Server manages the Unix socket HTTP server and Docker monitor
type Server struct {
	socketPath string
	httpServer *http.Server
	listener   net.Listener
	monitor    MonitorInterface
	ready      chan struct{}
}

// New creates a new Unix socket HTTP server with optional monitor
func New(socketPath string, handler http.Handler, monitor MonitorInterface) *Server {
	return &Server{
		socketPath: socketPath,
		httpServer: &http.Server{
			Handler:      handler,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
		},
		monitor: monitor,
		ready:   make(chan struct{}),
	}
}

// Start starts the server (and monitor if provided) and blocks until the context is cancelled
func (s *Server) Start(ctx context.Context) error {
	// Ensure socket directory exists
	socketDir := filepath.Dir(s.socketPath)
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	// Remove stale socket file if exists
	if _, err := os.Stat(s.socketPath); err == nil {
		slog.Debug("Removing stale socket file", "path", s.socketPath)
		if err := os.Remove(s.socketPath); err != nil {
			return fmt.Errorf("failed to remove stale socket: %w", err)
		}
	}

	// Create Unix socket listener
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on Unix socket: %w", err)
	}
	s.listener = listener

	// Set socket permissions for web server access
	if err := os.Chmod(s.socketPath, 0666); err != nil {
		listener.Close()
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	slog.Info("Unix socket server started", "path", s.socketPath)

	// Signal that server is ready
	close(s.ready)

	// Start monitor if provided (after socket is ready)
	if s.monitor != nil {
		go s.monitor.Start(ctx)
	}

	// Serve HTTP requests in a goroutine
	errCh := make(chan error, 1)
	go func() {
		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// Wait for context cancellation or server error
	select {
	case <-ctx.Done():
		return s.shutdown()
	case err := <-errCh:
		return err
	}
}

// Stop gracefully stops the server and monitor
func (s *Server) Stop(ctx context.Context) error {
	// Stop monitor first
	if s.monitor != nil {
		s.monitor.Stop()
	}
	return s.shutdown()
}

// shutdown performs graceful shutdown of HTTP server
func (s *Server) shutdown() error {
	slog.Info("Shutting down Unix socket server...")

	// Stop monitor
	if s.monitor != nil {
		s.monitor.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		slog.Warn("HTTP server shutdown error", "error", err)
	}

	// Remove socket file
	if s.socketPath != "" {
		if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
			slog.Warn("Failed to remove socket file", "path", s.socketPath, "error", err)
		}
	}

	return nil
}

// Ready returns a channel that is closed when the server is ready to accept connections
func (s *Server) Ready() <-chan struct{} {
	return s.ready
}

// SocketPath returns the Unix socket path
func (s *Server) SocketPath() string {
	return s.socketPath
}
