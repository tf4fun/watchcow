package server

import (
	"encoding/gob"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"watchcow/internal/app"
	"watchcow/internal/docker"
)

type storageFile interface {
	io.Writer
	Sync() error
	Close() error
}

// DashboardStorage manages persistent storage of container configurations.
type DashboardStorage struct {
	mu         sync.RWMutex
	configs    map[ContainerKey]*StoredConfig
	filePath   string
	createFile func(string) (storageFile, error)
	renameFile func(string, string) error
}

// NewDashboardStorage creates a new storage instance.
// If TRIM_PKGETC is set, uses ${TRIM_PKGETC}/dashboard.gob.
// Otherwise uses /tmp/watchcow/dashboard.gob.
func NewDashboardStorage() (*DashboardStorage, error) {
	var filePath string
	if pkgEtc := os.Getenv("TRIM_PKGETC"); pkgEtc != "" {
		filePath = filepath.Join(pkgEtc, "dashboard.gob")
	} else {
		filePath = "/tmp/watchcow/dashboard.gob"
	}

	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	s := &DashboardStorage{
		configs:  make(map[ContainerKey]*StoredConfig),
		filePath: filePath,
		createFile: func(path string) (storageFile, error) {
			return os.Create(path)
		},
		renameFile: os.Rename,
	}

	// Load existing data
	if err := s.load(); err != nil {
		slog.Warn("Failed to load dashboard storage, starting fresh", "path", filePath, "error", err)
	} else {
		if count, err := s.migrateStoredAppNames(); err != nil {
			slog.Warn("Failed to persist migrated dashboard app names", "path", filePath, "error", err)
		} else if count > 0 {
			slog.Info("Migrated dashboard app names to fnOS limits", "path", filePath, "configs", count)
		}
		slog.Debug("Loaded dashboard storage", "path", filePath, "configs", len(s.configs))
	}

	return s, nil
}

// load reads configurations from disk.
// If a .tmp file exists from an interrupted save, attempts to recover from it.
func (s *DashboardStorage) load() error {
	tmpPath := s.filePath + ".tmp"

	// Check for interrupted atomic write: .tmp exists but main file is missing or stale
	if _, err := os.Stat(tmpPath); err == nil {
		if s.tryLoadFrom(tmpPath) == nil {
			slog.Info("Recovered storage from incomplete save", "path", tmpPath)
			// Promote tmp to main file
			os.Rename(tmpPath, s.filePath)
			return nil
		}
		// tmp is corrupt, discard it
		os.Remove(tmpPath)
	}

	return s.tryLoadFrom(s.filePath)
}

// tryLoadFrom attempts to load configs from a specific file path.
func (s *DashboardStorage) tryLoadFrom(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	decoder := gob.NewDecoder(f)
	return decoder.Decode(&s.configs)
}

// save writes configurations to disk using atomic write (write-to-temp + rename)
// to prevent data loss on power failure.
func (s *DashboardStorage) save() error {
	tmpPath := s.filePath + ".tmp"
	f, err := s.createFile(tmpPath)
	if err != nil {
		return err
	}

	encoder := gob.NewEncoder(f)
	if err := encoder.Encode(s.configs); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := s.renameFile(tmpPath, s.filePath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// Get retrieves a configuration by key.
func (s *DashboardStorage) Get(key ContainerKey) *StoredConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cfg, ok := s.configs[key]; ok {
		return cloneStoredConfig(cfg)
	}
	return nil
}

// Set stores a configuration.
func (s *DashboardStorage) Set(cfg *StoredConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateAndSaveLocked(func(configs map[ContainerKey]*StoredConfig) {
		stored := cloneStoredConfig(cfg)
		reconcileStoredAppName(stored, stored.Key)
		configs[stored.Key] = stored
	})
}

// Replace stores cfg and removes obsolete keys in the same persisted update.
func (s *DashboardStorage) Replace(obsolete []ContainerKey, cfg *StoredConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateAndSaveLocked(func(configs map[ContainerKey]*StoredConfig) {
		for _, key := range obsolete {
			if key != cfg.Key {
				delete(configs, key)
			}
		}
		stored := cloneStoredConfig(cfg)
		reconcileStoredAppName(stored, stored.Key)
		configs[stored.Key] = stored
	})
}

// Delete removes a configuration.
func (s *DashboardStorage) Delete(key ContainerKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateAndSaveLocked(func(configs map[ContainerKey]*StoredConfig) {
		delete(configs, key)
	})
}

// DeleteMany removes all supplied keys in one persisted update.
func (s *DashboardStorage) DeleteMany(keys []ContainerKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateAndSaveLocked(func(configs map[ContainerKey]*StoredConfig) {
		for _, key := range keys {
			delete(configs, key)
		}
	})
}

// List returns all stored configurations.
func (s *DashboardStorage) List() []*StoredConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*StoredConfig, 0, len(s.configs))
	for _, cfg := range s.configs {
		result = append(result, cloneStoredConfig(cfg))
	}
	return result
}

// Has checks if a configuration exists.
func (s *DashboardStorage) Has(key ContainerKey) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.configs[key]
	return ok
}

// GetByKey implements docker.ConfigProvider interface.
// Returns the stored config for a container key, or nil if not found.
func (s *DashboardStorage) GetByKey(key string) *docker.StoredConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cfg, ok := s.configs[ContainerKey(key)]
	if !ok {
		return nil
	}
	return convertStoredConfigToDocker(cfg)
}

// GetCompatibleCandidates implements docker.ConfigProvider for configs saved
// before storage keys encoded the published port protocol.
func (s *DashboardStorage) GetCompatibleCandidates(image string, identityPorts map[string]string, portOptions map[string][]string) []docker.StoredConfigMatch {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := s.findCompatibleKeysLocked(image, identityPorts, portOptions)
	matches := make([]docker.StoredConfigMatch, 0, len(keys))
	for _, key := range keys {
		matches = append(matches, docker.StoredConfigMatch{
			Key:    string(key),
			Config: convertStoredConfigToDocker(s.configs[key]),
		})
	}
	return matches
}

// GetNamedCandidates implements docker.ConfigProvider for stable ownership
// across image-tag and published-port changes.
func (s *DashboardStorage) GetNamedCandidates(containerName string) []docker.StoredConfigMatch {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := s.findNamedKeysLocked(containerName)
	matches := make([]docker.StoredConfigMatch, 0, len(keys))
	for _, key := range keys {
		matches = append(matches, docker.StoredConfigMatch{
			Key:    string(key),
			Config: convertStoredConfigToDocker(s.configs[key]),
		})
	}
	return matches
}

// GetDeletingConfigs implements docker.ConfigProvider so uninstall intents are
// recoverable even when their Docker container is stopped or gone.
func (s *DashboardStorage) GetDeletingConfigs() []docker.StoredConfigMatch {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]ContainerKey, 0)
	for key, config := range s.configs {
		if config.Deleting {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	matches := make([]docker.StoredConfigMatch, 0, len(keys))
	for _, key := range keys {
		matches = append(matches, docker.StoredConfigMatch{Key: string(key), Config: convertStoredConfigToDocker(s.configs[key])})
	}
	return matches
}

// GetAllConfigs implements docker.ConfigProvider for applied-owner
// reconciliation when containers are removed while WatchCow is offline.
func (s *DashboardStorage) GetAllConfigs() []docker.StoredConfigMatch {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]ContainerKey, 0, len(s.configs))
	for key := range s.configs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	matches := make([]docker.StoredConfigMatch, 0, len(keys))
	for _, key := range keys {
		matches = append(matches, docker.StoredConfigMatch{Key: string(key), Config: convertStoredConfigToDocker(s.configs[key])})
	}
	return matches
}

// MarkApplied clears Pending for the exact revision that was installed.
func (s *DashboardStorage) MarkApplied(key, revision string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, ok := s.configs[ContainerKey(key)]
	if !ok || cfg.Revision != revision || !cfg.Pending {
		return nil
	}
	return s.updateAndSaveLocked(func(configs map[ContainerKey]*StoredConfig) {
		cfg := configs[ContainerKey(key)]
		cfg.Pending = false
		cfg.LastError = ""
	})
}

// MarkFailed records an asynchronous apply/delete error using an exact
// revision CAS, including legacy configs whose revision is empty.
func (s *DashboardStorage) MarkFailed(key, revision, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, ok := s.configs[ContainerKey(key)]
	if !ok || cfg.Revision != revision {
		return nil
	}
	return s.updateAndSaveLocked(func(configs map[ContainerKey]*StoredConfig) {
		config := configs[ContainerKey(key)]
		config.LastError = message
		if !config.Deleting {
			config.Pending = true
		}
	})
}

// MarkDeleting persists delete intent before asynchronous package removal.
func (s *DashboardStorage) MarkDeleting(keys []ContainerKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateAndSaveLocked(func(configs map[ContainerKey]*StoredConfig) {
		for _, key := range keys {
			if cfg, ok := configs[key]; ok {
				cfg.Deleting = true
				cfg.Pending = false
				cfg.LastError = ""
			}
		}
	})
}

// CompleteDelete removes deleting configs for an app after its fnOS package is
// gone. A concurrent save clears Deleting and preserves the active config.
func (s *DashboardStorage) CompleteDelete(appName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	for _, cfg := range s.configs {
		if cfg.AppName == appName && cfg.Deleting {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	return s.updateAndSaveLocked(func(configs map[ContainerKey]*StoredConfig) {
		for key, cfg := range configs {
			if cfg.AppName == appName && cfg.Deleting {
				delete(configs, key)
			}
		}
	})
}

// MigrateLegacy atomically moves a uniquely-owned legacy key to the canonical
// protocol-qualified (or named no-port) key.
func (s *DashboardStorage) MigrateLegacy(oldKey, newKey string) (*docker.StoredConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := ContainerKey(oldKey)
	current := ContainerKey(newKey)
	if cfg, ok := s.configs[current]; ok {
		return convertStoredConfigToDocker(cfg), nil
	}
	cfg, ok := s.configs[old]
	if !ok {
		return nil, nil
	}
	migrated := cloneStoredConfig(cfg)
	migrated.Key = current
	reconcileMigratedRuntimeConfig(migrated, old, current)
	reconcileStoredAppName(migrated, current)
	if err := s.updateAndSaveLocked(func(configs map[ContainerKey]*StoredConfig) {
		delete(configs, old)
		configs[current] = migrated
	}); err != nil {
		return nil, err
	}
	return convertStoredConfigToDocker(migrated), nil
}

func (s *DashboardStorage) migrateStoredAppNames() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for key, config := range s.configs {
		candidate := cloneStoredConfig(config)
		if reconcileStoredAppName(candidate, key) {
			count++
		}
	}
	if count == 0 {
		return 0, nil
	}

	err := s.updateAndSaveLocked(func(configs map[ContainerKey]*StoredConfig) {
		for key, config := range configs {
			reconcileStoredAppName(config, key)
		}
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// reconcileStoredAppName repairs Dashboard identifiers produced before the
// fnOS 3-32 character limit was enforced. Deleting configs retain the exact
// package identity requested for removal. Overlong identifiers are rejected
// before installation, so active configs do not need an old package identity.
func reconcileStoredAppName(config *StoredConfig, key ContainerKey) bool {
	if config == nil || config.Deleting || config.AppName == "" ||
		(len(config.AppName) >= app.MinAppNameLength && len(config.AppName) <= app.MaxAppNameLength) {
		return false
	}

	appName := ""
	if len(config.AppName) > app.MaxAppNameLength {
		appName = app.BoundGeneratedAppName(config.AppName)
	} else {
		containerName, identityPorts, named := parseNamedContainerKey(key)
		if !named {
			return false
		}
		appName = app.DashboardAppName(containerName, firstHostPort(webPortsFromIdentity(identityPorts)))
	}
	if appName == config.AppName {
		return false
	}

	blocked := !config.Pending && config.LastError != ""
	config.AppName = appName
	if !blocked {
		config.Pending = true
		config.LastError = ""
	}
	config.Revision = dashboardConfigRevision(config)
	config.UpdatedAt = time.Now()
	return true
}

func reconcileMigratedRuntimeConfig(config *StoredConfig, oldKey, newKey ContainerKey) {
	oldName, oldPorts, oldNamed := parseNamedContainerKey(oldKey)
	newName, newPorts, newNamed := parseNamedContainerKey(newKey)
	if !oldNamed || !newNamed {
		return
	}

	requiresApply := oldName != newName
	blockingError := ""
	oldWebPorts := webPortsFromIdentity(oldPorts)
	newWebPorts := webPortsFromIdentity(newPorts)
	containerPorts := make([]string, 0, len(oldWebPorts))
	for containerPort := range oldWebPorts {
		containerPorts = append(containerPorts, containerPort)
	}
	sort.Strings(containerPorts)
	for i := range config.Entries {
		if config.Entries[i].Name != "" {
			continue
		}
		for _, containerPort := range containerPorts {
			oldHostPort := oldWebPorts[containerPort]
			newHostPort := newWebPorts[containerPort]
			if oldHostPort != "" && config.Entries[i].Port == oldHostPort && oldHostPort != newHostPort {
				if newHostPort == "" {
					newHostPort = firstHostPort(newWebPorts)
				}
				config.Entries[i].Port = newHostPort
				if newHostPort == "" && config.Entries[i].Redirect == "" {
					blockingError = "原入口端口映射已移除，请配置新的端口或外部跳转地址"
				}
				requiresApply = true
				break
			}
		}
		break
	}
	if !requiresApply {
		return
	}

	config.Pending = blockingError == ""
	config.Deleting = false
	config.LastError = blockingError
	config.Revision = dashboardConfigRevision(config)
	config.UpdatedAt = time.Now()
}

func parseNamedContainerKey(key ContainerKey) (string, map[string]string, bool) {
	_, identity, ok := strings.Cut(string(key), "|")
	if !ok || !strings.HasPrefix(identity, "@") {
		return "", nil, false
	}
	name, encodedPorts, hasPorts := strings.Cut(strings.TrimPrefix(identity, "@"), ";")
	ports := make(map[string]string)
	if hasPorts {
		for _, pair := range strings.Split(encodedPorts, ",") {
			containerPort, hostPort, valid := strings.Cut(pair, ":")
			if valid {
				ports[containerPort] = hostPort
			}
		}
	}
	return name, ports, true
}

func webPortsFromIdentity(ports map[string]string) map[string]string {
	webPorts := make(map[string]string)
	for key, hostPort := range ports {
		containerPort, protocol, qualified := strings.Cut(key, "/")
		if !qualified || protocol == "tcp" {
			webPorts[containerPort] = hostPort
		}
	}
	return webPorts
}

// FindCompatibleKeys returns all legacy storage keys matching the container.
func (s *DashboardStorage) FindCompatibleKeys(image string, identityPorts map[string]string, portOptions map[string][]string) []ContainerKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findCompatibleKeysLocked(image, identityPorts, portOptions)
}

// FindNamedKeys returns configurations whose canonical owner has the supplied
// container name, independent of image and port details.
func (s *DashboardStorage) FindNamedKeys(containerName string) []ContainerKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findNamedKeysLocked(containerName)
}

func (s *DashboardStorage) findNamedKeysLocked(containerName string) []ContainerKey {
	wanted := strings.TrimPrefix(containerName, "/")
	var keys []ContainerKey
	for key := range s.configs {
		_, identity, ok := strings.Cut(string(key), "|")
		if !ok || !strings.HasPrefix(identity, "@") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(identity, "@"), ";")
		if name == wanted {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func (s *DashboardStorage) findCompatibleKeysLocked(image string, identityPorts map[string]string, portOptions map[string][]string) []ContainerKey {
	keys := make([]string, 0, len(s.configs))
	for key := range s.configs {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	var matches []ContainerKey
	for _, key := range keys {
		if docker.MatchesCompatibleContainerKey(key, image, identityPorts, portOptions) {
			matches = append(matches, ContainerKey(key))
		}
	}
	return matches
}

func convertStoredConfigToDocker(cfg *StoredConfig) *docker.StoredConfig {

	// Convert server.StoredConfig to docker.StoredConfig
	result := &docker.StoredConfig{
		Key:         string(cfg.Key),
		AppName:     cfg.AppName,
		DisplayName: cfg.DisplayName,
		Description: cfg.Description,
		Version:     cfg.Version,
		Maintainer:  cfg.Maintainer,
		IconBase64:  cfg.IconBase64,
		Revision:    cfg.Revision,
		Pending:     cfg.Pending,
		Deleting:    cfg.Deleting,
		LastError:   cfg.LastError,
		Entries:     make([]docker.StoredEntry, 0, len(cfg.Entries)),
	}

	for _, e := range cfg.Entries {
		converted := docker.StoredEntry(e)
		converted.FileTypes = append([]string(nil), e.FileTypes...)
		result.Entries = append(result.Entries, converted)
	}

	return result
}

func cloneStoredConfig(cfg *StoredConfig) *StoredConfig {
	if cfg == nil {
		return nil
	}
	copy := *cfg
	if cfg.Entries != nil {
		copy.Entries = append([]StoredEntry(nil), cfg.Entries...)
		for i := range copy.Entries {
			if cfg.Entries[i].FileTypes != nil {
				copy.Entries[i].FileTypes = append([]string(nil), cfg.Entries[i].FileTypes...)
			}
		}
	}
	return &copy
}

func cloneStoredConfigs(configs map[ContainerKey]*StoredConfig) map[ContainerKey]*StoredConfig {
	cloned := make(map[ContainerKey]*StoredConfig, len(configs))
	for key, cfg := range configs {
		cloned[key] = cloneStoredConfig(cfg)
	}
	return cloned
}

// updateAndSaveLocked applies a mutation transactionally. The caller must hold
// s.mu for writing; failed persistence leaves both memory and disk unchanged.
func (s *DashboardStorage) updateAndSaveLocked(update func(map[ContainerKey]*StoredConfig)) error {
	previous := s.configs
	s.configs = cloneStoredConfigs(previous)
	update(s.configs)
	if err := s.save(); err != nil {
		s.configs = previous
		return err
	}
	return nil
}
