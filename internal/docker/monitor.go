package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// GetNamedCandidates returns configs owned by the same stable container name.
	GetNamedCandidates(containerName string) []StoredConfigMatch
	// GetDeletingConfigs returns durable dashboard uninstall intents.
	GetDeletingConfigs() []StoredConfigMatch
	// GetAllConfigs returns dashboard configs for applied-owner reconciliation.
	GetAllConfigs() []StoredConfigMatch
	// MarkApplied clears Pending only if the stored revision still matches.
	MarkApplied(key, revision string) error
	// MarkFailed records an asynchronous apply/delete failure.
	MarkFailed(key, revision, message string) error
	// CompleteDelete removes deleting configs after the fnOS package is gone.
	CompleteDelete(appName string) error
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
	Deleting    bool
	LastError   string
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
	Type            string // "install", "start", "stop", "destroy", "container_start"
	AppName         string
	AppDir          string
	ContainerID     string
	ContainerName   string
	Labels          map[string]string
	StoredConfig    *StoredConfig // Config from dashboard storage (if no labels)
	ConfigKey       string        // Canonical storage key for stale uninstall detection
	LabelRevision   string        // Desired label config captured when the operation was queued
	PreviousAppName string        // Last applied app from the other configuration source
	ForceReconcile  bool          // Rebuild even when dashboard content itself is not pending
	ResultCh        chan error
}

// Monitor watches Docker containers and manages fnOS app installation
type Monitor struct {
	cli             *client.Client
	generator       *fpkgen.Generator
	installer       appInstaller
	configProvider  ConfigProvider
	stopCh          chan struct{}
	stopOnce        sync.Once
	inventoryScanMu sync.Mutex

	// Track all container states
	containers sync.Map // map[containerID]*ContainerState

	// App registry for runtime app info lookup
	registry *app.Registry

	// Operation queue for serializing all state changes and appcenter-cli calls
	opQueue chan *AppOperation

	labelRevisionsMu   sync.Mutex
	labelRevisions     map[string]string // appName -> last successfully installed label revision
	labelOwners        map[string]string // containerName -> last successfully installed label app
	pendingUninstalls  map[string]bool   // appName -> durable orphan cleanup intent
	labelRevisionsPath string
	inventoryReady     atomic.Bool // true after Docker returned one authoritative container list
}

const labelRevisionsFilename = "label-revisions.json"

type persistedMonitorState struct {
	LabelRevisions    map[string]string `json:"label_revisions"`
	LabelOwners       map[string]string `json:"label_owners"`
	PendingUninstalls map[string]bool   `json:"pending_uninstalls"`
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

	labelRevisionsPath := defaultLabelRevisionsPath()
	persistedState, err := loadPersistedMonitorState(labelRevisionsPath)
	if err != nil {
		slog.Warn("Failed to load applied label revisions; installed label apps will be reconciled", "path", labelRevisionsPath, "error", err)
		persistedState = newPersistedMonitorState()
	}

	return &Monitor{
		cli:       cli,
		generator: generator,
		installer: installer,
		stopCh:    make(chan struct{}),
		registry:  app.NewRegistry(),
		opQueue:   make(chan *AppOperation, 100),

		labelRevisions:     persistedState.LabelRevisions,
		labelOwners:        persistedState.LabelOwners,
		pendingUninstalls:  persistedState.PendingUninstalls,
		labelRevisionsPath: labelRevisionsPath,
	}, nil
}

func defaultLabelRevisionsPath() string {
	if pkgVar := os.Getenv("TRIM_PKGVAR"); pkgVar != "" {
		return filepath.Join(pkgVar, labelRevisionsFilename)
	}
	if pkgEtc := os.Getenv("TRIM_PKGETC"); pkgEtc != "" {
		return filepath.Join(pkgEtc, labelRevisionsFilename)
	}
	return filepath.Join("/tmp", "watchcow", labelRevisionsFilename)
}

func newPersistedMonitorState() persistedMonitorState {
	return persistedMonitorState{
		LabelRevisions:    make(map[string]string),
		LabelOwners:       make(map[string]string),
		PendingUninstalls: make(map[string]bool),
	}
}

func loadPersistedMonitorState(path string) (persistedMonitorState, error) {
	state := newPersistedMonitorState()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	var persisted persistedMonitorState
	if err := json.Unmarshal(data, &persisted); err == nil &&
		(persisted.LabelRevisions != nil || persisted.LabelOwners != nil || persisted.PendingUninstalls != nil) {
		if persisted.LabelRevisions != nil {
			state.LabelRevisions = persisted.LabelRevisions
		}
		if persisted.LabelOwners != nil {
			state.LabelOwners = persisted.LabelOwners
		}
		if persisted.PendingUninstalls != nil {
			state.PendingUninstalls = persisted.PendingUninstalls
		}
		return state, nil
	}

	// Compatibility with the first revision-only file format.
	if err := json.Unmarshal(data, &state.LabelRevisions); err != nil {
		return newPersistedMonitorState(), err
	}
	return state, nil
}

func loadLabelRevisions(path string) (map[string]string, error) {
	state, err := loadPersistedMonitorState(path)
	return state.LabelRevisions, err
}

func (m *Monitor) saveLabelRevisionsLocked() error {
	if m.labelRevisionsPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.labelRevisionsPath), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(persistedMonitorState{
		LabelRevisions:    m.labelRevisions,
		LabelOwners:       m.labelOwners,
		PendingUninstalls: m.pendingUninstalls,
	})
	if err != nil {
		return err
	}
	tmpPath := m.labelRevisionsPath + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, m.labelRevisionsPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func (m *Monitor) isLabelRevisionApplied(appName, revision string) bool {
	if appName == "" || revision == "" {
		return false
	}
	m.labelRevisionsMu.Lock()
	defer m.labelRevisionsMu.Unlock()
	return m.labelRevisions[appName] == revision
}

func (m *Monitor) markLabelRevisionApplied(appName, revision string) error {
	return m.markLabelRevisionAppliedForContainer(appName, revision, "")
}

func (m *Monitor) markLabelRevisionAppliedForContainer(appName, revision, containerName string) error {
	if appName == "" || revision == "" {
		return nil
	}
	m.labelRevisionsMu.Lock()
	defer m.labelRevisionsMu.Unlock()
	m.ensurePersistedStateLocked()
	previousRevisions := maps.Clone(m.labelRevisions)
	previousOwners := maps.Clone(m.labelOwners)
	previousUninstalls := maps.Clone(m.pendingUninstalls)
	m.labelRevisions[appName] = revision
	if containerName != "" {
		m.labelOwners[strings.TrimPrefix(containerName, "/")] = appName
	}
	delete(m.pendingUninstalls, appName)
	if err := m.saveLabelRevisionsLocked(); err != nil {
		m.labelRevisions = previousRevisions
		m.labelOwners = previousOwners
		m.pendingUninstalls = previousUninstalls
		return err
	}
	return nil
}

func (m *Monitor) clearLabelRevision(appName string) error {
	if appName == "" {
		return nil
	}
	m.labelRevisionsMu.Lock()
	defer m.labelRevisionsMu.Unlock()
	m.ensurePersistedStateLocked()
	found := false
	if _, ok := m.labelRevisions[appName]; ok {
		found = true
	}
	for _, ownedApp := range m.labelOwners {
		if ownedApp == appName {
			found = true
			break
		}
	}
	if !found && !m.pendingUninstalls[appName] {
		return nil
	}
	previousRevisions := maps.Clone(m.labelRevisions)
	previousOwners := maps.Clone(m.labelOwners)
	previousUninstalls := maps.Clone(m.pendingUninstalls)
	delete(m.labelRevisions, appName)
	delete(m.pendingUninstalls, appName)
	for containerName, ownedApp := range m.labelOwners {
		if ownedApp == appName {
			delete(m.labelOwners, containerName)
		}
	}
	if err := m.saveLabelRevisionsLocked(); err != nil {
		m.labelRevisions = previousRevisions
		m.labelOwners = previousOwners
		m.pendingUninstalls = previousUninstalls
		return err
	}
	return nil
}

func (m *Monitor) ensurePersistedStateLocked() {
	if m.labelRevisions == nil {
		m.labelRevisions = make(map[string]string)
	}
	if m.labelOwners == nil {
		m.labelOwners = make(map[string]string)
	}
	if m.pendingUninstalls == nil {
		m.pendingUninstalls = make(map[string]bool)
	}
}

func (m *Monitor) hasLabelRevision(appName string) bool {
	if appName == "" {
		return false
	}
	m.labelRevisionsMu.Lock()
	defer m.labelRevisionsMu.Unlock()
	return m.labelRevisions[appName] != ""
}

func (m *Monitor) appliedLabelAppForContainer(containerName string) string {
	m.labelRevisionsMu.Lock()
	defer m.labelRevisionsMu.Unlock()
	return m.labelOwners[strings.TrimPrefix(containerName, "/")]
}

func (m *Monitor) markPendingUninstall(appName string) error {
	if appName == "" {
		return nil
	}
	m.labelRevisionsMu.Lock()
	defer m.labelRevisionsMu.Unlock()
	m.ensurePersistedStateLocked()
	if m.pendingUninstalls[appName] {
		return nil
	}
	previous := maps.Clone(m.pendingUninstalls)
	m.pendingUninstalls[appName] = true
	if err := m.saveLabelRevisionsLocked(); err != nil {
		m.pendingUninstalls = previous
		return err
	}
	return nil
}

func (m *Monitor) pendingUninstallApps() []string {
	m.labelRevisionsMu.Lock()
	defer m.labelRevisionsMu.Unlock()
	apps := make([]string, 0, len(m.pendingUninstalls))
	for appName := range m.pendingUninstalls {
		apps = append(apps, appName)
	}
	sort.Strings(apps)
	return apps
}

func (m *Monitor) isPendingUninstall(appName string) bool {
	m.labelRevisionsMu.Lock()
	defer m.labelRevisionsMu.Unlock()
	return m.pendingUninstalls[appName]
}

func (m *Monitor) labelOwnerSnapshot() map[string]string {
	m.labelRevisionsMu.Lock()
	defer m.labelRevisionsMu.Unlock()
	return maps.Clone(m.labelOwners)
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
		m.recordDashboardFailure(storedConfig, fmt.Errorf("container not found"))
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
		if !m.queueOperation(&AppOperation{
			Type:          "dashboard_reinstall",
			ContainerID:   containerID,
			ContainerName: state.ContainerName,
			AppName:       state.AppName,
			Labels:        maps.Clone(state.Labels),
			StoredConfig:  storedConfig,
		}) {
			m.recordDashboardFailure(storedConfig, fmt.Errorf("operation queue full"))
		}
		return
	}

	slog.Info("Triggering app install from dashboard", "container", state.ContainerName)
	if !m.queueOperation(&AppOperation{
		Type:          "dashboard_install",
		ContainerID:   containerID,
		ContainerName: state.ContainerName,
		Labels:        maps.Clone(state.Labels),
		StoredConfig:  storedConfig,
	}) {
		m.recordDashboardFailure(storedConfig, fmt.Errorf("operation queue full"))
	}
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
	if containerName != "" {
		namedMatches := m.configProvider.GetNamedCandidates(containerName)
		if len(namedMatches) == 1 {
			config, err := m.configProvider.MigrateLegacy(namedMatches[0].Key, currentKey)
			if err != nil {
				slog.Error("Failed to migrate named dashboard key", "old_key", namedMatches[0].Key, "new_key", currentKey, "error", err)
				return nil
			}
			return config
		}
		if len(namedMatches) > 1 {
			return nil
		}
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

type labelConfigSnapshot struct {
	ContainerName string            `json:"container_name"`
	Image         string            `json:"image"`
	NetworkMode   string            `json:"network_mode"`
	WebPorts      map[string]string `json:"web_ports"`
	Labels        map[string]string `json:"labels"`
}

func labelConfigRevision(containerName, image, networkMode string, webPorts, labels map[string]string) string {
	watchcowLabels := make(map[string]string)
	for key, value := range labels {
		if strings.HasPrefix(key, "watchcow.") {
			watchcowLabels[key] = value
		}
	}
	payload, err := json.Marshal(labelConfigSnapshot{
		ContainerName: strings.TrimPrefix(containerName, "/"),
		Image:         image,
		NetworkMode:   networkMode,
		WebPorts:      webPorts,
		Labels:        watchcowLabels,
	})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:16])
}

func labelRevisionForState(state *ContainerState) string {
	if state == nil || !shouldInstall(state.Labels) {
		return ""
	}
	return labelConfigRevision(state.ContainerName, state.Image, state.NetworkMode, webPortsForState(state), state.Labels)
}

func (m *Monitor) isCurrentLabelRevision(containerID, revision string) bool {
	if revision == "" {
		return true
	}
	value, ok := m.containers.Load(containerID)
	if !ok {
		return false
	}
	return labelRevisionForState(value.(*ContainerState)) == revision
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

			case "orphan_uninstall":
				m.processOrphanUninstall(op)
			}
		}
	}
}

// processContainerStart handles container start - check if installed, start or generate
func (m *Monitor) processContainerStart(ctx context.Context, op *AppOperation) {
	m.processContainerStartWithMode(ctx, op, false)
}

func (m *Monitor) processContainerStartWithMode(ctx context.Context, op *AppOperation, forceInstall bool) {
	if op.StoredConfig != nil && op.StoredConfig.Deleting {
		slog.Info("Skipping apply for dashboard config pending deletion", "app", op.StoredConfig.AppName)
		return
	}
	if op.StoredConfig != nil && op.StoredConfig.LastError != "" && !op.StoredConfig.Pending {
		slog.Info("Skipping dashboard config that requires user input", "app", op.StoredConfig.AppName, "error", op.StoredConfig.LastError)
		return
	}
	if op.StoredConfig != nil && op.StoredConfig.Pending && !m.isCurrentPendingConfig(op.StoredConfig) {
		slog.Info("Skipping stale dashboard config operation", "app", op.StoredConfig.AppName, "revision", op.StoredConfig.Revision)
		return
	}
	if op.StoredConfig != nil && !op.StoredConfig.Pending && !m.isCurrentStoredConfig(op.StoredConfig) {
		slog.Info("Skipping stale dashboard config operation", "app", op.StoredConfig.AppName, "revision", op.StoredConfig.Revision)
		return
	}
	if op.StoredConfig == nil && op.LabelRevision != "" && !m.isCurrentLabelRevision(op.ContainerID, op.LabelRevision) {
		slog.Info("Skipping stale label config operation", "container", op.ContainerName, "revision", op.LabelRevision)
		return
	}

	// Determine app name based on config source
	var appName string
	if op.StoredConfig != nil {
		appName = op.StoredConfig.AppName
	} else {
		appName = getAppNameFromLabels(op.Labels, op.ContainerName)
		if m.labelAppHasMultipleOwners(op.ContainerID, appName) {
			slog.Error("Skipping label app with conflicting owners; set a unique watchcow.appname", "app", appName, "container", op.ContainerName)
			return
		}
	}
	if !forceInstall {
		previousAppName := op.PreviousAppName
		if previousAppName == "" && op.StoredConfig != nil {
			previousAppName = m.appliedLabelAppForContainer(op.ContainerName)
		}
		if m.installer != nil && previousAppName != "" && previousAppName != appName && m.installer.IsAppInstalled(previousAppName) {
			op.PreviousAppName = previousAppName
			op.ForceReconcile = true
			m.processDashboardReinstall(ctx, op)
			return
		}
		if op.StoredConfig != nil && m.hasLabelRevision(appName) {
			op.PreviousAppName = appName
			op.ForceReconcile = true
			m.processDashboardReinstall(ctx, op)
			return
		}
	}

	// Check if already installed in fnOS
	if !forceInstall && m.installer != nil && m.installer.IsAppInstalled(appName) {
		if op.StoredConfig != nil && op.StoredConfig.Pending {
			op.AppName = appName
			m.processDashboardReinstall(ctx, op)
			return
		}
		if op.StoredConfig == nil && op.LabelRevision != "" && !m.isLabelRevisionApplied(appName, op.LabelRevision) {
			op.AppName = appName
			m.processDashboardReinstall(ctx, op)
			return
		}
		// Already installed, transfer ownership to this new container.
		// Clear the app association from any previous container so that when the
		// old container is later destroyed it does not uninstall the live app.
		m.clearAppOwnership(appName, op.ContainerID)
		slog.Info("App already installed, starting", "app", appName)
		m.updateContainerState(op.ContainerID, nil, func(state *ContainerState) {
			state.AppName = appName
			state.Installed = true
		})
		// Register app in registry
		if op.StoredConfig != nil {
			m.registerAppFromStoredConfig(op.StoredConfig, op.ContainerID, op.ContainerName)
			if err := m.clearLabelRevision(appName); err != nil {
				slog.Error("Failed to clear label revision after dashboard ownership transfer", "app", appName, "error", err)
			}
		} else {
			m.registerAppFromLabels(appName, op.ContainerID, op.ContainerName, op.Labels)
			if op.LabelRevision != "" {
				if err := m.markLabelRevisionAppliedForContainer(appName, op.LabelRevision, op.ContainerName); err != nil {
					slog.Error("Failed to persist reused label ownership", "app", appName, "error", err)
				}
			}
		}
		if m.installer != nil {
			m.registry.UpdateStatus(appName, app.StatusInstalled)
			if err := m.installer.StartApp(appName); err != nil {
				slog.Error("Failed to start fnOS app", "app", appName, "error", err)
				m.registry.UpdateStatus(appName, app.StatusStopped)
			} else {
				m.registry.UpdateStatus(appName, app.StatusRunning)
			}
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
		m.recordDashboardFailure(op.StoredConfig, err)
		return
	}
	if op.StoredConfig != nil {
		current := m.isCurrentStoredConfig(op.StoredConfig)
		if op.StoredConfig.Pending {
			current = m.isCurrentPendingConfig(op.StoredConfig)
		}
		if !current {
			slog.Info("Discarding package generated for stale dashboard config", "app", config.AppName, "revision", op.StoredConfig.Revision)
			os.RemoveAll(appDir)
			return
		}
	}
	if op.StoredConfig == nil && op.LabelRevision != "" && !m.isCurrentLabelRevision(op.ContainerID, op.LabelRevision) {
		slog.Info("Discarding package generated for stale label config", "app", config.AppName, "revision", op.LabelRevision)
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
			m.recordDashboardFailure(op.StoredConfig, err)
		} else {
			m.clearAppOwnership(config.AppName, op.ContainerID)
			m.updateContainerState(op.ContainerID, nil, func(state *ContainerState) {
				state.Installed = true
				state.AppName = config.AppName
			})
			// Register app in registry
			m.registerAppFromConfig(config, op.ContainerID, op.ContainerName)
			if op.StoredConfig != nil {
				if m.configProvider != nil {
					if err := m.configProvider.MarkApplied(op.StoredConfig.Key, op.StoredConfig.Revision); err != nil {
						slog.Error("Failed to mark dashboard config as applied", "app", config.AppName, "error", err)
					}
				}
				if err := m.clearLabelRevision(config.AppName); err != nil {
					slog.Error("Failed to clear label revision after dashboard install", "app", config.AppName, "error", err)
				}
			} else if op.LabelRevision != "" {
				if err := m.markLabelRevisionAppliedForContainer(config.AppName, op.LabelRevision, op.ContainerName); err != nil {
					slog.Error("Failed to persist applied label revision", "app", config.AppName, "error", err)
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
	if config == nil || config.Key == "" || m.configProvider == nil {
		return true
	}
	current := m.configProvider.GetByKey(config.Key)
	return current != nil && current.Pending && !current.Deleting && current.Revision == config.Revision
}

func (m *Monitor) isCurrentStoredConfig(config *StoredConfig) bool {
	if config == nil || config.Key == "" || m.configProvider == nil {
		return true
	}
	current := m.configProvider.GetByKey(config.Key)
	if current == nil || current.Deleting {
		return false
	}
	return current.Revision == config.Revision
}

func (m *Monitor) recordDashboardFailure(config *StoredConfig, err error) {
	if config == nil || err == nil || m.configProvider == nil {
		return
	}
	current := m.configProvider.GetByKey(config.Key)
	if current == nil || current.Deleting || current.Revision != config.Revision {
		return
	}
	if markErr := m.configProvider.MarkFailed(config.Key, config.Revision, err.Error()); markErr != nil {
		slog.Error("Failed to persist dashboard operation error", "app", config.AppName, "error", markErr)
	}
}

func (m *Monitor) recordDashboardDeleteFailure(configKey, message string) {
	if configKey == "" || message == "" || m.configProvider == nil {
		return
	}
	current := m.configProvider.GetByKey(configKey)
	if current == nil || !current.Deleting {
		return
	}
	if err := m.configProvider.MarkFailed(configKey, current.Revision, message); err != nil {
		slog.Error("Failed to persist dashboard delete error", "app", current.AppName, "error", err)
	}
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

func (m *Monitor) clearAppOwnership(appName, exceptContainerID string) {
	if appName == "" {
		return
	}
	m.containers.Range(func(key, value any) bool {
		containerID := key.(string)
		if containerID == exceptContainerID {
			return true
		}
		state := value.(*ContainerState)
		if state.AppName == appName {
			m.updateContainerState(containerID, nil, func(current *ContainerState) {
				if current.AppName == appName {
					current.AppName = ""
					current.Installed = false
				}
			})
		}
		return true
	})
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
	image := ""
	if value, ok := m.containers.Load(containerID); ok {
		image = value.(*ContainerState).Image
	}
	config := appConfigFromStored(storedCfg, containerID, containerName, image)
	appInstance := cloneAppConfig(config)
	appInstance.Status = app.StatusRunning

	m.registry.Register(&appInstance)
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
		if err := m.installer.StopApp(state.AppName); err != nil {
			slog.Error("Failed to stop fnOS app", "app", state.AppName, "error", err)
			return
		}
		m.registry.UpdateStatus(state.AppName, app.StatusStopped)
	}
}

// processDestroy handles destroy operation
func (m *Monitor) processDestroy(op *AppOperation) {
	m.inventoryScanMu.Lock()
	defer m.inventoryScanMu.Unlock()

	v, exists := m.containers.Load(op.ContainerID)
	if !exists {
		slog.Debug("Container not tracked, skipping destroy", "id", op.ContainerID)
		return
	}
	state := v.(*ContainerState)

	appName := state.AppName
	wasInstalled := state.Installed
	if wasInstalled {
		if err := m.markPendingUninstall(appName); err != nil {
			slog.Error("Failed to persist uninstall intent after container removal", "app", appName, "error", err)
		}
	}

	// Remove from tracking
	m.containers.Delete(op.ContainerID)

	// Unregister from app registry
	m.registry.Unregister(appName)
	slog.Debug("Unregistered app from registry", "app", appName)

	// Uninstall if was installed
	if wasInstalled {
		if m.installer != nil {
			slog.Info("Uninstalling fnOS app", "app", appName)
			if err := m.installer.Uninstall(appName); err != nil {
				slog.Error("Failed to uninstall fnOS app after container removal", "app", appName, "error", err)
			} else if err := m.clearLabelRevision(appName); err != nil {
				slog.Error("Failed to clear label revision after uninstall", "app", appName, "error", err)
			}
		} else if err := m.clearLabelRevision(appName); err != nil {
			slog.Error("Failed to clear uninstall intent without an installer", "app", appName, "error", err)
		}
	}
	m.reconcileContainerLifecycle()
	m.reconcileRemovalIntents()
}

func (m *Monitor) reconcileContainerLifecycle() {
	m.containers.Range(func(key, value any) bool {
		state := value.(*ContainerState)
		switch state.State {
		case "destroyed":
			m.queueOperation(&AppOperation{Type: "destroy", ContainerID: state.ContainerID})
			return true
		case "exited":
			if state.Installed {
				registered := m.registry.Get(state.AppName)
				if registered == nil || registered.Status != app.StatusStopped {
					m.queueOperation(&AppOperation{Type: "stop", ContainerID: state.ContainerID})
				}
			}
			return true
		case "running":
		default:
			return true
		}

		if shouldInstall(state.Labels) {
			appName := getAppNameFromLabels(state.Labels, state.ContainerName)
			revision := labelRevisionForState(state)
			registered := m.registry.Get(appName)
			if !state.Installed || state.AppName != appName || !m.isLabelRevisionApplied(appName, revision) || registered == nil || registered.Status != app.StatusRunning {
				storedConfig := m.getStoredConfigForPorts(state.ContainerID, state.Image, state.Ports, state.LegacyPortOptions)
				previousAppName := ""
				if storedConfig != nil {
					previousAppName = storedConfig.AppName
				}
				m.queueOperation(&AppOperation{
					Type: "container_start", ContainerID: state.ContainerID, ContainerName: state.ContainerName,
					Labels: maps.Clone(state.Labels), LabelRevision: revision, PreviousAppName: previousAppName,
				})
			}
			return true
		}
		if m.configProvider == nil {
			return true
		}
		config := m.getStoredConfigForPorts(state.ContainerID, state.Image, state.Ports, state.LegacyPortOptions)
		registered := (*app.App)(nil)
		if config != nil {
			registered = m.registry.Get(config.AppName)
		}
		if config != nil && (config.Deleting || config.Pending || !state.Installed || state.AppName != config.AppName || m.appliedLabelAppForContainer(state.ContainerName) != "" || registered == nil || registered.Status != app.StatusRunning) {
			m.queueStoredConfigOperation(state.ContainerID, state.ContainerName, state.Labels, config)
		}
		return true
	})
}

func (m *Monitor) labelAppHasMultipleOwners(containerID, appName string) bool {
	if appName == "" {
		return false
	}
	owners := 0
	m.containers.Range(func(key, value any) bool {
		state := value.(*ContainerState)
		if state.State != "running" || !shouldInstall(state.Labels) || getAppNameFromLabels(state.Labels, state.ContainerName) != appName {
			return true
		}
		owners++
		return owners < 2
	})
	return owners > 1
}

func (m *Monitor) currentDesiredApps() (map[string]bool, map[string]string) {
	apps := make(map[string]bool)
	byContainer := make(map[string]string)
	m.containers.Range(func(_, value any) bool {
		state := value.(*ContainerState)
		if state.State == "destroyed" {
			return true
		}
		appName := ""
		if shouldInstall(state.Labels) {
			appName = getAppNameFromLabels(state.Labels, state.ContainerName)
		} else if m.configProvider != nil {
			config := m.getStoredConfigForPorts(state.ContainerID, state.Image, state.Ports, state.LegacyPortOptions)
			if config != nil && !config.Deleting {
				appName = config.AppName
			}
		}
		if appName != "" {
			apps[appName] = true
			byContainer[strings.TrimPrefix(state.ContainerName, "/")] = appName
		}
		return true
	})
	return apps, byContainer
}

func (m *Monitor) reconcileRemovalIntents() {
	// Absence is meaningful only after Docker has returned a complete list.
	// A transient daemon/socket failure must never be interpreted as removing
	// every managed container.
	if !m.inventoryReady.Load() {
		return
	}
	queued := make(map[string]bool)
	if m.configProvider != nil {
		for _, match := range m.configProvider.GetDeletingConfigs() {
			if match.Config == nil || match.Config.AppName == "" || queued[match.Config.AppName] {
				continue
			}
			queued[match.Config.AppName] = true
			m.queueOperation(&AppOperation{
				Type: "dashboard_uninstall", AppName: match.Config.AppName, ConfigKey: match.Key,
			})
		}
	}

	desiredApps, desiredByContainer := m.currentDesiredApps()
	if m.configProvider != nil {
		for _, match := range m.configProvider.GetAllConfigs() {
			config := match.Config
			if config == nil || config.Deleting || config.AppName == "" || desiredApps[config.AppName] {
				continue
			}
			if !m.isPendingUninstall(config.AppName) && (m.installer == nil || !m.installer.IsAppInstalled(config.AppName)) {
				continue
			}
			if err := m.markPendingUninstall(config.AppName); err != nil {
				slog.Error("Failed to persist orphaned dashboard app cleanup", "app", config.AppName, "error", err)
			}
		}
	}
	for containerName, appName := range m.labelOwnerSnapshot() {
		if desiredByContainer[containerName] == appName {
			continue
		}
		if err := m.markPendingUninstall(appName); err != nil {
			slog.Error("Failed to persist orphaned label app cleanup", "app", appName, "error", err)
		}
	}
	for _, appName := range m.pendingUninstallApps() {
		if desiredApps[appName] || queued[appName] {
			continue
		}
		queued[appName] = true
		m.queueOperation(&AppOperation{Type: "orphan_uninstall", AppName: appName})
	}
}

func (m *Monitor) runReconciler(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			if !m.inventoryReady.Load() {
				m.scanContainers(ctx)
			}
			m.reconcileContainerLifecycle()
			m.reconcileRemovalIntents()
		}
	}
}

func (m *Monitor) queueStoredConfigOperation(containerID, containerName string, labels map[string]string, config *StoredConfig) {
	if config == nil {
		return
	}
	if config.LastError != "" && !config.Pending && !config.Deleting {
		slog.Info("Waiting for dashboard input before applying blocked config", "app", config.AppName, "error", config.LastError)
		return
	}
	op := &AppOperation{
		Type:            "container_start",
		ContainerID:     containerID,
		ContainerName:   containerName,
		Labels:          maps.Clone(labels),
		StoredConfig:    config,
		PreviousAppName: m.appliedLabelAppForContainer(containerName),
	}
	if config.Deleting {
		op.Type = "dashboard_uninstall"
		op.AppName = config.AppName
		op.ConfigKey = config.Key
	}
	if m.queueOperation(op) {
		return
	}
	if config.Deleting && m.configProvider != nil {
		m.recordDashboardDeleteFailure(config.Key, "操作队列已满，请重试")
		return
	}
	m.recordDashboardFailure(config, fmt.Errorf("operation queue full"))
}

// queueOperation sends an operation to the worker (fire and forget, no wait).
// It returns false when the bounded queue cannot accept the operation.
func (m *Monitor) queueOperation(op *AppOperation) bool {
	select {
	case m.opQueue <- op:
		return true
	default:
		slog.Warn("Operation queue full, dropping operation", "type", op.Type, "app", op.AppName)
		return false
	}
}

// Start starts monitoring Docker containers
func (m *Monitor) Start(ctx context.Context) {
	slog.Info("Starting Docker monitor...")

	// Start operation worker for serializing all state changes
	go m.runOperationWorker(ctx)

	// Initial scan to process existing containers
	m.scanContainers(ctx)
	m.reconcileRemovalIntents()
	go m.runReconciler(ctx)

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
	// The subscription is active before this catch-up scan, so events that race
	// with the scan remain queued and are applied afterward. This closes gaps
	// between the initial scan and subscription, and after reconnects.
	if m.scanContainers(ctx) {
		m.reconcileRemovalIntents()
	}

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
	m.inventoryScanMu.Lock()
	defer m.inventoryScanMu.Unlock()

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
		networkMode := string(info.HostConfig.NetworkMode)

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
			state.NetworkMode = networkMode
		})

		// Check if should install: either has label config or has stored config
		hasLabelConfig := shouldInstall(info.Config.Labels)
		storedConfig := m.getStoredConfigForPorts(containerID, info.Config.Image, identityPorts, legacyPortOptions)

		if hasLabelConfig {
			previousAppName := ""
			if storedConfig != nil {
				previousAppName = storedConfig.AppName
			}
			m.queueOperation(&AppOperation{
				Type:            "container_start",
				ContainerID:     containerID,
				ContainerName:   containerName,
				Labels:          info.Config.Labels,
				LabelRevision:   labelConfigRevision(containerName, info.Config.Image, networkMode, webPorts, info.Config.Labels),
				PreviousAppName: previousAppName,
			})
		} else if storedConfig != nil {
			m.queueStoredConfigOperation(containerID, containerName, info.Config.Labels, storedConfig)
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
		m.updateContainerState(containerID, nil, func(state *ContainerState) {
			state.State = "destroyed"
		})

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
func (m *Monitor) scanContainers(ctx context.Context) bool {
	m.inventoryScanMu.Lock()
	defer m.inventoryScanMu.Unlock()
	m.inventoryReady.Store(false)

	previousIDs := make(map[string]bool)
	m.containers.Range(func(key, _ any) bool {
		previousIDs[key.(string)] = true
		return true
	})
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		slog.Error("Failed to list containers", "error", err)
		return false
	}
	slog.Info("Scanning existing containers...", "count", len(containers))

	seenIDs := make(map[string]bool, len(containers))
	for _, ctr := range containers {
		state := containerStateFromSummary(ctr)
		seenIDs[state.ContainerID] = true
		if value, ok := m.containers.Load(state.ContainerID); ok && value.(*ContainerState).State == "destroyed" {
			continue
		}
		m.updateContainerState(state.ContainerID, func() *ContainerState {
			return &ContainerState{ContainerID: state.ContainerID, ContainerName: state.ContainerName}
		}, func(current *ContainerState) {
			current.ContainerName = state.ContainerName
			current.Image = state.Image
			current.State = state.State
			current.Ports = maps.Clone(state.Ports)
			current.WebPorts = maps.Clone(state.WebPorts)
			current.LegacyPortOptions = cloneStringSliceMap(state.LegacyPortOptions)
			current.Labels = maps.Clone(state.Labels)
			current.NetworkMode = state.NetworkMode
		})
	}
	for containerID := range previousIDs {
		if seenIDs[containerID] {
			continue
		}
		m.updateContainerState(containerID, nil, func(state *ContainerState) {
			state.State = "destroyed"
		})
		m.queueOperation(&AppOperation{Type: "destroy", ContainerID: containerID})
	}

	// Resolve legacy configs only after every current container is visible, so
	// one protocol-less key cannot be claimed by two compatible containers.
	for _, ctr := range containers {
		// Only process running containers
		if ctr.State != "running" {
			continue
		}
		summaryState := containerStateFromSummary(ctr)
		containerID := summaryState.ContainerID
		containerName := summaryState.ContainerName
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
			previousAppName := ""
			if storedConfig != nil {
				previousAppName = storedConfig.AppName
			}
			m.queueOperation(&AppOperation{
				Type:            "container_start",
				ContainerID:     containerID,
				ContainerName:   containerName,
				Labels:          ctr.Labels,
				LabelRevision:   labelRevisionForState(state),
				PreviousAppName: previousAppName,
			})
		} else if storedConfig != nil {
			slog.Info("Found storage-configured container", "container", containerName)
			m.queueStoredConfigOperation(containerID, containerName, ctr.Labels, storedConfig)
		}
	}
	m.inventoryReady.Store(true)
	return true
}

func containerStateFromSummary(ctr container.Summary) *ContainerState {
	containerID := ctr.ID
	if len(containerID) > 12 {
		containerID = containerID[:12]
	}
	containerName := ""
	if len(ctr.Names) > 0 {
		containerName = strings.TrimPrefix(ctr.Names[0], "/")
	}
	summaryPorts := summaryPortMap(ctr.Ports)
	identityPorts, webPorts := extractPortMappings(summaryPorts)
	return &ContainerState{
		ContainerID:       containerID,
		ContainerName:     containerName,
		Image:             ctr.Image,
		State:             ctr.State,
		Ports:             identityPorts,
		WebPorts:          webPorts,
		LegacyPortOptions: legacyPortOptions(summaryPorts),
		Labels:            maps.Clone(ctr.Labels),
		NetworkMode:       ctr.HostConfig.NetworkMode,
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
		Icon:          labelOrDefault(labels, "watchcow.icon", fpkgen.DefaultIconForImage(image)),
		Labels:        maps.Clone(labels),
		Status:        app.StatusRunning,
		Entries:       make([]app.Entry, 0),
	}

	// Parse entries from labels using fpkgen's ParseEntries
	defaultIcon := appInstance.Icon
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

	if !m.queueOperation(&AppOperation{
		Type:        "dashboard_uninstall",
		ContainerID: containerID,
		AppName:     appName,
		ConfigKey:   configKey,
	}) && m.configProvider != nil {
		m.recordDashboardDeleteFailure(configKey, "操作队列已满，请重试")
	}
}

// processDashboardUninstall handles uninstall triggered from dashboard.
func (m *Monitor) processDashboardUninstall(op *AppOperation) {
	appName := op.AppName
	if appName == "" {
		return
	}
	if m.labelSourceOwnsApp(appName) {
		slog.Info("Skipping dashboard uninstall because the app is owned by label config", "app", appName)
		m.recordDashboardDeleteFailure(op.ConfigKey, "应用当前由 labels 配置接管，无法从 Dashboard 卸载")
		return
	}
	if m.appHasActiveDesiredOwner(appName) {
		slog.Info("Skipping dashboard uninstall because another active config owns the app", "app", appName)
		m.recordDashboardDeleteFailure(op.ConfigKey, "应用仍由另一个活动配置使用，无法卸载")
		return
	}
	if op.ConfigKey != "" && m.configProvider != nil {
		current := m.configProvider.GetByKey(op.ConfigKey)
		if current == nil || !current.Deleting {
			slog.Info("Skipping stale dashboard uninstall because delete intent changed", "app", appName, "key", op.ConfigKey)
			return
		}
	}

	slog.Info("Processing dashboard uninstall", "app", appName)
	if err := m.markPendingUninstall(appName); err != nil {
		slog.Error("Failed to persist dashboard uninstall intent", "app", appName, "error", err)
	}

	// Uninstall from fnOS
	if m.installer != nil && m.installer.IsAppInstalled(appName) {
		if err := m.installer.Uninstall(appName); err != nil {
			slog.Error("Dashboard uninstall failed", "app", appName, "error", err)
			m.recordFailedUninstallStatus(appName, err)
			m.recordDashboardDeleteFailure(op.ConfigKey, err.Error())
			return
		}
	}

	// Unregister only after the system package is actually gone.
	m.registry.Unregister(appName)
	if err := m.clearLabelRevision(appName); err != nil {
		slog.Error("Failed to clear label revision after dashboard uninstall", "app", appName, "error", err)
	}

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
	if m.configProvider != nil && op.ConfigKey != "" {
		if err := m.configProvider.CompleteDelete(appName); err != nil {
			slog.Error("Failed to finalize dashboard config deletion", "app", appName, "error", err)
			return
		}
	}

	slog.Info("Dashboard uninstall completed", "app", appName)
}

func (m *Monitor) processOrphanUninstall(op *AppOperation) {
	appName := op.AppName
	if appName == "" || !m.isPendingUninstall(appName) || m.appHasActiveDesiredOwner(appName) {
		return
	}
	if m.installer != nil && m.installer.IsAppInstalled(appName) {
		if err := m.installer.Uninstall(appName); err != nil {
			slog.Error("Failed to uninstall orphaned fnOS app", "app", appName, "error", err)
			m.recordFailedUninstallStatus(appName, err)
			return
		}
	}
	m.registry.Unregister(appName)
	m.clearAppOwnership(appName, "")
	if err := m.clearLabelRevision(appName); err != nil {
		slog.Error("Failed to finalize orphaned app cleanup", "app", appName, "error", err)
	}
}

func (m *Monitor) appHasActiveDesiredOwner(appName string) bool {
	if appName == "" {
		return false
	}
	desired, _ := m.currentDesiredApps()
	return desired[appName]
}

func (m *Monitor) labelSourceOwnsApp(appName string) bool {
	owned := false
	m.containers.Range(func(_, value any) bool {
		state := value.(*ContainerState)
		if shouldInstall(state.Labels) && getAppNameFromLabels(state.Labels, state.ContainerName) == appName {
			owned = true
			return false
		}
		return true
	})
	return owned
}

// processDashboardReinstall handles config update: uninstall old app, then install with new config.
func (m *Monitor) processDashboardReinstall(ctx context.Context, op *AppOperation) {
	if op.StoredConfig != nil {
		if op.ForceReconcile {
			if !m.isCurrentStoredConfig(op.StoredConfig) {
				return
			}
		} else if !m.isCurrentPendingConfig(op.StoredConfig) {
			return
		}
	}
	if op.StoredConfig == nil && op.LabelRevision != "" && !m.isCurrentLabelRevision(op.ContainerID, op.LabelRevision) {
		return
	}
	oldAppName := m.installedAppName(op.ContainerID)
	desiredAppName := op.AppName
	if op.StoredConfig != nil {
		desiredAppName = op.StoredConfig.AppName
	} else {
		desiredAppName = getAppNameFromLabels(op.Labels, op.ContainerName)
	}
	if op.PreviousAppName != "" && m.installer != nil && m.installer.IsAppInstalled(op.PreviousAppName) {
		oldAppName = op.PreviousAppName
	}
	if oldAppName == "" && m.installer != nil && desiredAppName != "" && m.installer.IsAppInstalled(desiredAppName) {
		oldAppName = desiredAppName
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
			m.recordDashboardFailure(op.StoredConfig, err)
			return
		}
	}
	m.registry.Unregister(oldAppName)
	if err := m.clearLabelRevision(oldAppName); err != nil {
		slog.Error("Failed to clear label revision after uninstall", "app", oldAppName, "error", err)
	}

	// The package has a single system identity. Clear every previous owner so a
	// later destroy event cannot uninstall the package after it is reinstalled.
	m.clearAppOwnership(oldAppName, "")
	if op.StoredConfig != nil {
		current := m.isCurrentPendingConfig(op.StoredConfig)
		if op.ForceReconcile {
			current = m.isCurrentStoredConfig(op.StoredConfig)
		}
		if !current {
			slog.Info("Skipping stale dashboard config after uninstall", "app", op.StoredConfig.AppName, "revision", op.StoredConfig.Revision)
			return
		}
	}
	if op.StoredConfig == nil && op.LabelRevision != "" && !m.isCurrentLabelRevision(op.ContainerID, op.LabelRevision) {
		slog.Info("Skipping stale label config after uninstall", "app", desiredAppName, "revision", op.LabelRevision)
		return
	}
	if oldAppName != desiredAppName && desiredAppName != "" && m.installer != nil && m.installer.IsAppInstalled(desiredAppName) {
		slog.Info("Removing stale desired package before source reconciliation", "app", desiredAppName)
		if err := m.installer.Uninstall(desiredAppName); err != nil {
			slog.Error("Cannot reconcile because stale desired package uninstall failed", "app", desiredAppName, "error", err)
			m.recordFailedUninstallStatus(desiredAppName, err)
			m.recordDashboardFailure(op.StoredConfig, err)
			return
		}
		m.registry.Unregister(desiredAppName)
		if err := m.clearLabelRevision(desiredAppName); err != nil {
			slog.Error("Failed to clear stale desired label revision", "app", desiredAppName, "error", err)
		}
		m.clearAppOwnership(desiredAppName, "")
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
