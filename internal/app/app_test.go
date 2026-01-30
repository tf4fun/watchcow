package app

import (
	"testing"
)

func TestApp_GetEntry(t *testing.T) {
	app := &App{
		AppName: "test.app",
		Entries: []Entry{
			{Name: "", Title: "Default"},
			{Name: "admin", Title: "Admin Panel"},
			{Name: "api", Title: "API Docs"},
		},
	}

	tests := []struct {
		name     string
		expected string
	}{
		{"", "Default"},
		{"admin", "Admin Panel"},
		{"api", "API Docs"},
		{"nonexistent", ""},
	}

	for _, tt := range tests {
		entry := app.GetEntry(tt.name)
		if tt.expected == "" {
			if entry != nil {
				t.Errorf("GetEntry(%q) should return nil", tt.name)
			}
		} else {
			if entry == nil {
				t.Errorf("GetEntry(%q) should not return nil", tt.name)
			} else if entry.Title != tt.expected {
				t.Errorf("GetEntry(%q).Title = %q, want %q", tt.name, entry.Title, tt.expected)
			}
		}
	}
}

func TestApp_GetDefaultEntry(t *testing.T) {
	t.Run("with default entry", func(t *testing.T) {
		app := &App{
			Entries: []Entry{
				{Name: "admin", Title: "Admin"},
				{Name: "", Title: "Default"},
			},
		}
		entry := app.GetDefaultEntry()
		if entry == nil || entry.Title != "Default" {
			t.Errorf("expected default entry, got %v", entry)
		}
	})

	t.Run("without default entry", func(t *testing.T) {
		app := &App{
			Entries: []Entry{
				{Name: "admin", Title: "Admin"},
				{Name: "api", Title: "API"},
			},
		}
		entry := app.GetDefaultEntry()
		if entry == nil || entry.Title != "Admin" {
			t.Errorf("expected first entry, got %v", entry)
		}
	})

	t.Run("no entries", func(t *testing.T) {
		app := &App{}
		entry := app.GetDefaultEntry()
		if entry != nil {
			t.Errorf("expected nil, got %v", entry)
		}
	})
}

func TestApp_HasRedirect(t *testing.T) {
	t.Run("with redirect", func(t *testing.T) {
		app := &App{
			Entries: []Entry{
				{Name: "", Redirect: "https://example.com", Port: "8080"},
			},
		}
		if !app.HasRedirect() {
			t.Error("expected HasRedirect() to return true")
		}
	})

	t.Run("without redirect", func(t *testing.T) {
		app := &App{
			Entries: []Entry{
				{Name: "", Port: "8080"},
			},
		}
		if app.HasRedirect() {
			t.Error("expected HasRedirect() to return false")
		}
	})
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	registry := NewRegistry()

	app := &App{
		AppName:     "test.app",
		DisplayName: "Test App",
		ContainerID: "abc123",
		Status:      StatusRunning,
	}

	registry.Register(app)

	// Get by name
	got := registry.Get("test.app")
	if got == nil {
		t.Fatal("expected to get app by name")
	}
	if got.DisplayName != "Test App" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Test App")
	}

	// Get by container ID
	got = registry.GetByContainerID("abc123")
	if got == nil {
		t.Fatal("expected to get app by container ID")
	}
	if got.AppName != "test.app" {
		t.Errorf("AppName = %q, want %q", got.AppName, "test.app")
	}

	// Get nonexistent
	if registry.Get("nonexistent") != nil {
		t.Error("expected nil for nonexistent app")
	}
}

func TestRegistry_Unregister(t *testing.T) {
	registry := NewRegistry()

	app := &App{AppName: "test.app"}
	registry.Register(app)

	if registry.Get("test.app") == nil {
		t.Fatal("app should exist before unregister")
	}

	registry.Unregister("test.app")

	if registry.Get("test.app") != nil {
		t.Error("app should not exist after unregister")
	}
}

func TestRegistry_List(t *testing.T) {
	registry := NewRegistry()

	registry.Register(&App{AppName: "app1"})
	registry.Register(&App{AppName: "app2"})
	registry.Register(&App{AppName: "app3"})

	apps := registry.List()
	if len(apps) != 3 {
		t.Errorf("expected 3 apps, got %d", len(apps))
	}

	// Check all apps are present
	names := make(map[string]bool)
	for _, app := range apps {
		names[app.AppName] = true
	}
	for _, name := range []string{"app1", "app2", "app3"} {
		if !names[name] {
			t.Errorf("missing app %q in list", name)
		}
	}
}

func TestRegistry_UpdateStatus(t *testing.T) {
	registry := NewRegistry()

	app := &App{AppName: "test.app", Status: StatusPending}
	registry.Register(app)

	if !registry.UpdateStatus("test.app", StatusRunning) {
		t.Error("UpdateStatus should return true for existing app")
	}

	got := registry.Get("test.app")
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, StatusRunning)
	}

	if registry.UpdateStatus("nonexistent", StatusRunning) {
		t.Error("UpdateStatus should return false for nonexistent app")
	}
}
