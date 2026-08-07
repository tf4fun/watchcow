package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDashboardStorage_SetAndGet(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	defer os.Unsetenv("TRIM_PKGETC")

	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	key := ContainerKey("nginx:alpine|80:8080")
	config := &StoredConfig{
		Key:         key,
		AppName:     "watchcow.nginx",
		DisplayName: "Nginx",
		Description: "Web server",
		Version:     "1.0.0",
		Maintainer:  "Test",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Entries: []StoredEntry{
			{
				Name:     "",
				Title:    "Nginx",
				Protocol: "http",
				Port:     "80",
				Path:     "/",
				UIType:   "url",
				AllUsers: true,
			},
		},
	}

	// Set
	if err := storage.Set(config); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Get
	got := storage.Get(key)
	if got == nil {
		t.Fatal("Get() returned nil")
	}
	if got.AppName != "watchcow.nginx" {
		t.Errorf("AppName = %q, want %q", got.AppName, "watchcow.nginx")
	}
	if got.DisplayName != "Nginx" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Nginx")
	}
	if len(got.Entries) != 1 {
		t.Errorf("len(Entries) = %d, want 1", len(got.Entries))
	}

	// Verify file was created
	filePath := filepath.Join(tmpDir, "dashboard.gob")
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Error("dashboard.gob file was not created")
	}
}

func TestDashboardStorage_Has(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	defer os.Unsetenv("TRIM_PKGETC")

	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	key := ContainerKey("nginx:alpine|80:8080")

	// Should not have before set
	if storage.Has(key) {
		t.Error("Has() should return false before Set()")
	}

	// Set
	storage.Set(&StoredConfig{Key: key, AppName: "test"})

	// Should have after set
	if !storage.Has(key) {
		t.Error("Has() should return true after Set()")
	}
}

func TestDashboardStorage_Delete(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	defer os.Unsetenv("TRIM_PKGETC")

	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	key := ContainerKey("nginx:alpine|80:8080")

	// Set then delete
	storage.Set(&StoredConfig{Key: key, AppName: "test"})
	if !storage.Has(key) {
		t.Fatal("config should exist after Set()")
	}

	if err := storage.Delete(key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if storage.Has(key) {
		t.Error("config should not exist after Delete()")
	}
	if storage.Get(key) != nil {
		t.Error("Get() should return nil after Delete()")
	}
}

func TestDashboardStorage_List(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	defer os.Unsetenv("TRIM_PKGETC")

	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	// Add multiple configs
	configs := []*StoredConfig{
		{Key: ContainerKey("nginx|80:8080"), AppName: "nginx"},
		{Key: ContainerKey("redis|6379:6379"), AppName: "redis"},
		{Key: ContainerKey("mysql|3306:3306"), AppName: "mysql"},
	}

	for _, cfg := range configs {
		storage.Set(cfg)
	}

	// List
	list := storage.List()
	if len(list) != 3 {
		t.Errorf("List() returned %d configs, want 3", len(list))
	}

	// Verify all configs are present
	appNames := make(map[string]bool)
	for _, cfg := range list {
		appNames[cfg.AppName] = true
	}
	for _, expected := range []string{"nginx", "redis", "mysql"} {
		if !appNames[expected] {
			t.Errorf("List() missing config with AppName %q", expected)
		}
	}
}

func TestDashboardStorage_Persistence(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	defer os.Unsetenv("TRIM_PKGETC")

	// Create storage and add config
	storage1, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	key := ContainerKey("nginx:alpine|80:8080")
	storage1.Set(&StoredConfig{
		Key:         key,
		AppName:     "watchcow.nginx",
		DisplayName: "Nginx Persisted",
	})

	// Create new storage instance (should load from file)
	storage2, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	// Should have the same config
	got := storage2.Get(key)
	if got == nil {
		t.Fatal("config should persist across storage instances")
	}
	if got.DisplayName != "Nginx Persisted" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Nginx Persisted")
	}
}

func TestDashboardStorage_GetByKey(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	defer os.Unsetenv("TRIM_PKGETC")

	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	key := ContainerKey("nginx:alpine|80:8080")
	storage.Set(&StoredConfig{
		Key:         key,
		AppName:     "watchcow.nginx",
		DisplayName: "Nginx",
		Description: "Web server",
		Version:     "1.0.0",
		Maintainer:  "Test",
		Entries: []StoredEntry{
			{
				Name:          "",
				Title:         "Nginx",
				Protocol:      "http",
				Port:          "80",
				Path:          "/",
				UIType:        "url",
				AllUsers:      true,
				Redirect:      "https://example.com",
				ForceExternal: true,
				FileTypes:     []string{".html", ".css"},
			},
		},
	})

	// Test GetByKey (docker.ConfigProvider interface)
	dockerCfg := storage.GetByKey(string(key))
	if dockerCfg == nil {
		t.Fatal("GetByKey() returned nil")
	}
	if dockerCfg.AppName != "watchcow.nginx" {
		t.Errorf("AppName = %q, want %q", dockerCfg.AppName, "watchcow.nginx")
	}
	if dockerCfg.DisplayName != "Nginx" {
		t.Errorf("DisplayName = %q, want %q", dockerCfg.DisplayName, "Nginx")
	}
	if len(dockerCfg.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(dockerCfg.Entries))
	}
	if dockerCfg.Entries[0].Port != "80" {
		t.Errorf("Entry[0].Port = %q, want %q", dockerCfg.Entries[0].Port, "80")
	}
	if dockerCfg.Entries[0].Redirect != "https://example.com" {
		t.Errorf("Entry[0].Redirect = %q, want %q", dockerCfg.Entries[0].Redirect, "https://example.com")
	}
	if !dockerCfg.Entries[0].ForceExternal {
		t.Error("Entry[0].ForceExternal should be true")
	}

	dockerCfg.Entries[0].FileTypes[0] = "mutated"
	fresh := storage.GetByKey(string(key))
	if fresh.Entries[0].FileTypes[0] != ".html" {
		t.Errorf("GetByKey returned shared FileTypes storage: %v", fresh.Entries[0].FileTypes)
	}

	// Test GetByKey for nonexistent key
	if storage.GetByKey("nonexistent|") != nil {
		t.Error("GetByKey() should return nil for nonexistent key")
	}
}

func TestDashboardStorage_GetCompatibleLegacyKey(t *testing.T) {
	t.Setenv("TRIM_PKGETC", t.TempDir())
	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatal(err)
	}

	key := ContainerKey("dns:latest|53:2053,80:8080")
	if err := storage.Set(&StoredConfig{
		Key:     key,
		AppName: "watchcow.dns",
		Entries: []StoredEntry{{FileTypes: []string{"zone"}}},
	}); err != nil {
		t.Fatal(err)
	}
	options := map[string][]string{
		"53": {"1053", "2053"},
		"80": {"8080"},
	}

	identityPorts := map[string]string{"53/tcp": "1053", "53/udp": "2053", "80/tcp": "8080"}
	matched := storage.FindCompatibleKeys("dns:latest", identityPorts, options)
	if len(matched) != 1 || matched[0] != key {
		t.Fatalf("FindCompatibleKeys() = %q; want [%q]", matched, key)
	}
	matches := storage.GetCompatibleCandidates("dns:latest", identityPorts, options)
	if len(matches) != 1 || matches[0].Config.AppName != "watchcow.dns" {
		t.Fatalf("GetCompatibleCandidates() = %+v", matches)
	}
	config := matches[0].Config
	config.Entries[0].FileTypes[0] = "mutated"
	if fresh := storage.GetCompatibleCandidates("dns:latest", identityPorts, options)[0].Config; fresh.Entries[0].FileTypes[0] != "zone" {
		t.Errorf("GetCompatibleCandidates returned shared entry state: %+v", fresh.Entries)
	}

	if got := storage.FindCompatibleKeys("dns:latest", identityPorts, map[string][]string{"53": {"9999"}, "80": {"8080"}}); len(got) != 0 {
		t.Error("FindCompatibleKeys matched a host port outside the current binding set")
	}
}

func TestDashboardStorage_MarkAppliedUsesRevisionCAS(t *testing.T) {
	t.Setenv("TRIM_PKGETC", t.TempDir())
	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatal(err)
	}
	key := ContainerKey("web:latest|8080/tcp:18080")
	if err := storage.Set(&StoredConfig{Key: key, Revision: "new", Pending: true}); err != nil {
		t.Fatal(err)
	}

	if err := storage.MarkApplied(string(key), "old"); err != nil {
		t.Fatal(err)
	}
	if !storage.Get(key).Pending {
		t.Error("stale install acknowledgement cleared the new revision")
	}
	if err := storage.MarkApplied(string(key), "new"); err != nil {
		t.Fatal(err)
	}
	if storage.Get(key).Pending {
		t.Error("matching install acknowledgement did not clear pending")
	}
}

func TestDashboardStorage_GetReturnsCopy(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	defer os.Unsetenv("TRIM_PKGETC")

	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	key := ContainerKey("nginx|80:8080")
	storage.Set(&StoredConfig{Key: key, AppName: "original"})

	// Get and modify
	got := storage.Get(key)
	got.AppName = "modified"

	// Original should be unchanged
	got2 := storage.Get(key)
	if got2.AppName != "original" {
		t.Errorf("Get() should return a copy, but original was modified")
	}
}

func TestDashboardStorage_GetReturnsDeepCopy(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	defer os.Unsetenv("TRIM_PKGETC")

	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	key := ContainerKey("nginx|80:8080")
	original := &StoredConfig{
		Key:     key,
		AppName: "original",
		Entries: []StoredEntry{
			{Name: "admin", FileTypes: []string{"txt"}},
		},
	}
	if err := storage.Set(original); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	original.Entries[0].Name = "changed-after-set"
	original.Entries[0].FileTypes[0] = "md"

	got := storage.Get(key)
	got.Entries[0].Name = "changed-after-get"
	got.Entries[0].FileTypes[0] = "json"

	got2 := storage.Get(key)
	if got2.Entries[0].Name != "admin" {
		t.Errorf("stored entry name = %q, want %q", got2.Entries[0].Name, "admin")
	}
	if got2.Entries[0].FileTypes[0] != "txt" {
		t.Errorf("stored file type = %q, want %q", got2.Entries[0].FileTypes[0], "txt")
	}
}

func TestDashboardStorage_FallbackPath(t *testing.T) {
	// Unset TRIM_PKGETC to use fallback
	os.Unsetenv("TRIM_PKGETC")

	// This should use /tmp/watchcow/dashboard.gob
	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	// Just verify it was created successfully
	if storage == nil {
		t.Error("storage should not be nil")
	}
}
