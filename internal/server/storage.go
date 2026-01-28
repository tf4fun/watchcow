package server

import (
	"encoding/gob"
	"log/slog"
	"os"
	"path/filepath"
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
func (s *DashboardStorage) load() error {
	f, err := os.Open(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No file yet, not an error
		}
		return err
	}
	defer f.Close()

	decoder := gob.NewDecoder(f)
	return decoder.Decode(&s.configs)
}

// save writes configurations to disk.
func (s *DashboardStorage) save() error {
	f, err := os.Create(s.filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	encoder := gob.NewEncoder(f)
	return encoder.Encode(s.configs)
}

// Get retrieves a configuration by key.
func (s *DashboardStorage) Get(key ContainerKey) *StoredConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cfg, ok := s.configs[key]; ok {
		// Return a copy to avoid race conditions
		copy := *cfg
		return &copy
	}
	return nil
}

// Set stores a configuration.
func (s *DashboardStorage) Set(cfg *StoredConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.configs[cfg.Key] = cfg
	return s.save()
}

// Delete removes a configuration.
func (s *DashboardStorage) Delete(key ContainerKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.configs, key)
	return s.save()
}

// List returns all stored configurations.
func (s *DashboardStorage) List() []*StoredConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*StoredConfig, 0, len(s.configs))
	for _, cfg := range s.configs {
		copy := *cfg
		result = append(result, &copy)
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

	// Convert server.StoredConfig to docker.StoredConfig
	result := &docker.StoredConfig{
		AppName:     cfg.AppName,
		DisplayName: cfg.DisplayName,
		Description: cfg.Description,
		Version:     cfg.Version,
		Maintainer:  cfg.Maintainer,
		IconBase64:  cfg.IconBase64,
		Entries:     make([]docker.StoredEntry, 0, len(cfg.Entries)),
	}

	for _, e := range cfg.Entries {
		result.Entries = append(result.Entries, docker.StoredEntry{
			Name:       e.Name,
			Title:      e.Title,
			Protocol:   e.Protocol,
			Port:       e.Port,
			Path:       e.Path,
			UIType:     e.UIType,
			AllUsers:   e.AllUsers,
			FileTypes:  e.FileTypes,
			NoDisplay:  e.NoDisplay,
			Redirect:   e.Redirect,
			IconBase64: e.IconBase64,
		})
	}

	return result
}
