package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var errInjectedStorageWrite = errors.New("injected storage write failure")

type injectedStorageFile struct {
	writeErr error
	syncErr  error
}

func (f *injectedStorageFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *injectedStorageFile) Sync() error {
	return f.syncErr
}

func (f *injectedStorageFile) Close() error {
	return nil
}

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

func TestDashboardStorage_NamedLookupAndPortMigration(t *testing.T) {
	t.Setenv("TRIM_PKGETC", t.TempDir())
	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatal(err)
	}
	oldKey := ContainerKey("nginx:1.25|@nginx;80/tcp:8080")
	newKey := ContainerKey("nginx:1.26|@nginx;80/tcp:9090")
	if err := storage.Set(&StoredConfig{
		Key: oldKey, AppName: "watchcow.nginx.8080", Revision: "old",
		Entries: []StoredEntry{{Port: "8080", Protocol: "http", Path: "/", UIType: "url"}},
	}); err != nil {
		t.Fatal(err)
	}

	if keys := storage.FindNamedKeys("/nginx"); len(keys) != 1 || keys[0] != oldKey {
		t.Fatalf("FindNamedKeys() = %v", keys)
	}
	if matches := storage.GetNamedCandidates("nginx"); len(matches) != 1 || matches[0].Key != string(oldKey) {
		t.Fatalf("GetNamedCandidates() = %+v", matches)
	}
	migrated, err := storage.MigrateLegacy(string(oldKey), string(newKey))
	if err != nil {
		t.Fatal(err)
	}
	if migrated == nil || migrated.Key != string(newKey) || !migrated.Pending || migrated.Revision == "old" {
		t.Fatalf("migrated config = %+v", migrated)
	}
	if len(migrated.Entries) != 1 || migrated.Entries[0].Port != "9090" {
		t.Fatalf("default dashboard port did not follow mapping: %+v", migrated.Entries)
	}
	if storage.Get(oldKey) != nil || storage.Get(newKey) == nil {
		t.Fatalf("migration was not persisted: old=%+v new=%+v", storage.Get(oldKey), storage.Get(newKey))
	}
}

func TestDashboardStorage_MigrationTracksSelectedNonFirstPort(t *testing.T) {
	t.Setenv("TRIM_PKGETC", t.TempDir())
	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatal(err)
	}
	oldKey := ContainerKey("web:1|@web;80/tcp:8080,443/tcp:8443")
	newKey := ContainerKey("web:2|@web;80/tcp:8080,443/tcp:9443")
	if err := storage.Set(&StoredConfig{
		Key: oldKey, AppName: "watchcow.web", Revision: "old",
		Entries: []StoredEntry{{Port: "8443", Protocol: "https", Path: "/", UIType: "url"}},
	}); err != nil {
		t.Fatal(err)
	}

	migrated, err := storage.MigrateLegacy(string(oldKey), string(newKey))
	if err != nil {
		t.Fatal(err)
	}
	if migrated == nil || !migrated.Pending || len(migrated.Entries) != 1 || migrated.Entries[0].Port != "9443" {
		t.Fatalf("selected secondary port did not migrate: %+v", migrated)
	}
}

func TestDashboardStorage_MigrationFallsBackWhenSelectedMappingIsRemoved(t *testing.T) {
	t.Setenv("TRIM_PKGETC", t.TempDir())
	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatal(err)
	}
	oldKey := ContainerKey("web:1|@web;80/tcp:8080,443/tcp:8443")
	newKey := ContainerKey("web:2|@web;80/tcp:8080")
	if err := storage.Set(&StoredConfig{
		Key: oldKey, AppName: "watchcow.web", Revision: "old",
		Entries: []StoredEntry{{Port: "8443", Protocol: "https", Path: "/", UIType: "url"}},
	}); err != nil {
		t.Fatal(err)
	}

	migrated, err := storage.MigrateLegacy(string(oldKey), string(newKey))
	if err != nil {
		t.Fatal(err)
	}
	if migrated == nil || !migrated.Pending || len(migrated.Entries) != 1 || migrated.Entries[0].Port != "8080" {
		t.Fatalf("removed selected mapping did not fall back to the remaining web port: %+v", migrated)
	}
}

func TestDashboardStorage_MigrationBlocksWhenAllSelectedPortsAreRemoved(t *testing.T) {
	t.Setenv("TRIM_PKGETC", t.TempDir())
	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatal(err)
	}
	oldKey := ContainerKey("web:1|@web;80/tcp:8080")
	newKey := ContainerKey("web:2|@web")
	if err := storage.Set(&StoredConfig{
		Key: oldKey, AppName: "watchcow.web", Revision: "old",
		Entries: []StoredEntry{{Port: "8080", Protocol: "http", Path: "/", UIType: "url"}},
	}); err != nil {
		t.Fatal(err)
	}

	migrated, err := storage.MigrateLegacy(string(oldKey), string(newKey))
	if err != nil {
		t.Fatal(err)
	}
	if migrated == nil || migrated.Pending || migrated.LastError == "" || migrated.Entries[0].Port != "" {
		t.Fatalf("portless migration was not blocked for dashboard input: %+v", migrated)
	}
}

func TestDashboardStorage_RenameAndPortChangeMigrateTogether(t *testing.T) {
	t.Setenv("TRIM_PKGETC", t.TempDir())
	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatal(err)
	}
	oldKey := ContainerKey("web:1|@old-name;80/tcp:8080")
	newKey := ContainerKey("web:2|@new-name;80/tcp:9090")
	if err := storage.Set(&StoredConfig{
		Key: oldKey, AppName: "watchcow.web", Revision: "old",
		Entries: []StoredEntry{{Port: "8080", Protocol: "http", Path: "/", UIType: "url"}},
	}); err != nil {
		t.Fatal(err)
	}

	migrated, err := storage.MigrateLegacy(string(oldKey), string(newKey))
	if err != nil {
		t.Fatal(err)
	}
	if migrated == nil || !migrated.Pending || migrated.Entries[0].Port != "9090" {
		t.Fatalf("combined rename and port migration was incomplete: %+v", migrated)
	}
}

func TestDashboardStorage_MarkFailedUsesExactEmptyRevision(t *testing.T) {
	t.Setenv("TRIM_PKGETC", t.TempDir())
	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatal(err)
	}
	key := ContainerKey("web|@web")
	if err := storage.Set(&StoredConfig{Key: key, AppName: "watchcow.web", Revision: "new", Pending: true}); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkFailed(string(key), "", "old failure"); err != nil {
		t.Fatal(err)
	}
	if config := storage.Get(key); config.LastError != "" {
		t.Fatalf("empty stale revision changed current config: %+v", config)
	}
}

func TestDashboardStorage_RenameMigrationRequiresApply(t *testing.T) {
	t.Setenv("TRIM_PKGETC", t.TempDir())
	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatal(err)
	}
	oldKey := ContainerKey("web:latest|@old-name;80/tcp:8080")
	newKey := ContainerKey("web:latest|@new-name;80/tcp:8080")
	if err := storage.Set(&StoredConfig{Key: oldKey, AppName: "watchcow.web", Revision: "same"}); err != nil {
		t.Fatal(err)
	}

	migrated, err := storage.MigrateLegacy(string(oldKey), string(newKey))
	if err != nil {
		t.Fatal(err)
	}
	if migrated == nil || !migrated.Pending {
		t.Fatalf("rename did not require package regeneration: %+v", migrated)
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

func TestDashboardStorage_WriteFailureRollsBackMutations(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*DashboardStorage)
	}{
		{
			name: "create",
			inject: func(storage *DashboardStorage) {
				storage.createFile = func(string) (storageFile, error) {
					return nil, errInjectedStorageWrite
				}
			},
		},
		{
			name: "encode",
			inject: func(storage *DashboardStorage) {
				storage.createFile = func(string) (storageFile, error) {
					return &injectedStorageFile{writeErr: errInjectedStorageWrite}, nil
				}
			},
		},
		{
			name: "sync",
			inject: func(storage *DashboardStorage) {
				storage.createFile = func(string) (storageFile, error) {
					return &injectedStorageFile{syncErr: errInjectedStorageWrite}, nil
				}
			},
		},
		{
			name: "rename",
			inject: func(storage *DashboardStorage) {
				storage.renameFile = func(string, string) error {
					return errInjectedStorageWrite
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("TRIM_PKGETC", dataDir)
			storage, err := NewDashboardStorage()
			if err != nil {
				t.Fatal(err)
			}

			primaryKey := ContainerKey("web:latest|@web;8080/tcp:18080")
			obsoleteKey := ContainerKey("web:latest|8080:18080")
			newKey := ContainerKey("api:latest|@api;9000/tcp:19000")
			migratedKey := ContainerKey("web:latest|@renamed;8080/tcp:18080")
			if err := storage.Set(&StoredConfig{
				Key:         primaryKey,
				AppName:     "watchcow.web",
				DisplayName: "Original",
				Revision:    "revision-a",
				Pending:     true,
				Entries:     []StoredEntry{{FileTypes: []string{"html"}}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := storage.Set(&StoredConfig{Key: obsoleteKey, AppName: "watchcow.legacy"}); err != nil {
				t.Fatal(err)
			}

			assertUnchanged := func(t *testing.T) {
				t.Helper()
				primary := storage.Get(primaryKey)
				if primary == nil || primary.DisplayName != "Original" || !primary.Pending ||
					len(primary.Entries) != 1 || len(primary.Entries[0].FileTypes) != 1 || primary.Entries[0].FileTypes[0] != "html" {
					t.Fatalf("primary config changed after failed write: %+v", primary)
				}
				if obsolete := storage.Get(obsoleteKey); obsolete == nil || obsolete.AppName != "watchcow.legacy" {
					t.Fatalf("obsolete config changed after failed write: %+v", obsolete)
				}
				if storage.Has(newKey) || storage.Has(migratedKey) {
					t.Fatalf("failed write added a new key: new=%+v migrated=%+v", storage.Get(newKey), storage.Get(migratedKey))
				}
			}

			tt.inject(storage)

			if err := storage.Set(&StoredConfig{Key: primaryKey, DisplayName: "Changed"}); err == nil {
				t.Fatal("Set() succeeded with an injected storage failure")
			}
			assertUnchanged(t)

			if err := storage.Replace([]ContainerKey{obsoleteKey}, &StoredConfig{Key: newKey, AppName: "watchcow.api"}); err == nil {
				t.Fatal("Replace() succeeded with an injected storage failure")
			}
			assertUnchanged(t)

			if err := storage.Delete(primaryKey); err == nil {
				t.Fatal("Delete() succeeded with an injected storage failure")
			}
			assertUnchanged(t)

			if err := storage.DeleteMany([]ContainerKey{primaryKey, obsoleteKey}); err == nil {
				t.Fatal("DeleteMany() succeeded with an injected storage failure")
			}
			assertUnchanged(t)

			if err := storage.MarkApplied(string(primaryKey), "revision-a"); err == nil {
				t.Fatal("MarkApplied() succeeded with an injected storage failure")
			}
			assertUnchanged(t)

			if migrated, err := storage.MigrateLegacy(string(obsoleteKey), string(migratedKey)); err == nil || migrated != nil {
				t.Fatalf("MigrateLegacy() = %+v, %v; want persistence error", migrated, err)
			}
			assertUnchanged(t)

			reloaded, err := NewDashboardStorage()
			if err != nil {
				t.Fatal(err)
			}
			if primary := reloaded.Get(primaryKey); primary == nil || primary.DisplayName != "Original" || !primary.Pending {
				t.Fatalf("persisted primary config changed after failed writes: %+v", primary)
			}
			if obsolete := reloaded.Get(obsoleteKey); obsolete == nil || obsolete.AppName != "watchcow.legacy" {
				t.Fatalf("persisted obsolete config changed after failed writes: %+v", obsolete)
			}
			if reloaded.Has(newKey) || reloaded.Has(migratedKey) {
				t.Fatal("failed write changed persisted keys")
			}
			if _, err := os.Stat(storage.filePath + ".tmp"); !os.IsNotExist(err) {
				t.Fatalf("failed write left a recoverable temporary file: %v", err)
			}
		})
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
