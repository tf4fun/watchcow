package app

import (
	"strings"
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

func TestEntry_GetRedirectConfigPreservesBehavior(t *testing.T) {
	entry := Entry{Redirect: "https://example.com", Port: "8080", ForceExternal: true}
	config := entry.GetRedirectConfig()
	if config == nil {
		t.Fatal("expected redirect config")
	}
	if config.Host != entry.Redirect || config.Port != entry.Port || config.ForceExternal != entry.ForceExternal {
		t.Errorf("redirect config = %+v, want fields from %+v", config, entry)
	}
}

func TestDefaultAppName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "simple", in: "nginx", want: "watchcow.nginx"},
		{name: "leading slash", in: "/my_app", want: "watchcow.my-app"},
		{name: "mixed case and punctuation", in: "My_App.1", want: "watchcow.my-app1"},
		{name: "empty after sanitize", in: "...", want: "watchcow.app"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultAppName(tt.in); got != tt.want {
				t.Errorf("DefaultAppName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestGeneratedAppNameBoundsLongAutomaticNames(t *testing.T) {
	nameA := GeneratedAppName(strings.Repeat("a", 140), "18080")
	nameB := GeneratedAppName(strings.Repeat("a", 139)+"b", "18080")
	if len(nameA) != MaxAppNameLength || len(nameB) != MaxAppNameLength {
		t.Fatalf("generated names exceed bound: %d, %d", len(nameA), len(nameB))
	}
	if nameA == nameB {
		t.Fatalf("different long names collided: %q", nameA)
	}
	if nameA != GeneratedAppName(strings.Repeat("a", 140), "18080") {
		t.Error("generated name is not deterministic")
	}
}

func TestBoundGeneratedAppNamePreservesOriginalIdentity(t *testing.T) {
	original := "watchcow.beecount-beecount-cloud-1.8869"
	got := BoundGeneratedAppName(original)
	if len(got) != MaxAppNameLength {
		t.Fatalf("BoundGeneratedAppName() length = %d, want %d: %q", len(got), MaxAppNameLength, got)
	}
	if got != BoundGeneratedAppName(original) {
		t.Fatalf("BoundGeneratedAppName() is not deterministic: %q", got)
	}
	if got == BoundGeneratedAppName("watchcow.beecount-beecount-cloud-1.9090") {
		t.Fatalf("different original identities collided: %q", got)
	}
}

func TestDashboardAppNameBoundsIssue39ContainerNames(t *testing.T) {
	tests := []struct {
		containerName string
		hostPort      string
	}{
		{containerName: "beecount-beecount-cloud-1", hostPort: "8869"},
		{containerName: "llonebot-llonebot-1", hostPort: "3080"},
	}

	for _, tt := range tests {
		t.Run(tt.containerName, func(t *testing.T) {
			got := DashboardAppName(tt.containerName, tt.hostPort)
			if len(got) != MaxAppNameLength {
				t.Fatalf("DashboardAppName() length = %d, want %d: %q", len(got), MaxAppNameLength, got)
			}
			if got != DashboardAppName(tt.containerName, tt.hostPort) {
				t.Fatalf("DashboardAppName() is not deterministic: %q", got)
			}
		})
	}
}

func TestDashboardAppNameDisambiguatesSanitizedNames(t *testing.T) {
	plain := DashboardAppName("foo-bar", "8080")
	sanitized := DashboardAppName("foo_bar", "8080")
	if plain != "watchcow.foo-bar.8080" {
		t.Fatalf("plain name changed: %q", plain)
	}
	if sanitized == plain {
		t.Fatalf("sanitized names collided: %q", sanitized)
	}
	if sanitized != DashboardAppName("foo_bar", "8080") {
		t.Fatalf("dashboard name is not deterministic: %q", sanitized)
	}
	if upper := DashboardAppName("Foo", "8080"); upper == DashboardAppName("foo", "8080") {
		t.Fatalf("case-normalized names collided: %q", upper)
	}
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
