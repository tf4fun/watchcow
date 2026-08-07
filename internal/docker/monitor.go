package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"sort"
	"strconv"
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
	// GetCompatibleCandidates returns configs saved with legacy protocol-less keys.
	GetCompatibleCandidates(image string, identityPorts map[string]string, portOptions map[string][]string) []StoredConfigMatch
	// MarkApplied clears Pending only if the stored revision still matches.
	MarkApplied(key, revision string) error
	// MigrateLegacy moves one uniquely-owned legacy config to its canonical key.
	MigrateLegacy(oldKey, newKey string) (*StoredConfig, error)
}

type StoredConfigMatch struct {
	Key    string
	Config *StoredConfig
}

type appInstaller interface {
	InstallLocal(appDir string, labels map[string]string) error
	Uninstall(appName string) error
	StartApp(appName string) error
	StopApp(appName string) error
	IsAppInstalled(appName string) bool
}

// StoredConfig represents a saved container configuration (from dashboard).
type StoredConfig struct {
	Key         string
	AppName     string
	DisplayName string
	Description string
	Version     string
	Maintainer  string
	Entries     []StoredEntry
	IconBase64  string
	Revision    string
	Pending     bool
}

// StoredEntry represents a saved entry configuration.
type StoredEntry struct {
	Name          string
	Title         string
	Protocol      string
	Port          string
	Path          string
	UIType        string
	AllUsers      bool
	FileTypes     []string
	NoDisplay     bool
	Redirect      string
	ForceExternal bool
	IconBase64    string
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
	ConfigKey     string        // Canonical storage key for stale uninstall detection
	ResultCh      chan error
}

// Monitor watches Docker containers and manages fnOS app installation
type Monitor struct {
	cli            *client.Client
	generator      *fpkgen.Generator
	installer      appInstaller
	configProvider ConfigProvider
	stopCh         chan struct{}
	stopOnce       sync.Once

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
	State         string // "running", "exited", etc.
	// Ports includes every published protocol for stable container identity.
	// WebPorts contains only TCP mappings usable by HTTP/HTTPS entries.
	Ports             map[string]string
	WebPorts          map[string]string
	LegacyPortOptions map[string][]string
	Labels            map[string]string
	NetworkMode       string // e.g. "host", "bridge", "default"
	// watchcow-specific state
	AppName   string
	Installed bool
}

func cloneContainerState(state *ContainerState) *ContainerState {
	if state == nil {
		return nil
	}
	cloned := *state
	cloned.Ports = maps.Clone(state.Ports)
	cloned.WebPorts = maps.Clone(state.WebPorts)
	cloned.LegacyPortOptions = cloneStringSliceMap(state.LegacyPortOptions)
	cloned.Labels = maps.Clone(state.Labels)
	return &cloned
}

func cloneStringSliceMap(values map[string][]string) map[string][]string {
	cloned := make(map[string][]string, len(values))
	for key, items := range values {
		cloned[key] = append([]string(nil), items...)
	}
	return cloned
}

// updateContainerState publishes a new immutable snapshot without losing a
// concurrent event or worker update. create may be nil when the state must exist.
func (m *Monitor) updateContainerState(containerID string, create func() *ContainerState, update func(*ContainerState)) *ContainerState {
	for {
		value, exists := m.containers.Load(containerID)
		if !exists {
			if create == nil {
				return nil
			}
			state := create()
			update(state)
			actual, loaded := m.containers.LoadOrStore(containerID, state)
			if !loaded {
				return state
			}
			value = actual
		}

		current := value.(*ContainerState)
		next := cloneContainerState(current)
		update(next)
		if m.containers.CompareAndSwap(containerID, current, next) {
			return next
		}
	}
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
// If app is already installed, it will be uninstalled first then reinstalled with new config.
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

	// If already installed, uninstall first then reinstall with new config
	if state.Installed && state.AppName != "" {
		slog.Info("Config updated, reinstalling app", "container", state.ContainerName, "app", state.AppName)
		m.queueOperation(&AppOperation{
			Type:          "dashboard_reinstall",
			ContainerID:   containerID,
			ContainerName: state.ContainerName,
			AppName:       state.AppName,
			Labels:        maps.Clone(state.Labels),
			StoredConfig:  storedConfig,
		})
		return
	}

	slog.Info("Triggering app install from dashboard", "container", state.ContainerName)
	m.queueOperation(&AppOperation{
		Type:          "dashboard_install",
		ContainerID:   containerID,
		ContainerName: state.ContainerName,
		Labels:        maps.Clone(state.Labels),
		StoredConfig:  storedConfig,
	})
}

// GetContainerByKey finds a container by its key (image|ports).
func (m *Monitor) GetContainerByKey(key string) (containerID string, found bool) {
	m.containers.Range(func(k, v any) bool {
		state := v.(*ContainerState)
		if makeContainerKeyForName(state.Image, state.ContainerName, state.Ports) == key {
			containerID = state.ContainerID
			found = true
			return false
		}
		return true
	})
	if found {
		return
	}

	compatibleID := ""
	compatibleCount := 0
	m.containers.Range(func(k, v any) bool {
		state := v.(*ContainerState)
		if MatchesCompatibleContainerKey(key, state.Image, state.Ports, state.LegacyPortOptions) {
			compatibleID = state.ContainerID
			compatibleCount++
		}
		return true
	})
	if compatibleCount == 1 {
		return compatibleID, true
	}
	return
}

// makeContainerKey creates a container key from image and ports.
func makeContainerKey(image string, ports map[string]string) string {
	return makeContainerKeyForName(image, "", ports)
}

func makeContainerKeyForName(image, containerName string, ports map[string]string) string {
	if containerName != "" {
		key := image + "|@" + strings.TrimPrefix(containerName, "/")
		if len(ports) == 0 {
			return key
		}
		return key + ";" + encodePortPairs(ports)
	}
	if len(ports) == 0 {
		return image + "|"
	}
	return image + "|" + encodePortPairs(ports)
}

func encodePortPairs(ports map[string]string) string {
	var portPairs []string
	for containerPort, hostPort := range ports {
		portPairs = append(portPairs, fmt.Sprintf("%s:%s", containerPort, hostPort))
	}
	sort.Strings(portPairs)

	return strings.Join(portPairs, ",")
}

// getStoredConfig looks up stored config for a container.
func (m *Monitor) getStoredConfig(image string, ports map[string]string) *StoredConfig {
	return m.getStoredConfigForPorts("", image, ports, legacyOptionsFromIdentity(ports))
}

func (m *Monitor) getStoredConfigForPorts(containerID, image string, ports map[string]string, portOptions map[string][]string) *StoredConfig {
	if m.configProvider == nil {
		return nil
	}
	containerName := ""
	if value, ok := m.containers.Load(containerID); ok {
		containerName = value.(*ContainerState).ContainerName
	}
	currentKey := makeContainerKeyForName(image, containerName, ports)
	if config := m.configProvider.GetByKey(currentKey); config != nil {
		return config
	}
	matches := m.configProvider.GetCompatibleCandidates(image, ports, portOptions)
	if len(matches) != 1 {
		return nil
	}
	if containerID != "" {
		ambiguous := false
		m.containers.Range(func(key, value any) bool {
			if key.(string) == containerID {
				return true
			}
			state := value.(*ContainerState)
			if MatchesCompatibleContainerKey(matches[0].Key, state.Image, state.Ports, state.LegacyPortOptions) {
				ambiguous = true
				return false
			}
			return true
		})
		if ambiguous {
			return nil
		}
	}
	if matches[0].Key != currentKey {
		config, err := m.configProvider.MigrateLegacy(matches[0].Key, currentKey)
		if err != nil {
			slog.Error("Failed to migrate legacy dashboard key", "old_key", matches[0].Key, "new_key", currentKey, "error", err)
			return nil
		}
		return config
	}
	return matches[0].Config
}

func legacyOptionsFromIdentity(ports map[string]string) map[string][]string {
	sets := make(map[string]map[string]bool)
	for key, hostPort := range ports {
		containerPort, _, _ := strings.Cut(key, "/")
		if sets[containerPort] == nil {
			sets[containerPort] = make(map[string]bool)
		}
		sets[containerPort][hostPort] = true
	}
	return sortedPortOptions(sets)
}

// MatchesCompatibleContainerKey reports whether an older or renamed storage
// key could represent the current published port bindings.
func MatchesCompatibleContainerKey(key, image string, identityPorts map[string]string, portOptions map[string][]string) bool {
	keyImage, encodedPorts, ok := strings.Cut(key, "|")
	if !ok || keyImage != image {
		return false
	}
	if encodedPorts == "" {
		return len(portOptions) == 0
	}
	if strings.HasPrefix(encodedPorts, "@") {
		_, qualifiedPorts, hasPorts := strings.Cut(encodedPorts, ";")
		if !hasPorts {
			return len(identityPorts) == 0
		}
		return qualifiedPorts == encodePortPairs(identityPorts)
	}
	if strings.Contains(encodedPorts, "/") {
		return encodedPorts == encodePortPairs(identityPorts)
	}

	pairs := strings.Split(encodedPorts, ",")
	if len(pairs) != len(portOptions) {
		return false
	}
	seen := make(map[string]bool, len(pairs))
	for _, pair := range pairs {
		containerPort, hostPort, ok := strings.Cut(pair, ":")
		if !ok || strings.Contains(containerPort, "/") || seen[containerPort] {
			return false
		}
		seen[containerPort] = true
		if !slicesContains(portOptions[containerPort], hostPort) {
			return false
		}
	}
	return true
}

func slicesContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// runOperationWorker serializes app lifecycle and appcenter-cli operations.
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

			case "dashboard_install":
				m.processDashboardInstall(ctx, op)

			case "dashboard_reinstall":
				m.processDashboardReinstall(ctx, op)

			case "stop":
				m.processStop(op)

			case "destroy":
				m.processDestroy(op)

			case "dashboard_uninstall":
				m.processDashboardUninstall(op)
			}
		}
	}
}

// processContainerStart handles container start - check if installed, start or generate
func (m *Monitor) processContainerStart(ctx context.Context, op *AppOperation) {
	m.processContainerStartWithMode(ctx, op, false)
}

func (m *Monitor) processContainerStartWithMode(ctx context.Context, op *AppOperation, forceInstall bool) {
	if op.StoredConfig != nil && op.StoredConfig.Pending && !m.isCurrentPendingConfig(op.StoredConfig) {
		slog.Info("Skipping stale dashboard config operation", "app", op.StoredConfig.AppName, "revision", op.StoredConfig.Revision)
		return
	}

	// Determine app name based on config source
	var appName string
	if op.StoredConfig != nil {
		appName = op.StoredConfig.AppName
	} else {
		appName = getAppNameFromLabels(op.Labels, op.ContainerName)
	}

	// Check if already installed in fnOS
	if !forceInstall && m.installer != nil && m.installer.IsAppInstalled(appName) {
		if op.StoredConfig != nil && op.StoredConfig.Pending {
			op.AppName = appName
			m.processDashboardReinstall(ctx, op)
			return
		}
		// Already installed, transfer ownership to this new container.
		// Clear the app association from any previous container so that when the
		// old container is later destroyed it does not uninstall the live app.
		m.containers.Range(func(k, val any) bool {
			if k.(string) == op.ContainerID {
				return true
			}
			other := val.(*ContainerState)
			if other.AppName == appName {
				m.updateContainerState(k.(string), nil, func(state *ContainerState) {
					if state.AppName == appName {
						state.AppName = ""
						state.Installed = false
					}
				})
			}
			return true
		})
		slog.Info("App already installed, starting", "app", appName)
		m.updateContainerState(op.ContainerID, nil, func(state *ContainerState) {
			state.AppName = appName
			state.Installed = true
		})
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
	m.updateContainerState(op.ContainerID, nil, func(state *ContainerState) {
		state.AppName = appName
		state.Installed = false
	})

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
		return
	}
	if op.StoredConfig != nil && op.StoredConfig.Pending && !m.isCurrentPendingConfig(op.StoredConfig) {
		slog.Info("Discarding package generated for stale dashboard config", "app", config.AppName, "revision", op.StoredConfig.Revision)
		os.RemoveAll(appDir)
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
		if err := m.installer.InstallLocal(appDir, op.Labels); err != nil {
			slog.Error("Failed to install fnOS app", "app", config.AppName, "error", err)
		} else {
			m.updateContainerState(op.ContainerID, nil, func(state *ContainerState) {
				state.Installed = true
				state.AppName = config.AppName
			})
			// Register app in registry
			m.registerAppFromConfig(config, op.ContainerID, op.ContainerName)
			if op.StoredConfig != nil && m.configProvider != nil {
				if err := m.configProvider.MarkApplied(op.StoredConfig.Key, op.StoredConfig.Revision); err != nil {
					slog.Error("Failed to mark dashboard config as applied", "app", config.AppName, "error", err)
				}
			}
			slog.Info("Successfully installed fnOS app", "app", config.AppName)
		}
	}
	os.RemoveAll(appDir)
}

func (m *Monitor) processDashboardInstall(ctx context.Context, op *AppOperation) {
	if op.StoredConfig != nil && !m.isCurrentPendingConfig(op.StoredConfig) {
		return
	}
	if appName := m.installedAppName(op.ContainerID); appName != "" {
		op.AppName = appName
		m.processDashboardReinstall(ctx, op)
		return
	}
	m.processContainerStart(ctx, op)
}

func (m *Monitor) isCurrentPendingConfig(config *StoredConfig) bool {
	if config == nil || config.Key == "" || config.Revision == "" || m.configProvider == nil {
		return true
	}
	current := m.configProvider.GetByKey(config.Key)
	return current != nil && current.Pending && current.Revision == config.Revision
}

func (m *Monitor) installedAppName(containerID string) string {
	value, ok := m.containers.Load(containerID)
	if !ok {
		return ""
	}
	state := value.(*ContainerState)
	if !state.Installed {
		return ""
	}
	return state.AppName
}

// generateFromStoredConfig generates an app package from stored config.
func (m *Monitor) generateFromStoredConfig(ctx context.Context, containerID string, storedCfg *StoredConfig) (*fpkgen.AppConfig, string, error) {
	// Inspect container for runtime info
	info, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to inspect container: %w", err)
	}

	config := appConfigFromStored(storedCfg, containerID, strings.TrimPrefix(info.Name, "/"), info.Config.Image)

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

func appConfigFromStored(storedCfg *StoredConfig, containerID, containerName, image string) *fpkgen.AppConfig {
	config := &fpkgen.AppConfig{
		AppName:       storedCfg.AppName,
		DisplayName:   storedCfg.DisplayName,
		Description:   storedCfg.Description,
		Version:       storedCfg.Version,
		Maintainer:    storedCfg.Maintainer,
		ContainerID:   containerID,
		ContainerName: containerName,
		Image:         image,
		Icon:          storedCfg.IconBase64, // Base64 data from dashboard upload → Base64IconSource
		Entries:       make([]fpkgen.Entry, 0, len(storedCfg.Entries)),
	}

	// Convert entries (dashboard path: icon comes from config level, not entry level)
	for _, e := range storedCfg.Entries {
		config.Entries = append(config.Entries, appEntryFromStored(e, storedCfg.IconBase64))
	}

	if entry := config.GetDefaultEntry(); entry != nil {
		if entry.Title == "" {
			entry.Title = config.DisplayName
		}
		populateLegacyEntryFields(config)
	}
	return config
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
		Icon:          storedCfg.IconBase64,
		Status:        app.StatusRunning,
		Entries:       make([]app.Entry, 0, len(storedCfg.Entries)),
	}
	if value, ok := m.containers.Load(containerID); ok {
		appInstance.Image = value.(*ContainerState).Image
	}

	for _, e := range storedCfg.Entries {
		appInstance.Entries = append(appInstance.Entries, appEntryFromStored(e, storedCfg.IconBase64))
	}
	populateLegacyEntryFields(appInstance)

	m.registry.Register(appInstance)
	slog.Debug("Registered app in registry from stored config", "app", storedCfg.AppName, "entries", len(appInstance.Entries))
}

func appEntryFromStored(entry StoredEntry, defaultIcon string) app.Entry {
	icon := entry.IconBase64
	if icon == "" {
		icon = defaultIcon
	}
	return app.Entry{
		Name:          entry.Name,
		Title:         entry.Title,
		Protocol:      entry.Protocol,
		Port:          entry.Port,
		Path:          entry.Path,
		UIType:        entry.UIType,
		AllUsers:      entry.AllUsers,
		Icon:          icon,
		FileTypes:     append([]string(nil), entry.FileTypes...),
		NoDisplay:     entry.NoDisplay,
		Redirect:      entry.Redirect,
		ForceExternal: entry.ForceExternal,
	}
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
		if err := m.installer.Uninstall(appName); err != nil {
			slog.Error("Failed to uninstall fnOS app after container removal", "app", appName, "error", err)
		}
	}
	m.reconcileStoredContainers()
}

func (m *Monitor) reconcileStoredContainers() {
	if m.configProvider == nil {
		return
	}
	m.containers.Range(func(key, value any) bool {
		state := value.(*ContainerState)
		if state.State != "running" || state.Installed || shouldInstall(state.Labels) {
			return true
		}
		config := m.getStoredConfigForPorts(state.ContainerID, state.Image, state.Ports, state.LegacyPortOptions)
		if config != nil {
			m.queueOperation(&AppOperation{
				Type:          "container_start",
				ContainerID:   state.ContainerID,
				ContainerName: state.ContainerName,
				Labels:        maps.Clone(state.Labels),
				StoredConfig:  config,
			})
		}
		return true
	})
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
		case err, ok := <-errChan:
			if !ok {
				m.reconnectDockerEvents(ctx, "Docker event error channel closed, reconnecting...")
				return
			}
			if err != nil {
				m.reconnectDockerEvents(ctx, "Docker event stream error, reconnecting...", "error", err)
				return
			}
		case event, ok := <-eventChan:
			if !ok {
				m.reconnectDockerEvents(ctx, "Docker event channel closed, reconnecting...")
				return
			}
			m.handleDockerEvent(ctx, event)
		}
	}
}

func (m *Monitor) reconnectDockerEvents(ctx context.Context, msg string, args ...any) {
	slog.Warn(msg, args...)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-m.stopCh:
		return
	case <-timer.C:
		go m.listenToDockerEvents(ctx)
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

		identityPorts, webPorts := extractPortMappings(info.NetworkSettings.Ports)
		legacyPortOptions := legacyPortOptions(info.NetworkSettings.Ports)

		// Publish a complete runtime snapshot while preserving install ownership.
		m.updateContainerState(containerID, func() *ContainerState {
			return &ContainerState{
				ContainerID:   containerID,
				ContainerName: containerName,
			}
		}, func(state *ContainerState) {
			state.ContainerName = containerName
			state.Image = info.Config.Image
			state.State = "running"
			state.Ports = maps.Clone(identityPorts)
			state.WebPorts = maps.Clone(webPorts)
			state.LegacyPortOptions = cloneStringSliceMap(legacyPortOptions)
			state.Labels = maps.Clone(info.Config.Labels)
			state.NetworkMode = string(info.HostConfig.NetworkMode)
		})

		// Check if should install: either has label config or has stored config
		hasLabelConfig := shouldInstall(info.Config.Labels)
		storedConfig := m.getStoredConfigForPorts(containerID, info.Config.Image, identityPorts, legacyPortOptions)

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
		m.updateContainerState(containerID, nil, func(state *ContainerState) {
			state.State = "exited"
		})

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

func extractPortMappings(portMap nat.PortMap) (map[string]string, map[string]string) {
	identityPorts := make(map[string]string)
	webPorts := make(map[string]string)
	ports := sortedPublishedPorts(portMap)
	for _, port := range ports {
		hostPort := preferredHostPort(portMap[port])
		if hostPort == "" {
			continue
		}
		identityPorts[port.Port()+"/"+port.Proto()] = hostPort
		if port.Proto() == "tcp" {
			webPorts[port.Port()] = hostPort
		}
	}
	return identityPorts, webPorts
}

func sortedPublishedPorts(portMap nat.PortMap) []nat.Port {
	ports := make([]nat.Port, 0, len(portMap))
	for port := range portMap {
		ports = append(ports, port)
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Int() != ports[j].Int() {
			return ports[i].Int() < ports[j].Int()
		}
		if ports[i].Proto() == "tcp" && ports[j].Proto() != "tcp" {
			return true
		}
		if ports[j].Proto() == "tcp" && ports[i].Proto() != "tcp" {
			return false
		}
		return ports[i].Proto() < ports[j].Proto()
	})
	return ports
}

func preferredHostPort(bindings []nat.PortBinding) string {
	candidates := sortedPortBindings(bindings)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].HostPort
}

func sortedPortBindings(bindings []nat.PortBinding) []nat.PortBinding {
	candidates := make([]nat.PortBinding, 0, len(bindings))
	for _, binding := range bindings {
		if binding.HostPort != "" {
			candidates = append(candidates, binding)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].HostPort != candidates[j].HostPort {
			return lessPort(candidates[i].HostPort, candidates[j].HostPort)
		}
		return candidates[i].HostIP < candidates[j].HostIP
	})
	return candidates
}

func legacyPortOptions(portMap nat.PortMap) map[string][]string {
	sets := make(map[string]map[string]bool)
	for _, port := range sortedPublishedPorts(portMap) {
		for _, binding := range sortedPortBindings(portMap[port]) {
			containerPort := port.Port()
			if sets[containerPort] == nil {
				sets[containerPort] = make(map[string]bool)
			}
			sets[containerPort][binding.HostPort] = true
		}
	}
	return sortedPortOptions(sets)
}

func sortedPortOptions(sets map[string]map[string]bool) map[string][]string {
	options := make(map[string][]string, len(sets))
	for containerPort, values := range sets {
		for hostPort := range values {
			options[containerPort] = append(options[containerPort], hostPort)
		}
		sort.Slice(options[containerPort], func(i, j int) bool {
			return lessPort(options[containerPort][i], options[containerPort][j])
		})
	}
	return options
}

func extractPorts(portMap nat.PortMap) map[string]string {
	_, webPorts := extractPortMappings(portMap)
	return webPorts
}

func extractSummaryPortMappings(ports []container.Port) (map[string]string, map[string]string) {
	return extractPortMappings(summaryPortMap(ports))
}

func summaryPortMap(ports []container.Port) nat.PortMap {
	portMap := make(nat.PortMap)
	for _, published := range ports {
		if published.PublicPort == 0 {
			continue
		}
		protocol := published.Type
		if protocol == "" {
			protocol = "tcp"
		}
		port := nat.Port(fmt.Sprintf("%d/%s", published.PrivatePort, protocol))
		portMap[port] = append(portMap[port], nat.PortBinding{
			HostIP:   published.IP,
			HostPort: fmt.Sprintf("%d", published.PublicPort),
		})
	}
	return portMap
}

// getAppNameFromLabels extracts appName from labels
func getAppNameFromLabels(labels map[string]string, containerName string) string {
	appName := labels["watchcow.appname"]
	if appName == "" {
		appName = app.DefaultAppName(containerName)
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

		summaryPorts := summaryPortMap(ctr.Ports)
		identityPorts, webPorts := extractPortMappings(summaryPorts)
		legacyPortOptions := legacyPortOptions(summaryPorts)

		// Add to state map
		m.containers.Store(containerID, &ContainerState{
			ContainerID:       containerID,
			ContainerName:     containerName,
			Image:             ctr.Image,
			State:             ctr.State,
			Ports:             identityPorts,
			WebPorts:          webPorts,
			LegacyPortOptions: legacyPortOptions,
			Labels:            maps.Clone(ctr.Labels),
		})
	}

	// Resolve legacy configs only after every current container is visible, so
	// one protocol-less key cannot be claimed by two compatible containers.
	for _, ctr := range containers {
		// Only process running containers
		if ctr.State != "running" {
			continue
		}
		containerID := ctr.ID[:12]
		containerName := strings.TrimPrefix(ctr.Names[0], "/")
		value, ok := m.containers.Load(containerID)
		if !ok {
			continue
		}
		state := value.(*ContainerState)

		// Check if should install: either has label config or has stored config
		hasLabelConfig := shouldInstall(ctr.Labels)
		storedConfig := m.getStoredConfigForPorts(containerID, ctr.Image, state.Ports, state.LegacyPortOptions)

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
	m.stopOnce.Do(func() {
		close(m.stopCh)

		if m.generator != nil {
			m.generator.Close()
		}

		if m.cli != nil {
			if err := m.cli.Close(); err != nil {
				slog.Warn("Error closing Docker client", "error", err)
			}
		}
	})
}

// Registry returns the app registry for external access (e.g., by server)
func (m *Monitor) Registry() *app.Registry {
	return m.registry
}

// registerAppFromConfig creates and registers an App instance from fpkgen.AppConfig
func (m *Monitor) registerAppFromConfig(config *fpkgen.AppConfig, containerID, containerName string) {
	appInstance := cloneAppConfig(config)
	appInstance.ContainerID = containerID
	appInstance.ContainerName = containerName
	appInstance.Status = app.StatusRunning

	m.registry.Register(&appInstance)
	slog.Debug("Registered app in registry", "app", config.AppName, "entries", len(appInstance.Entries))
}

func cloneAppEntries(entries []app.Entry) []app.Entry {
	cloned := append([]app.Entry(nil), entries...)
	for i := range cloned {
		cloned[i].FileTypes = append([]string(nil), entries[i].FileTypes...)
		if entries[i].Control != nil {
			control := *entries[i].Control
			cloned[i].Control = &control
		}
	}
	return cloned
}

func cloneAppConfig(config *fpkgen.AppConfig) app.App {
	cloned := *config
	cloned.Entries = cloneAppEntries(config.Entries)
	cloned.Volumes = append([]app.VolumeMapping(nil), config.Volumes...)
	cloned.Environment = append([]string(nil), config.Environment...)
	cloned.Labels = maps.Clone(config.Labels)
	return cloned
}

func populateLegacyEntryFields(appInstance *app.App) {
	entry := appInstance.GetDefaultEntry()
	if entry == nil {
		return
	}
	appInstance.Protocol = entry.Protocol
	appInstance.Port = entry.Port
	appInstance.Path = entry.Path
	appInstance.UIType = entry.UIType
	appInstance.AllUsers = entry.AllUsers
}

func labelOrDefault(labels map[string]string, key, fallback string) string {
	if value := labels[key]; value != "" {
		return value
	}
	return fallback
}

func (m *Monitor) defaultPortForContainer(containerID string, labels map[string]string) string {
	if port := labels["watchcow.service_port"]; port != "" {
		return port
	}

	value, ok := m.containers.Load(containerID)
	if !ok {
		return ""
	}
	return firstHostPort(webPortsForState(value.(*ContainerState)))
}

func firstHostPort(ports map[string]string) string {
	containerPorts := make([]string, 0, len(ports))
	for containerPort := range ports {
		containerPorts = append(containerPorts, containerPort)
	}
	sort.Slice(containerPorts, func(i, j int) bool {
		return lessPort(containerPorts[i], containerPorts[j])
	})
	if len(containerPorts) == 0 {
		return ""
	}
	return ports[containerPorts[0]]
}

func lessPort(a, b string) bool {
	aPort, aErr := strconv.Atoi(a)
	bPort, bErr := strconv.Atoi(b)
	if aErr == nil && bErr == nil && aPort != bPort {
		return aPort < bPort
	}
	return a < b
}

func webPortsForState(state *ContainerState) map[string]string {
	if state.WebPorts != nil {
		return state.WebPorts
	}
	return state.Ports
}

// registerAppFromLabels creates and registers an App instance from container labels
// Used when app is already installed and we need to reconstruct the app info
func (m *Monitor) registerAppFromLabels(appName, containerID, containerName string, labels map[string]string) {
	defaultPort := m.defaultPortForContainer(containerID, labels)
	image := ""
	if value, ok := m.containers.Load(containerID); ok {
		image = value.(*ContainerState).Image
	}
	displayName := labelOrDefault(labels, "watchcow.display_name", fpkgen.PrettifyName(containerName))
	appInstance := &app.App{
		AppName:       appName,
		Version:       labelOrDefault(labels, "watchcow.version", "1.0.0"),
		DisplayName:   displayName,
		Description:   labelOrDefault(labels, "watchcow.desc", fmt.Sprintf("Docker container: %s", image)),
		Maintainer:    labelOrDefault(labels, "watchcow.maintainer", "WatchCow"),
		ContainerID:   containerID,
		ContainerName: containerName,
		Image:         image,
		Protocol:      labelOrDefault(labels, "watchcow.protocol", "http"),
		Port:          defaultPort,
		Path:          labelOrDefault(labels, "watchcow.path", "/"),
		UIType:        labelOrDefault(labels, "watchcow.ui_type", "url"),
		AllUsers:      labelOrDefault(labels, "watchcow.all_users", "true") == "true",
		Icon:          labels["watchcow.icon"],
		Labels:        maps.Clone(labels),
		Status:        app.StatusRunning,
		Entries:       make([]app.Entry, 0),
	}

	// Parse entries from labels using fpkgen's ParseEntries
	defaultIcon := labels["watchcow.icon"]
	appInstance.Entries = cloneAppEntries(fpkgen.ParseEntries(labels, appInstance.DisplayName, defaultIcon, defaultPort))
	populateLegacyEntryFields(appInstance)

	m.registry.Register(appInstance)
	slog.Debug("Registered app in registry from labels", "app", appName, "entries", len(appInstance.Entries))
}

// ContainerInfo represents container information for the dashboard.
type ContainerInfo struct {
	ID                string
	Name              string
	Image             string
	State             string
	Ports             map[string]string   // TCP containerPort -> hostPort
	IdentityPorts     map[string]string   // Protocol-qualified published ports used for new storage keys
	LegacyPortOptions map[string][]string // All host bindings accepted when matching old protocol-less keys
	Labels            map[string]string
	NetworkMode       string
}

// ListAllContainers returns all containers from the internal state map.
func (m *Monitor) ListAllContainers(ctx context.Context) ([]ContainerInfo, error) {
	var result []ContainerInfo
	m.containers.Range(func(key, value any) bool {
		state := value.(*ContainerState)
		result = append(result, ContainerInfo{
			ID:                state.ContainerID,
			Name:              state.ContainerName,
			Image:             state.Image,
			State:             state.State,
			Ports:             maps.Clone(webPortsForState(state)),
			IdentityPorts:     maps.Clone(state.Ports),
			LegacyPortOptions: cloneStringSliceMap(state.LegacyPortOptions),
			Labels:            maps.Clone(state.Labels),
			NetworkMode:       state.NetworkMode,
		})
		return true
	})
	return result, nil
}

// TriggerUninstall queues an app uninstall operation (called from dashboard when config is deleted).
func (m *Monitor) TriggerUninstall(containerID, appName, configKey string) {
	if appName == "" {
		return
	}

	slog.Info("Queueing app uninstall from dashboard", "app", appName)

	m.queueOperation(&AppOperation{
		Type:        "dashboard_uninstall",
		ContainerID: containerID,
		AppName:     appName,
		ConfigKey:   configKey,
	})
}

// processDashboardUninstall handles uninstall triggered from dashboard.
func (m *Monitor) processDashboardUninstall(op *AppOperation) {
	appName := op.AppName
	if appName == "" {
		return
	}
	if op.ConfigKey != "" && m.configProvider != nil && m.configProvider.GetByKey(op.ConfigKey) != nil {
		slog.Info("Skipping stale dashboard uninstall because a new config exists", "app", appName, "key", op.ConfigKey)
		return
	}

	slog.Info("Processing dashboard uninstall", "app", appName)

	// Uninstall from fnOS
	if m.installer != nil {
		if err := m.installer.Uninstall(appName); err != nil {
			slog.Error("Dashboard uninstall failed", "app", appName, "error", err)
			m.recordFailedUninstallStatus(appName, err)
			return
		}
	}

	// Unregister only after the system package is actually gone.
	m.registry.Unregister(appName)

	// Clear installed state for any container with this app name
	m.containers.Range(func(key, value any) bool {
		state := value.(*ContainerState)
		if state.AppName == appName {
			m.updateContainerState(key.(string), nil, func(current *ContainerState) {
				if current.AppName == appName {
					current.AppName = ""
					current.Installed = false
				}
			})
		}
		return true
	})

	slog.Info("Dashboard uninstall completed", "app", appName)
}

// processDashboardReinstall handles config update: uninstall old app, then install with new config.
func (m *Monitor) processDashboardReinstall(ctx context.Context, op *AppOperation) {
	if op.StoredConfig != nil && !m.isCurrentPendingConfig(op.StoredConfig) {
		return
	}
	oldAppName := m.installedAppName(op.ContainerID)
	if oldAppName == "" && m.installer != nil && op.StoredConfig != nil && m.installer.IsAppInstalled(op.StoredConfig.AppName) {
		oldAppName = op.StoredConfig.AppName
	}
	if oldAppName == "" {
		m.processContainerStartWithMode(ctx, op, true)
		return
	}

	// Step 1: Uninstall the old app
	slog.Info("Uninstalling old app for reinstall", "app", oldAppName)
	if oldAppName != "" && m.installer != nil {
		if err := m.installer.Uninstall(oldAppName); err != nil {
			slog.Error("Cannot reinstall because uninstall failed", "app", oldAppName, "error", err)
			m.recordFailedUninstallStatus(oldAppName, err)
			return
		}
	}
	m.registry.Unregister(oldAppName)

	// Clear installed state
	m.updateContainerState(op.ContainerID, nil, func(state *ContainerState) {
		state.AppName = ""
		state.Installed = false
	})
	if op.StoredConfig != nil && !m.isCurrentPendingConfig(op.StoredConfig) {
		slog.Info("Skipping stale dashboard config after uninstall", "app", op.StoredConfig.AppName, "revision", op.StoredConfig.Revision)
		return
	}

	// Step 2: Always regenerate and install the new config. The ordinary
	// container-start path intentionally reuses an existing installed app.
	slog.Info("Installing app with new config", "container", op.ContainerName)
	m.processContainerStartWithMode(ctx, op, true)
}

func (m *Monitor) recordFailedUninstallStatus(appName string, err error) {
	var restoreErr *fpkgen.RestoreStoppedAppError
	if errors.As(err, &restoreErr) {
		m.registry.UpdateStatus(appName, app.StatusStopped)
	}
}
