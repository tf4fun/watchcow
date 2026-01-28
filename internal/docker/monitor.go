package docker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"watchcow/internal/app"
	"watchcow/internal/fpkgen"
)

// ConfigProvider provides stored container configurations.
// Implemented by server.DashboardStorage.
type ConfigProvider interface {
	// GetByKey returns the stored config for a container key, or nil if not found.
	GetByKey(key string) *StoredConfig
}

// StoredConfig represents a saved container configuration (from dashboard).
type StoredConfig struct {
	AppName     string
	DisplayName string
	Description string
	Version     string
	Maintainer  string
	Entries     []StoredEntry
	IconBase64  string
}

// StoredEntry represents a saved entry configuration.
type StoredEntry struct {
	Name       string
	Title      string
	Protocol   string
	Port       string
	Path       string
	UIType     string
	AllUsers   bool
	FileTypes  []string
	NoDisplay  bool
	Redirect   string
	IconBase64 string
}

// AppOperation represents an operation to be processed serially
type AppOperation struct {
	Type          string // "install", "start", "stop", "destroy", "container_start"
	AppName       string
	AppDir        string
	ContainerID   string
	ContainerName string
	Labels        map[string]string
	StoredConfig  *StoredConfig // Config from dashboard storage (if no labels)
	ResultCh      chan error
}

// Monitor watches Docker containers and manages fnOS app installation
type Monitor struct {
	cli            *client.Client
	generator      *fpkgen.Generator
	installer      *fpkgen.Installer
	configProvider ConfigProvider
	stopCh         chan struct{}

	// Track all container states
	containers sync.Map // map[containerID]*ContainerState

	// App registry for runtime app info lookup
	registry *app.Registry

	// Operation queue for serializing all state changes and appcenter-cli calls
	opQueue chan *AppOperation
}

// ContainerState tracks the state of a container
type ContainerState struct {
	ContainerID   string
	ContainerName string
	Image         string
	State         string            // "running", "exited", etc.
	Ports         map[string]string // containerPort -> hostPort
	Labels        map[string]string
	NetworkMode   string // e.g. "host", "bridge", "default"
	// watchcow-specific state
	AppName   string
	Installed bool
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
		cli:       cli,
		generator: generator,
		installer: installer,
		stopCh:    make(chan struct{}),
		registry:  app.NewRegistry(),
		opQueue:   make(chan *AppOperation, 100),
	}, nil
}

// SetConfigProvider sets the config provider for dashboard storage lookup.
func (m *Monitor) SetConfigProvider(provider ConfigProvider) {
	m.configProvider = provider
}

// TriggerInstall triggers app installation for a container using stored config.
// Called by dashboard after saving config.
func (m *Monitor) TriggerInstall(containerID string, storedConfig *StoredConfig) {
	// Get container state
	v, ok := m.containers.Load(containerID)
	if !ok {
		slog.Debug("Container not found for trigger install", "id", containerID)
		return
	}
	state := v.(*ContainerState)

	// Only trigger for running containers
	if state.State != "running" {
		slog.Debug("Container not running, skipping trigger install", "id", containerID, "state", state.State)
		return
	}

	// Check if already installed
	if state.Installed {
		slog.Debug("Container already installed, skipping trigger install", "id", containerID)
		return
	}

	slog.Info("Triggering app install from dashboard", "container", state.ContainerName)
	m.queueOperation(&AppOperation{
		Type:          "dashboard_install",
		ContainerID:   containerID,
		ContainerName: state.ContainerName,
		Labels:        state.Labels,
		StoredConfig:  storedConfig,
	})
}

// GetContainerByKey finds a container by its key (image|ports).
func (m *Monitor) GetContainerByKey(key string) (containerID string, found bool) {
	m.containers.Range(func(k, v any) bool {
		state := v.(*ContainerState)
		containerKey := makeContainerKey(state.Image, state.Ports)
		if containerKey == key {
			containerID = state.ContainerID
			found = true
			return false // stop iteration
		}
		return true
	})
	return
}

// makeContainerKey creates a container key from image and ports.
func makeContainerKey(image string, ports map[string]string) string {
	if len(ports) == 0 {
		return image + "|"
	}

	var portPairs []string
	for containerPort, hostPort := range ports {
		portPairs = append(portPairs, fmt.Sprintf("%s:%s", containerPort, hostPort))
	}
	sort.Strings(portPairs)

	return image + "|" + strings.Join(portPairs, ",")
}

// getStoredConfig looks up stored config for a container.
func (m *Monitor) getStoredConfig(image string, ports map[string]string) *StoredConfig {
	if m.configProvider == nil {
		return nil
	}
	key := makeContainerKey(image, ports)
	return m.configProvider.GetByKey(key)
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
			case "container_start", "dashboard_install":
				m.processContainerStart(ctx, op)

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
	// Determine app name based on config source
	var appName string
	if op.StoredConfig != nil {
		appName = op.StoredConfig.AppName
	} else {
		appName = getAppNameFromLabels(op.Labels, op.ContainerName)
	}

	// Check if already installed in fnOS
	if m.installer != nil && m.installer.IsAppInstalled(appName) {
		// Already installed, update state and start it
		slog.Info("App already installed, starting", "app", appName)
		if v, ok := m.containers.Load(op.ContainerID); ok {
			state := v.(*ContainerState)
			state.AppName = appName
			state.Installed = true
		}
		// Register app in registry
		if op.StoredConfig != nil {
			m.registerAppFromStoredConfig(op.StoredConfig, op.ContainerID, op.ContainerName)
		} else {
			m.registerAppFromLabels(appName, op.ContainerID, op.ContainerName, op.Labels)
		}
		if m.installer != nil {
			m.installer.StartApp(appName)
		}
		return
	}

	// Not installed, update state as pending
	if v, ok := m.containers.Load(op.ContainerID); ok {
		state := v.(*ContainerState)
		state.AppName = appName
		state.Installed = false
	}

	// Generate app package
	time.Sleep(2 * time.Second)

	var config *fpkgen.AppConfig
	var appDir string
	var err error

	if op.StoredConfig != nil {
		// Generate from stored config
		config, appDir, err = m.generateFromStoredConfig(ctx, op.ContainerID, op.StoredConfig)
	} else {
		// Generate from container labels
		config, appDir, err = m.generator.GenerateFromContainer(ctx, op.ContainerID)
	}

	if err != nil {
		slog.Error("Failed to generate fnOS app", "container", op.ContainerName, "error", err)
		m.containers.Delete(op.ContainerID)
		return
	}

	// Check if container was destroyed during generation
	if _, exists := m.containers.Load(op.ContainerID); !exists {
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
			if v, exists := m.containers.Load(op.ContainerID); exists {
				state := v.(*ContainerState)
				state.Installed = true
				state.AppName = config.AppName
			}
			// Register app in registry
			m.registerAppFromConfig(config, op.ContainerID, op.ContainerName)
			slog.Info("Successfully installed fnOS app", "app", config.AppName)
		}
	}
	os.RemoveAll(appDir)
}

// generateFromStoredConfig generates an app package from stored config.
func (m *Monitor) generateFromStoredConfig(ctx context.Context, containerID string, storedCfg *StoredConfig) (*fpkgen.AppConfig, string, error) {
	// Inspect container for runtime info
	info, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to inspect container: %w", err)
	}

	// Convert StoredConfig to fpkgen.AppConfig
	config := &fpkgen.AppConfig{
		AppName:       storedCfg.AppName,
		DisplayName:   storedCfg.DisplayName,
		Description:   storedCfg.Description,
		Version:       storedCfg.Version,
		Maintainer:    storedCfg.Maintainer,
		ContainerID:   containerID,
		ContainerName: strings.TrimPrefix(info.Name, "/"),
		Image:         info.Config.Image,
		Icon:          storedCfg.IconBase64, // Will be handled as base64
		Entries:       make([]fpkgen.Entry, 0, len(storedCfg.Entries)),
	}

	// Convert entries
	for _, e := range storedCfg.Entries {
		entry := fpkgen.Entry{
			Name:      e.Name,
			Title:     e.Title,
			Protocol:  e.Protocol,
			Port:      e.Port,
			Path:      e.Path,
			UIType:    e.UIType,
			AllUsers:  e.AllUsers,
			FileTypes: e.FileTypes,
			NoDisplay: e.NoDisplay,
			Redirect:  e.Redirect,
			Icon:      e.IconBase64,
		}
		config.Entries = append(config.Entries, entry)
	}

	// Set default entry title if empty
	if len(config.Entries) > 0 && config.Entries[0].Title == "" {
		config.Entries[0].Title = config.DisplayName
	}

	// Create temp directory for app package
	appDir, err := os.MkdirTemp("", "watchcow-app-*")
	if err != nil {
		return nil, "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	// Generate package files
	if err := m.generator.GenerateFromConfig(config, appDir); err != nil {
		os.RemoveAll(appDir)
		return nil, "", fmt.Errorf("failed to generate package: %w", err)
	}

	return config, appDir, nil
}

// registerAppFromStoredConfig creates and registers an App instance from stored config.
func (m *Monitor) registerAppFromStoredConfig(storedCfg *StoredConfig, containerID, containerName string) {
	appInstance := &app.App{
		AppName:       storedCfg.AppName,
		DisplayName:   storedCfg.DisplayName,
		Description:   storedCfg.Description,
		Version:       storedCfg.Version,
		Maintainer:    storedCfg.Maintainer,
		ContainerID:   containerID,
		ContainerName: containerName,
		Status:        app.StatusRunning,
		Entries:       make([]app.Entry, 0, len(storedCfg.Entries)),
	}

	for _, e := range storedCfg.Entries {
		entry := app.Entry{
			Name:      e.Name,
			Title:     e.Title,
			Protocol:  e.Protocol,
			Port:      e.Port,
			Path:      e.Path,
			UIType:    e.UIType,
			AllUsers:  e.AllUsers,
			FileTypes: e.FileTypes,
			NoDisplay: e.NoDisplay,
			Redirect:  e.Redirect,
		}
		appInstance.Entries = append(appInstance.Entries, entry)
	}

	m.registry.Register(appInstance)
	slog.Debug("Registered app in registry from stored config", "app", storedCfg.AppName, "entries", len(appInstance.Entries))
}

// processStop handles stop operation
func (m *Monitor) processStop(op *AppOperation) {
	v, exists := m.containers.Load(op.ContainerID)
	if !exists {
		slog.Debug("Container not tracked, skipping stop", "id", op.ContainerID)
		return
	}
	state := v.(*ContainerState)
	if !state.Installed {
		slog.Debug("Container not installed, skipping stop", "id", op.ContainerID)
		return
	}
	slog.Info("Stopping fnOS app", "app", state.AppName)
	if m.installer != nil {
		m.installer.StopApp(state.AppName)
	}
}

// processDestroy handles destroy operation
func (m *Monitor) processDestroy(op *AppOperation) {
	v, exists := m.containers.Load(op.ContainerID)
	if !exists {
		slog.Debug("Container not tracked, skipping destroy", "id", op.ContainerID)
		return
	}
	state := v.(*ContainerState)

	appName := state.AppName
	wasInstalled := state.Installed

	// Remove from tracking
	m.containers.Delete(op.ContainerID)

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

		// Inspect container to get full info
		info, err := m.cli.ContainerInspect(ctx, containerID)
		if err != nil {
			slog.Debug("Failed to inspect container", "container", containerName, "error", err)
			return
		}

		// Extract port mappings
		ports := extractPorts(info.NetworkSettings.Ports)

		// Update container state
		v, loaded := m.containers.Load(containerID)
		var state *ContainerState
		if loaded {
			state = v.(*ContainerState)
		} else {
			state = &ContainerState{
				ContainerID:   containerID,
				ContainerName: containerName,
			}
		}
		state.Image = info.Config.Image
		state.State = "running"
		state.Ports = ports
		state.Labels = info.Config.Labels
		state.NetworkMode = string(info.HostConfig.NetworkMode)
		m.containers.Store(containerID, state)

		// Check if should install: either has label config or has stored config
		hasLabelConfig := shouldInstall(info.Config.Labels)
		storedConfig := m.getStoredConfig(info.Config.Image, ports)

		if hasLabelConfig {
			m.queueOperation(&AppOperation{
				Type:          "container_start",
				ContainerID:   containerID,
				ContainerName: containerName,
				Labels:        info.Config.Labels,
			})
		} else if storedConfig != nil {
			m.queueOperation(&AppOperation{
				Type:          "container_start",
				ContainerID:   containerID,
				ContainerName: containerName,
				Labels:        info.Config.Labels,
				StoredConfig:  storedConfig,
			})
		}

	case "stop", "die":
		slog.Info("Container stopped", "container", containerName, "id", containerID)

		// Update state
		if v, ok := m.containers.Load(containerID); ok {
			state := v.(*ContainerState)
			state.State = "exited"
		}

		// Queue stop operation
		m.queueOperation(&AppOperation{
			Type:        "stop",
			ContainerID: containerID,
		})

	case "destroy":
		slog.Info("Container destroyed", "container", containerName, "id", containerID)

		// Queue destroy operation (processDestroy will handle cleanup and uninstall)
		m.queueOperation(&AppOperation{
			Type:        "destroy",
			ContainerID: containerID,
		})
	}
}

// extractPorts extracts port mappings from container network settings
func extractPorts(portMap nat.PortMap) map[string]string {
	ports := make(map[string]string)
	for port, bindings := range portMap {
		if len(bindings) > 0 && bindings[0].HostPort != "" {
			containerPort := port.Port()
			hostPort := bindings[0].HostPort
			ports[containerPort] = hostPort
		}
	}
	return ports
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

// scanContainers scans all containers and populates the state map
func (m *Monitor) scanContainers(ctx context.Context) {
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		slog.Error("Failed to list containers", "error", err)
		return
	}

	slog.Info("Scanning existing containers...", "count", len(containers))

	for _, ctr := range containers {
		containerID := ctr.ID[:12]
		containerName := strings.TrimPrefix(ctr.Names[0], "/")

		// Extract port mappings
		ports := make(map[string]string)
		for _, p := range ctr.Ports {
			if p.PublicPort > 0 {
				containerPort := fmt.Sprintf("%d", p.PrivatePort)
				hostPort := fmt.Sprintf("%d", p.PublicPort)
				ports[containerPort] = hostPort
			}
		}

		// Add to state map
		m.containers.Store(containerID, &ContainerState{
			ContainerID:   containerID,
			ContainerName: containerName,
			Image:         ctr.Image,
			State:         ctr.State,
			Ports:         ports,
			Labels:        ctr.Labels,
		})

		// Only process running containers
		if ctr.State != "running" {
			continue
		}

		// Check if should install: either has label config or has stored config
		hasLabelConfig := shouldInstall(ctr.Labels)
		storedConfig := m.getStoredConfig(ctr.Image, ports)

		if hasLabelConfig {
			slog.Info("Found label-configured container", "container", containerName)
			m.queueOperation(&AppOperation{
				Type:          "container_start",
				ContainerID:   containerID,
				ContainerName: containerName,
				Labels:        ctr.Labels,
			})
		} else if storedConfig != nil {
			slog.Info("Found storage-configured container", "container", containerName)
			m.queueOperation(&AppOperation{
				Type:          "container_start",
				ContainerID:   containerID,
				ContainerName: containerName,
				Labels:        ctr.Labels,
				StoredConfig:  storedConfig,
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

// ContainerInfo represents container information for the dashboard.
type ContainerInfo struct {
	ID          string
	Name        string
	Image       string
	State       string
	Ports       map[string]string // containerPort -> hostPort
	Labels      map[string]string
	NetworkMode string
}

// ListAllContainers returns all containers from the internal state map.
func (m *Monitor) ListAllContainers(ctx context.Context) ([]ContainerInfo, error) {
	var result []ContainerInfo
	m.containers.Range(func(key, value any) bool {
		state := value.(*ContainerState)
		result = append(result, ContainerInfo{
			ID:          state.ContainerID,
			Name:        state.ContainerName,
			Image:       state.Image,
			State:       state.State,
			Ports:       state.Ports,
			Labels:      state.Labels,
			NetworkMode: state.NetworkMode,
		})
		return true
	})
	return result, nil
}

// TriggerUninstall uninstalls an app by name (called from dashboard when config is deleted).
func (m *Monitor) TriggerUninstall(appName string) {
	if appName == "" {
		return
	}

	slog.Info("Triggering app uninstall from dashboard", "app", appName)

	// Unregister from app registry
	m.registry.Unregister(appName)

	// Uninstall from fnOS
	if m.installer != nil {
		m.installer.Uninstall(appName)
	}

	// Clear installed state for any container with this app name
	m.containers.Range(func(key, value any) bool {
		state := value.(*ContainerState)
		if state.AppName == appName {
			state.AppName = ""
			state.Installed = false
		}
		return true
	})
}
