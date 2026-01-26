package docker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"watchcow/internal/app"
	"watchcow/internal/fpkgen"
)

// AppOperation represents an operation to be processed serially
type AppOperation struct {
	Type          string // "install", "start", "stop", "destroy", "container_start"
	AppName       string
	AppDir        string
	ContainerID   string
	ContainerName string
	Labels        map[string]string
	ResultCh      chan error
}

// Monitor watches Docker containers and manages fnOS app installation
type Monitor struct {
	cli       *client.Client
	generator *fpkgen.Generator
	installer *fpkgen.Installer
	stopCh    chan struct{}

	// Track container states - only accessed from operation worker goroutine
	containers map[string]*ContainerState // map[containerID]state

	// App registry for runtime app info lookup
	registry *app.Registry

	// Operation queue for serializing all state changes and appcenter-cli calls
	opQueue chan *AppOperation
}

// ContainerState tracks the state of a monitored container
type ContainerState struct {
	ContainerID   string
	ContainerName string
	AppName       string
	Installed     bool
	Labels        map[string]string
}

// NewMonitor creates a new Docker monitor
func NewMonitor() (*Monitor, error) {
	// Connect to Docker daemon
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	// Create generator
	generator, err := fpkgen.NewGenerator()
	if err != nil {
		cli.Close()
		return nil, fmt.Errorf("failed to create generator: %w", err)
	}

	// Try to create installer (may fail if appcenter-cli not available)
	installer, err := fpkgen.NewInstaller()
	if err != nil {
		slog.Warn("appcenter-cli not available, will only generate app packages", "error", err)
		// Continue without installer - useful for development/testing
	} else {
		slog.Info("Installer ready, apps will be auto-installed via appcenter-cli")
	}

	return &Monitor{
		cli:        cli,
		generator:  generator,
		installer:  installer,
		stopCh:     make(chan struct{}),
		containers: make(map[string]*ContainerState),
		registry:   app.NewRegistry(),
		opQueue:    make(chan *AppOperation, 100),
	}, nil
}

// runOperationWorker processes all operations sequentially (single goroutine owns containers map)
func (m *Monitor) runOperationWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case op := <-m.opQueue:
			switch op.Type {
			case "container_start":
				m.processContainerStart(ctx, op)

			case "install":
				m.processInstall(op)

			case "stop":
				m.processStop(op)

			case "destroy":
				m.processDestroy(op)
			}
		}
	}
}

// processContainerStart handles container start - check if installed, start or generate
func (m *Monitor) processContainerStart(ctx context.Context, op *AppOperation) {
	appName := getAppNameFromLabels(op.Labels, op.ContainerName)

	// Check if already installed in fnOS
	if m.installer != nil && m.installer.IsAppInstalled(appName) {
		// Already installed, register and start it
		slog.Info("App already installed, starting", "app", appName)
		m.containers[op.ContainerID] = &ContainerState{
			ContainerID:   op.ContainerID,
			ContainerName: op.ContainerName,
			AppName:       appName,
			Installed:     true,
			Labels:        op.Labels,
		}
		// Register app in registry (from labels)
		m.registerAppFromLabels(appName, op.ContainerID, op.ContainerName, op.Labels)
		if m.installer != nil {
			m.installer.StartApp(appName)
		}
		return
	}

	// Not installed, register as pending
	m.containers[op.ContainerID] = &ContainerState{
		ContainerID:   op.ContainerID,
		ContainerName: op.ContainerName,
		AppName:       appName,
		Installed:     false,
		Labels:        op.Labels,
	}

	// Generate app package (this blocks the worker, but ensures serialization)
	time.Sleep(2 * time.Second)

	config, appDir, err := m.generator.GenerateFromContainer(ctx, op.ContainerID)
	if err != nil {
		slog.Error("Failed to generate fnOS app", "container", op.ContainerName, "error", err)
		delete(m.containers, op.ContainerID)
		return
	}

	// Check if container was destroyed during generation
	if _, exists := m.containers[op.ContainerID]; !exists {
		slog.Info("Container destroyed during generation, skipping install", "container", op.ContainerName)
		os.RemoveAll(appDir)
		return
	}

	// Install
	slog.Info("Installing fnOS app", "app", config.AppName)
	if m.installer != nil {
		if err := m.installer.InstallLocal(appDir); err != nil {
			slog.Error("Failed to install fnOS app", "app", config.AppName, "error", err)
		} else {
			if state, exists := m.containers[op.ContainerID]; exists {
				state.Installed = true
				state.AppName = config.AppName
			}
			// Register app in registry (from config)
			m.registerAppFromConfig(config, op.ContainerID, op.ContainerName)
			slog.Info("Successfully installed fnOS app", "app", config.AppName)
		}
	}
	os.RemoveAll(appDir)
}

// processInstall handles install operation
func (m *Monitor) processInstall(op *AppOperation) {
	state, exists := m.containers[op.ContainerID]
	if !exists {
		slog.Info("Container no longer tracked, skipping install", "app", op.AppName)
		os.RemoveAll(op.AppDir)
		return
	}

	slog.Info("Installing fnOS app", "app", op.AppName)
	if m.installer != nil {
		if err := m.installer.InstallLocal(op.AppDir); err != nil {
			slog.Error("Failed to install fnOS app", "app", op.AppName, "error", err)
		} else {
			state.Installed = true
			state.AppName = op.AppName
			slog.Info("Successfully installed fnOS app", "app", op.AppName)
		}
	}
	os.RemoveAll(op.AppDir)
}

// processStop handles stop operation
func (m *Monitor) processStop(op *AppOperation) {
	state, exists := m.containers[op.ContainerID]
	if !exists || !state.Installed {
		slog.Debug("Container not tracked or not installed, skipping stop", "id", op.ContainerID)
		return
	}
	slog.Info("Stopping fnOS app", "app", state.AppName)
	if m.installer != nil {
		m.installer.StopApp(state.AppName)
	}
}

// processDestroy handles destroy operation
func (m *Monitor) processDestroy(op *AppOperation) {
	state, exists := m.containers[op.ContainerID]
	if !exists {
		slog.Debug("Container not tracked, skipping destroy", "id", op.ContainerID)
		return
	}

	appName := state.AppName
	wasInstalled := state.Installed

	// Remove from tracking
	delete(m.containers, op.ContainerID)

	// Unregister from app registry
	m.registry.Unregister(appName)
	slog.Debug("Unregistered app from registry", "app", appName)

	// Uninstall if was installed
	if wasInstalled && m.installer != nil {
		slog.Info("Uninstalling fnOS app", "app", appName)
		m.installer.Uninstall(appName)
	}
}

// queueOperation sends an operation to the worker (fire and forget, no wait)
func (m *Monitor) queueOperation(op *AppOperation) {
	select {
	case m.opQueue <- op:
	default:
		slog.Warn("Operation queue full, dropping operation", "type", op.Type, "app", op.AppName)
	}
}

// Start starts monitoring Docker containers
func (m *Monitor) Start(ctx context.Context) {
	slog.Info("Starting Docker monitor...")

	// Start operation worker for serializing all state changes
	go m.runOperationWorker(ctx)

	// Initial scan to process existing containers
	m.scanContainers(ctx)

	// Start listening to Docker events for real-time updates
	go m.listenToDockerEvents(ctx)
}

// listenToDockerEvents listens to Docker daemon events
func (m *Monitor) listenToDockerEvents(ctx context.Context) {
	// Set up event filters
	eventFilters := filters.NewArgs()
	eventFilters.Add("type", "container")
	eventFilters.Add("event", "start")
	eventFilters.Add("event", "stop")
	eventFilters.Add("event", "die")
	eventFilters.Add("event", "destroy")

	eventChan, errChan := m.cli.Events(ctx, events.ListOptions{
		Filters: eventFilters,
	})

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case err := <-errChan:
			if err != nil {
				slog.Warn("Docker event stream error, reconnecting...", "error", err)
				time.Sleep(5 * time.Second)
				go m.listenToDockerEvents(ctx)
				return
			}
		case event := <-eventChan:
			m.handleDockerEvent(ctx, event)
		}
	}
}

// handleDockerEvent processes a Docker event
func (m *Monitor) handleDockerEvent(ctx context.Context, event events.Message) {
	containerName := event.Actor.Attributes["name"]
	containerID := event.Actor.ID
	if len(containerID) > 12 {
		containerID = containerID[:12]
	}

	switch event.Action {
	case "start":
		slog.Info("Container started", "container", containerName, "id", containerID)

		// Inspect container to get full labels (event.Actor.Attributes is incomplete)
		info, err := m.cli.ContainerInspect(ctx, containerID)
		if err != nil {
			slog.Debug("Failed to inspect container", "container", containerName, "error", err)
			return
		}

		labels := info.Config.Labels
		if shouldInstall(labels) {
			m.queueOperation(&AppOperation{
				Type:          "container_start",
				ContainerID:   containerID,
				ContainerName: containerName,
				Labels:        labels,
			})
		}

	case "stop", "die":
		slog.Info("Container stopped", "container", containerName, "id", containerID)
		m.queueOperation(&AppOperation{
			Type:        "stop",
			ContainerID: containerID,
		})

	case "destroy":
		slog.Info("Container destroyed", "container", containerName, "id", containerID)
		m.queueOperation(&AppOperation{
			Type:        "destroy",
			ContainerID: containerID,
		})
	}
}

// getAppNameFromLabels extracts appName from labels
func getAppNameFromLabels(labels map[string]string, containerName string) string {
	appName := labels["watchcow.appname"]
	if appName == "" {
		appName = "watchcow." + containerName
	}
	return appName
}

// shouldInstall checks if a container should be installed as fnOS app
func shouldInstall(labels map[string]string) bool {
	// Check watchcow.enable label
	if labels["watchcow.enable"] != "true" {
		return false
	}

	// Check watchcow.install label (default to "fnos" if enable is true)
	installMode := labels["watchcow.install"]
	return installMode == "fnos" || installMode == "true" || installMode == ""
}

// scanContainers scans all running containers
func (m *Monitor) scanContainers(ctx context.Context) {
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		slog.Error("Failed to list containers", "error", err)
		return
	}

	slog.Info("Scanning existing containers...", "count", len(containers))

	for _, ctr := range containers {
		containerID := ctr.ID[:12]
		containerName := strings.TrimPrefix(ctr.Names[0], "/")

		// Check if should be installed
		if shouldInstall(ctr.Labels) {
			slog.Info("Found container to install", "container", containerName)
			m.queueOperation(&AppOperation{
				Type:          "container_start",
				ContainerID:   containerID,
				ContainerName: containerName,
				Labels:        ctr.Labels,
			})
		}
	}
}

// Stop stops the monitor
func (m *Monitor) Stop() {
	close(m.stopCh)

	if m.generator != nil {
		m.generator.Close()
	}

	if m.cli != nil {
		if err := m.cli.Close(); err != nil {
			slog.Warn("Error closing Docker client", "error", err)
		}
	}
}

// Registry returns the app registry for external access (e.g., by server)
func (m *Monitor) Registry() *app.Registry {
	return m.registry
}

// registerAppFromConfig creates and registers an App instance from fpkgen.AppConfig
func (m *Monitor) registerAppFromConfig(config *fpkgen.AppConfig, containerID, containerName string) {
	appInstance := &app.App{
		AppName:       config.AppName,
		DisplayName:   config.DisplayName,
		ContainerID:   containerID,
		ContainerName: containerName,
		Image:         config.Image,
		Status:        app.StatusRunning,
		Entries:       make([]app.Entry, 0, len(config.Entries)),
	}

	// Convert entries
	for _, e := range config.Entries {
		entry := app.Entry{
			Name:     e.Name,
			Title:    e.Title,
			Protocol: e.Protocol,
			Port:     e.Port,
			Path:     e.Path,
			Redirect: e.Redirect,
		}
		appInstance.Entries = append(appInstance.Entries, entry)
	}

	m.registry.Register(appInstance)
	slog.Debug("Registered app in registry", "app", config.AppName, "entries", len(appInstance.Entries))
}

// registerAppFromLabels creates and registers an App instance from container labels
// Used when app is already installed and we need to reconstruct the app info
func (m *Monitor) registerAppFromLabels(appName, containerID, containerName string, labels map[string]string) {
	appInstance := &app.App{
		AppName:       appName,
		DisplayName:   labels["watchcow.display_name"],
		ContainerID:   containerID,
		ContainerName: containerName,
		Image:         labels["watchcow.image"],
		Status:        app.StatusRunning,
		Entries:       make([]app.Entry, 0),
	}

	if appInstance.DisplayName == "" {
		appInstance.DisplayName = containerName
	}

	// Parse entries from labels using fpkgen's ParseEntries
	defaultPort := labels["watchcow.service_port"]
	defaultIcon := labels["watchcow.icon"]
	entries := fpkgen.ParseEntries(labels, appInstance.DisplayName, defaultIcon, defaultPort)
	for _, e := range entries {
		entry := app.Entry{
			Name:     e.Name,
			Title:    e.Title,
			Protocol: e.Protocol,
			Port:     e.Port,
			Path:     e.Path,
			Redirect: e.Redirect,
		}
		appInstance.Entries = append(appInstance.Entries, entry)
	}

	m.registry.Register(appInstance)
	slog.Debug("Registered app in registry from labels", "app", appName, "entries", len(appInstance.Entries))
}
