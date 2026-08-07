package server

import (
	"encoding/gob"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"watchcow/internal/docker"
)

// DashboardStorage manages persistent storage of container configurations.
type DashboardStorage struct {
	mu       sync.RWMutex
	configs  map[ContainerKey]*StoredConfig
	filePath string
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
	}

	// Load existing data
	if err := s.load(); err != nil {
		slog.Warn("Failed to load dashboard storage, starting fresh", "path", filePath, "error", err)
	} else {
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
	f, err := os.Create(tmpPath)
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
	f.Close()

	return os.Rename(tmpPath, s.filePath)
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

	s.configs[cfg.Key] = cloneStoredConfig(cfg)
	return s.save()
}

// Replace stores cfg and removes obsolete keys in the same persisted update.
func (s *DashboardStorage) Replace(obsolete []ContainerKey, cfg *StoredConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, key := range obsolete {
		if key != cfg.Key {
			delete(s.configs, key)
		}
	}
	s.configs[cfg.Key] = cloneStoredConfig(cfg)
	return s.save()
}

// Delete removes a configuration.
func (s *DashboardStorage) Delete(key ContainerKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.configs, key)
	return s.save()
}

// DeleteMany removes all supplied keys in one persisted update.
func (s *DashboardStorage) DeleteMany(keys []ContainerKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, key := range keys {
		delete(s.configs, key)
	}
	return s.save()
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

// MarkApplied clears Pending for the exact revision that was installed.
func (s *DashboardStorage) MarkApplied(key, revision string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, ok := s.configs[ContainerKey(key)]
	if !ok || cfg.Revision != revision || !cfg.Pending {
		return nil
	}
	cfg = cloneStoredConfig(cfg)
	cfg.Pending = false
	s.configs[ContainerKey(key)] = cfg
	return s.save()
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
	delete(s.configs, old)
	s.configs[current] = migrated
	if err := s.save(); err != nil {
		delete(s.configs, current)
		s.configs[old] = cfg
		return nil, err
	}
	return convertStoredConfigToDocker(migrated), nil
}

// FindCompatibleKeys returns all legacy storage keys matching the container.
func (s *DashboardStorage) FindCompatibleKeys(image string, identityPorts map[string]string, portOptions map[string][]string) []ContainerKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findCompatibleKeysLocked(image, identityPorts, portOptions)
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
