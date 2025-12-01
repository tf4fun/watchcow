package fpkgen

import (
	"encoding/json"
	"testing"
)

// TestParseEntries_DefaultEntry tests parsing of default entry (watchcow.service_port style)
func TestParseEntries_DefaultEntry(t *testing.T) {
	labels := map[string]string{
		"watchcow.enable":       "true",
		"watchcow.service_port": "8080",
		"watchcow.protocol":     "https",
		"watchcow.path":         "/app",
		"watchcow.ui_type":      "iframe",
		"watchcow.all_users":    "false",
		"watchcow.title":        "My App",
		"watchcow.icon":         "https://example.com/icon.png",
	}

	entries := parseEntries(labels, "Test App", "https://default.icon/icon.png", "9090")

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Name != "" {
		t.Errorf("expected empty name for default entry, got %q", e.Name)
	}
	if e.Title != "My App" {
		t.Errorf("expected title 'My App', got %q", e.Title)
	}
	if e.Port != "8080" {
		t.Errorf("expected port '8080', got %q", e.Port)
	}
	if e.Protocol != "https" {
		t.Errorf("expected protocol 'https', got %q", e.Protocol)
	}
	if e.Path != "/app" {
		t.Errorf("expected path '/app', got %q", e.Path)
	}
	if e.UIType != "iframe" {
		t.Errorf("expected ui_type 'iframe', got %q", e.UIType)
	}
	if e.AllUsers != false {
		t.Errorf("expected all_users false, got true")
	}
	if e.Icon != "https://example.com/icon.png" {
		t.Errorf("expected custom icon, got %q", e.Icon)
	}
}

// TestParseEntries_DefaultEntryDefaults tests default values for default entry
func TestParseEntries_DefaultEntryDefaults(t *testing.T) {
	labels := map[string]string{
		"watchcow.enable":       "true",
		"watchcow.service_port": "8080",
	}

	entries := parseEntries(labels, "Test App", "https://default.icon/icon.png", "9090")

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	// Check defaults
	if e.Title != "Test App" {
		t.Errorf("expected title from display_name 'Test App', got %q", e.Title)
	}
	if e.Protocol != "http" {
		t.Errorf("expected default protocol 'http', got %q", e.Protocol)
	}
	if e.Path != "/" {
		t.Errorf("expected default path '/', got %q", e.Path)
	}
	if e.UIType != "url" {
		t.Errorf("expected default ui_type 'url', got %q", e.UIType)
	}
	if e.AllUsers != true {
		t.Errorf("expected default all_users true, got false")
	}
	if e.Icon != "https://default.icon/icon.png" {
		t.Errorf("expected default icon, got %q", e.Icon)
	}
}

// TestParseEntries_NamedEntry tests parsing of named entry (watchcow.admin.service_port style)
func TestParseEntries_NamedEntry(t *testing.T) {
	labels := map[string]string{
		"watchcow.enable":             "true",
		"watchcow.admin.service_port": "8081",
		"watchcow.admin.protocol":     "http",
		"watchcow.admin.path":         "/admin",
		"watchcow.admin.ui_type":      "iframe",
		"watchcow.admin.all_users":    "false",
		"watchcow.admin.title":        "Admin Panel",
		"watchcow.admin.icon":         "https://example.com/admin-icon.png",
	}

	entries := parseEntries(labels, "Test App", "https://default.icon/icon.png", "9090")

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Name != "admin" {
		t.Errorf("expected name 'admin', got %q", e.Name)
	}
	if e.Title != "Admin Panel" {
		t.Errorf("expected title 'Admin Panel', got %q", e.Title)
	}
	if e.Port != "8081" {
		t.Errorf("expected port '8081', got %q", e.Port)
	}
	if e.AllUsers != false {
		t.Errorf("expected all_users false, got true")
	}
}

// TestParseEntries_NamedEntryDefaultTitle tests default title for named entry
func TestParseEntries_NamedEntryDefaultTitle(t *testing.T) {
	labels := map[string]string{
		"watchcow.enable":             "true",
		"watchcow.admin.service_port": "8081",
	}

	entries := parseEntries(labels, "Test App", "https://default.icon/icon.png", "9090")

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	// Named entry should have title "display_name - entry_name"
	expectedTitle := "Test App - admin"
	if e.Title != expectedTitle {
		t.Errorf("expected title %q, got %q", expectedTitle, e.Title)
	}
}

// TestParseEntries_MultipleEntries tests multiple entries (default + named)
func TestParseEntries_MultipleEntries(t *testing.T) {
	labels := map[string]string{
		"watchcow.enable": "true",
		// Default entry
		"watchcow.service_port": "8080",
		"watchcow.title":        "Main App",
		// Admin entry
		"watchcow.admin.service_port": "8081",
		"watchcow.admin.title":        "Admin Panel",
		"watchcow.admin.all_users":    "false",
		// API entry
		"watchcow.api.service_port": "8082",
		"watchcow.api.path":         "/api",
		"watchcow.api.no_display":   "true",
	}

	entries := parseEntries(labels, "Test App", "https://default.icon/icon.png", "9090")

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Find each entry
	var defaultEntry, adminEntry, apiEntry *Entry
	for i := range entries {
		switch entries[i].Name {
		case "":
			defaultEntry = &entries[i]
		case "admin":
			adminEntry = &entries[i]
		case "api":
			apiEntry = &entries[i]
		}
	}

	if defaultEntry == nil {
		t.Error("default entry not found")
	} else {
		if defaultEntry.Title != "Main App" {
			t.Errorf("default entry title: expected 'Main App', got %q", defaultEntry.Title)
		}
		if defaultEntry.Port != "8080" {
			t.Errorf("default entry port: expected '8080', got %q", defaultEntry.Port)
		}
	}

	if adminEntry == nil {
		t.Error("admin entry not found")
	} else {
		if adminEntry.Title != "Admin Panel" {
			t.Errorf("admin entry title: expected 'Admin Panel', got %q", adminEntry.Title)
		}
		if adminEntry.AllUsers != false {
			t.Error("admin entry should have all_users=false")
		}
	}

	if apiEntry == nil {
		t.Error("api entry not found")
	} else {
		if apiEntry.Path != "/api" {
			t.Errorf("api entry path: expected '/api', got %q", apiEntry.Path)
		}
		if apiEntry.NoDisplay != true {
			t.Error("api entry should have no_display=true")
		}
		// Default title for named entry
		if apiEntry.Title != "Test App - api" {
			t.Errorf("api entry title: expected 'Test App - api', got %q", apiEntry.Title)
		}
	}
}

// TestParseEntries_OnlyNamedEntries tests scenario with only named entries (no default)
func TestParseEntries_OnlyNamedEntries(t *testing.T) {
	labels := map[string]string{
		"watchcow.enable": "true",
		// Only named entries, no default
		"watchcow.main.service_port":  "8080",
		"watchcow.main.title":         "Main",
		"watchcow.admin.service_port": "8081",
		"watchcow.admin.title":        "Admin",
	}

	entries := parseEntries(labels, "Test App", "https://default.icon/icon.png", "9090")

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Verify no default entry exists
	for _, e := range entries {
		if e.Name == "" {
			t.Error("should not have a default entry")
		}
	}
}

// TestParseEntries_FileTypes tests file_types parsing
func TestParseEntries_FileTypes(t *testing.T) {
	labels := map[string]string{
		"watchcow.enable":            "true",
		"watchcow.editor.path":       "/edit",
		"watchcow.editor.file_types": "txt, md, json, xml",
		"watchcow.editor.no_display": "true",
	}

	entries := parseEntries(labels, "Editor", "https://default.icon/icon.png", "8080")

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	expectedTypes := []string{"txt", "md", "json", "xml"}
	if len(e.FileTypes) != len(expectedTypes) {
		t.Fatalf("expected %d file types, got %d", len(expectedTypes), len(e.FileTypes))
	}
	for i, ft := range expectedTypes {
		if e.FileTypes[i] != ft {
			t.Errorf("file type %d: expected %q, got %q", i, ft, e.FileTypes[i])
		}
	}
}

// TestParseEntries_Control tests control settings parsing
func TestParseEntries_Control(t *testing.T) {
	labels := map[string]string{
		"watchcow.enable":                    "true",
		"watchcow.service_port":              "8080",
		"watchcow.control.access_perm":       "readonly",
		"watchcow.control.port_perm":         "hidden",
		"watchcow.control.path_perm":         "editable",
		"watchcow.admin.service_port":        "8081",
		"watchcow.admin.control.access_perm": "editable",
	}

	entries := parseEntries(labels, "Test App", "https://default.icon/icon.png", "9090")

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Find default entry
	var defaultEntry, adminEntry *Entry
	for i := range entries {
		if entries[i].Name == "" {
			defaultEntry = &entries[i]
		} else if entries[i].Name == "admin" {
			adminEntry = &entries[i]
		}
	}

	if defaultEntry == nil || defaultEntry.Control == nil {
		t.Fatal("default entry or its control is nil")
	}
	if defaultEntry.Control.AccessPerm != "readonly" {
		t.Errorf("default entry access_perm: expected 'readonly', got %q", defaultEntry.Control.AccessPerm)
	}
	if defaultEntry.Control.PortPerm != "hidden" {
		t.Errorf("default entry port_perm: expected 'hidden', got %q", defaultEntry.Control.PortPerm)
	}
	if defaultEntry.Control.PathPerm != "editable" {
		t.Errorf("default entry path_perm: expected 'editable', got %q", defaultEntry.Control.PathPerm)
	}

	if adminEntry == nil || adminEntry.Control == nil {
		t.Fatal("admin entry or its control is nil")
	}
	if adminEntry.Control.AccessPerm != "editable" {
		t.Errorf("admin entry access_perm: expected 'editable', got %q", adminEntry.Control.AccessPerm)
	}
}

// TestGenerateUIConfigJSON tests JSON generation for UI config
func TestGenerateUIConfigJSON(t *testing.T) {
	config := &AppConfig{
		AppName:     "watchcow.testapp",
		DisplayName: "Test App",
		Entries: []Entry{
			{
				Name:     "",
				Title:    "Test App",
				Protocol: "http",
				Port:     "8080",
				Path:     "/",
				UIType:   "url",
				AllUsers: true,
				Icon:     "https://example.com/icon.png",
			},
			{
				Name:      "admin",
				Title:     "Admin Panel",
				Protocol:  "https",
				Port:      "8081",
				Path:      "/admin",
				UIType:    "iframe",
				AllUsers:  false,
				Icon:      "https://example.com/admin-icon.png",
				FileTypes: []string{"txt", "md"},
				NoDisplay: true,
				Control: &EntryControl{
					AccessPerm: "readonly",
					PortPerm:   "hidden",
				},
			},
		},
	}

	data := NewTemplateData(config)
	jsonBytes, err := GenerateUIConfigJSON(data)
	if err != nil {
		t.Fatalf("GenerateUIConfigJSON failed: %v", err)
	}

	// Parse and verify JSON structure
	var result UIConfig
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		t.Fatalf("failed to parse generated JSON: %v", err)
	}

	// Check default entry
	defaultEntry, ok := result.URL["watchcow.testapp"]
	if !ok {
		t.Error("default entry 'watchcow.testapp' not found in JSON")
	} else {
		if defaultEntry.Title != "Test App" {
			t.Errorf("default entry title: expected 'Test App', got %q", defaultEntry.Title)
		}
		if defaultEntry.Port != "8080" {
			t.Errorf("default entry port: expected '8080', got %q", defaultEntry.Port)
		}
		if defaultEntry.AllUsers != true {
			t.Error("default entry should have allUsers=true")
		}
	}

	// Check admin entry
	adminEntry, ok := result.URL["watchcow.testapp.admin"]
	if !ok {
		t.Error("admin entry 'watchcow.testapp.admin' not found in JSON")
	} else {
		if adminEntry.Title != "Admin Panel" {
			t.Errorf("admin entry title: expected 'Admin Panel', got %q", adminEntry.Title)
		}
		if adminEntry.AllUsers != false {
			t.Error("admin entry should have allUsers=false")
		}
		if len(adminEntry.FileTypes) != 2 {
			t.Errorf("admin entry file types: expected 2, got %d", len(adminEntry.FileTypes))
		}
		if !adminEntry.NoDisplay {
			t.Error("admin entry should have noDisplay=true")
		}
		if adminEntry.Control == nil {
			t.Error("admin entry control should not be nil")
		} else {
			if adminEntry.Control.AccessPerm != "readonly" {
				t.Errorf("admin entry control.accessPerm: expected 'readonly', got %q", adminEntry.Control.AccessPerm)
			}
			if adminEntry.Control.PortPerm != "hidden" {
				t.Errorf("admin entry control.portPerm: expected 'hidden', got %q", adminEntry.Control.PortPerm)
			}
		}
	}
}

// TestTemplateData_DefaultLaunchEntry tests DefaultLaunchEntry is set correctly
func TestTemplateData_DefaultLaunchEntry(t *testing.T) {
	// Test with default entry (displayable)
	config1 := &AppConfig{
		AppName: "watchcow.app1",
		Entries: []Entry{
			{Name: "", Title: "Main", Port: "8080"},
			{Name: "admin", Title: "Admin", Port: "8081"},
		},
	}
	data1 := NewTemplateData(config1)
	if data1.DefaultLaunchEntry != "watchcow.app1" {
		t.Errorf("with default entry, DefaultLaunchEntry should be 'watchcow.app1', got %q", data1.DefaultLaunchEntry)
	}

	// Test with only named entries (no default)
	config2 := &AppConfig{
		AppName: "watchcow.app2",
		Entries: []Entry{
			{Name: "main", Title: "Main", Port: "8080"},
			{Name: "admin", Title: "Admin", Port: "8081"},
		},
	}
	data2 := NewTemplateData(config2)
	// Should use first displayable entry's full name
	if data2.DefaultLaunchEntry != "watchcow.app2.main" {
		t.Errorf("with only named entries, DefaultLaunchEntry should be 'watchcow.app2.main', got %q", data2.DefaultLaunchEntry)
	}

	// Test with first entry having NoDisplay=true
	config3 := &AppConfig{
		AppName: "watchcow.app3",
		Entries: []Entry{
			{Name: "api", Title: "API", Port: "8080", NoDisplay: true},
			{Name: "web", Title: "Web", Port: "8081", NoDisplay: false},
		},
	}
	data3 := NewTemplateData(config3)
	// Should skip first entry (NoDisplay=true) and use second entry
	if data3.DefaultLaunchEntry != "watchcow.app3.web" {
		t.Errorf("should skip NoDisplay entry, DefaultLaunchEntry should be 'watchcow.app3.web', got %q", data3.DefaultLaunchEntry)
	}

	// Test with all entries having NoDisplay=true
	config4 := &AppConfig{
		AppName: "watchcow.app4",
		Entries: []Entry{
			{Name: "api", Title: "API", Port: "8080", NoDisplay: true},
			{Name: "hook", Title: "Hook", Port: "8081", NoDisplay: true},
		},
	}
	data4 := NewTemplateData(config4)
	// Should be empty when no displayable entries
	if data4.DefaultLaunchEntry != "" {
		t.Errorf("with all NoDisplay=true, DefaultLaunchEntry should be empty, got %q", data4.DefaultLaunchEntry)
	}
}

// TestHasDefaultEntry tests hasDefaultEntry function
func TestHasDefaultEntry(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected bool
	}{
		{
			name:     "empty labels",
			labels:   map[string]string{},
			expected: false,
		},
		{
			name: "only named entry",
			labels: map[string]string{
				"watchcow.admin.service_port": "8080",
			},
			expected: false,
		},
		{
			name: "has service_port",
			labels: map[string]string{
				"watchcow.service_port": "8080",
			},
			expected: true,
		},
		{
			name: "has protocol",
			labels: map[string]string{
				"watchcow.protocol": "https",
			},
			expected: true,
		},
		{
			name: "has path",
			labels: map[string]string{
				"watchcow.path": "/app",
			},
			expected: true,
		},
		{
			name: "has title",
			labels: map[string]string{
				"watchcow.title": "My App",
			},
			expected: true,
		},
		{
			name: "has ui_type",
			labels: map[string]string{
				"watchcow.ui_type": "iframe",
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasDefaultEntry(tt.labels)
			if result != tt.expected {
				t.Errorf("hasDefaultEntry() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// TestIsEntryField tests isEntryField function
func TestIsEntryField(t *testing.T) {
	validFields := []string{
		"service_port", "protocol", "path", "ui_type",
		"all_users", "icon", "title", "file_types", "no_display",
		"control.access_perm", "control.port_perm", "control.path_perm",
	}

	for _, field := range validFields {
		if !isEntryField(field) {
			t.Errorf("isEntryField(%q) should return true", field)
		}
	}

	invalidFields := []string{
		"enable", "appname", "display_name", "desc", "version", "maintainer",
		"invalid", "random",
	}

	for _, field := range invalidFields {
		if isEntryField(field) {
			t.Errorf("isEntryField(%q) should return false", field)
		}
	}
}
