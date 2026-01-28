package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"watchcow/internal/docker"
)

// mockContainerLister implements ContainerLister for testing
type mockContainerLister struct {
	containers []RawContainerInfo
}

func (m *mockContainerLister) ListAllContainers(ctx context.Context) ([]RawContainerInfo, error) {
	return m.containers, nil
}

// mockInstallTrigger implements InstallTrigger for testing
type mockInstallTrigger struct {
	triggerCalls []triggerCall
}

type triggerCall struct {
	containerID  string
	storedConfig *docker.StoredConfig
}

func (m *mockInstallTrigger) TriggerInstall(containerID string, storedConfig *docker.StoredConfig) {
	m.triggerCalls = append(m.triggerCalls, triggerCall{containerID, storedConfig})
}

func newMockInstallTrigger() *mockInstallTrigger {
	return &mockInstallTrigger{
		triggerCalls: make([]triggerCall, 0),
	}
}

// setChiURLParam sets chi URL params on a request for testing
func setChiURLParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func setupTestHandler(t *testing.T) (*DashboardHandler, *DashboardStorage, *mockInstallTrigger) {
	t.Helper()

	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	t.Cleanup(func() { os.Unsetenv("TRIM_PKGETC") })

	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	lister := &mockContainerLister{
		containers: []RawContainerInfo{
			{
				ID:    "abc123",
				Name:  "nginx",
				Image: "nginx:alpine",
				State: "running",
				Ports: map[string]string{"80": "8080"},
				Labels: map[string]string{},
			},
			{
				ID:    "def456",
				Name:  "redis",
				Image: "redis:latest",
				State: "running",
				Ports: map[string]string{"6379": "6379"},
				Labels: map[string]string{
					"watchcow.enable": "true",
				},
			},
		},
	}

	trigger := newMockInstallTrigger()

	handler, err := NewDashboardHandler(storage, lister, trigger)
	if err != nil {
		t.Fatalf("NewDashboardHandler() error = %v", err)
	}

	return handler, storage, trigger
}

func TestDashboardHandler_Dashboard(t *testing.T) {
	handler, _, _ := setupTestHandler(t)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	handler.handleDashboard(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "WatchCow 控制面板") {
		t.Error("response should contain dashboard title")
	}
	if !strings.Contains(body, "WatchCow") {
		t.Error("response should contain WatchCow branding")
	}
	// Container list is loaded via HTMX into #main-content
	if !strings.Contains(body, `hx-get="containers"`) {
		t.Error("response should contain HTMX container list loader")
	}
	if !strings.Contains(body, `id="main-content"`) {
		t.Error("response should contain main-content div")
	}
}

func TestDashboardHandler_ContainerList(t *testing.T) {
	handler, _, _ := setupTestHandler(t)

	req := httptest.NewRequest("GET", "/containers", nil)
	w := httptest.NewRecorder()

	handler.handleContainerList(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "nginx") {
		t.Error("response should contain container 'nginx'")
	}
	if !strings.Contains(body, "redis") {
		t.Error("response should contain container 'redis'")
	}
}

func TestDashboardHandler_ContainerForm(t *testing.T) {
	handler, _, _ := setupTestHandler(t)

	containerID := "abc123"
	req := httptest.NewRequest("GET", "/containers/"+containerID, nil)
	req = setChiURLParam(req, "id", containerID)
	w := httptest.NewRecorder()

	handler.handleContainerForm(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "nginx") {
		t.Error("response should contain container name")
	}
}

func TestDashboardHandler_ContainerSave(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)

	containerID := "abc123"
	key := "nginx:alpine|80:8080"
	form := url.Values{
		"appname":        {"watchcow.nginx"},
		"display_name":   {"Nginx Test"},
		"description":    {"Web server test"},
		"version":        {"1.0.0"},
		"maintainer":     {"Tester"},
		"entry_title":    {"Nginx"},
		"entry_protocol": {"http"},
		"entry_port":     {"80"},
		"entry_path":     {"/"},
		"entry_ui_type":  {"url"},
	}

	req := httptest.NewRequest("POST", "/containers/"+containerID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", containerID)
	w := httptest.NewRecorder()

	handler.handleContainerSave(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
		t.Errorf("body: %s", w.Body.String())
	}

	// Verify config was saved
	saved := storage.Get(ContainerKey(key))
	if saved == nil {
		t.Fatal("config should be saved")
	}
	if saved.AppName != "watchcow.nginx" {
		t.Errorf("AppName = %q, want %q", saved.AppName, "watchcow.nginx")
	}
	if saved.DisplayName != "Nginx Test" {
		t.Errorf("DisplayName = %q, want %q", saved.DisplayName, "Nginx Test")
	}

	// Verify TriggerInstall was called
	if len(trigger.triggerCalls) != 1 {
		t.Errorf("expected 1 TriggerInstall call, got %d", len(trigger.triggerCalls))
	} else {
		if trigger.triggerCalls[0].containerID != "abc123" {
			t.Errorf("TriggerInstall containerID = %q, want %q", trigger.triggerCalls[0].containerID, "abc123")
		}
		if trigger.triggerCalls[0].storedConfig.AppName != "watchcow.nginx" {
			t.Errorf("TriggerInstall storedConfig.AppName = %q, want %q", trigger.triggerCalls[0].storedConfig.AppName, "watchcow.nginx")
		}
	}
}

func TestDashboardHandler_ContainerSave_LabelConfigured(t *testing.T) {
	handler, _, _ := setupTestHandler(t)

	// redis has watchcow.enable=true label
	containerID := "def456"
	form := url.Values{
		"appname":      {"watchcow.redis"},
		"display_name": {"Redis"},
	}

	req := httptest.NewRequest("POST", "/containers/"+containerID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", containerID)
	w := httptest.NewRecorder()

	handler.handleContainerSave(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected status 403 for label-configured container, got %d", resp.StatusCode)
	}
}

func TestDashboardHandler_ContainerDelete(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)

	containerID := "abc123"
	key := ContainerKey("nginx:alpine|80:8080")

	// First save a config
	storage.Set(&StoredConfig{Key: key, AppName: "test"})
	if !storage.Has(key) {
		t.Fatal("config should exist before delete")
	}

	req := httptest.NewRequest("DELETE", "/containers/"+containerID, nil)
	req = setChiURLParam(req, "id", containerID)
	w := httptest.NewRecorder()

	handler.handleContainerDelete(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	// Verify config was deleted
	if storage.Has(key) {
		t.Error("config should be deleted")
	}
}

func TestDashboardHandler_ConvertToDockerConfig(t *testing.T) {
	handler, _, _ := setupTestHandler(t)

	config := &StoredConfig{
		Key:         "test|80:8080",
		AppName:     "watchcow.test",
		DisplayName: "Test App",
		Description: "Test description",
		Version:     "1.0.0",
		Maintainer:  "Tester",
		IconBase64:  "icon-base64",
		Entries: []StoredEntry{
			{
				Name:       "",
				Title:      "Default",
				Protocol:   "http",
				Port:       "80",
				Path:       "/",
				UIType:     "url",
				AllUsers:   true,
				FileTypes:  []string{".html"},
				NoDisplay:  false,
				Redirect:   "https://example.com",
				IconBase64: "entry-icon",
			},
		},
	}

	dockerCfg := handler.convertToDockerConfig(config)

	if dockerCfg.AppName != "watchcow.test" {
		t.Errorf("AppName = %q, want %q", dockerCfg.AppName, "watchcow.test")
	}
	if dockerCfg.DisplayName != "Test App" {
		t.Errorf("DisplayName = %q, want %q", dockerCfg.DisplayName, "Test App")
	}
	if dockerCfg.Description != "Test description" {
		t.Errorf("Description = %q, want %q", dockerCfg.Description, "Test description")
	}
	if dockerCfg.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", dockerCfg.Version, "1.0.0")
	}
	if dockerCfg.Maintainer != "Tester" {
		t.Errorf("Maintainer = %q, want %q", dockerCfg.Maintainer, "Tester")
	}
	if dockerCfg.IconBase64 != "icon-base64" {
		t.Errorf("IconBase64 = %q, want %q", dockerCfg.IconBase64, "icon-base64")
	}

	if len(dockerCfg.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(dockerCfg.Entries))
	}

	entry := dockerCfg.Entries[0]
	if entry.Name != "" {
		t.Errorf("Entry.Name = %q, want %q", entry.Name, "")
	}
	if entry.Title != "Default" {
		t.Errorf("Entry.Title = %q, want %q", entry.Title, "Default")
	}
	if entry.Protocol != "http" {
		t.Errorf("Entry.Protocol = %q, want %q", entry.Protocol, "http")
	}
	if entry.Port != "80" {
		t.Errorf("Entry.Port = %q, want %q", entry.Port, "80")
	}
	if entry.Path != "/" {
		t.Errorf("Entry.Path = %q, want %q", entry.Path, "/")
	}
	if entry.UIType != "url" {
		t.Errorf("Entry.UIType = %q, want %q", entry.UIType, "url")
	}
	if !entry.AllUsers {
		t.Error("Entry.AllUsers should be true")
	}
	if len(entry.FileTypes) != 1 || entry.FileTypes[0] != ".html" {
		t.Errorf("Entry.FileTypes = %v, want [.html]", entry.FileTypes)
	}
	if entry.NoDisplay {
		t.Error("Entry.NoDisplay should be false")
	}
	if entry.Redirect != "https://example.com" {
		t.Errorf("Entry.Redirect = %q, want %q", entry.Redirect, "https://example.com")
	}
	if entry.IconBase64 != "entry-icon" {
		t.Errorf("Entry.IconBase64 = %q, want %q", entry.IconBase64, "entry-icon")
	}
}

func TestDashboardHandler_SaveTriggersInstall(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)

	// Pre-condition: no trigger calls
	if len(trigger.triggerCalls) != 0 {
		t.Fatal("should start with no trigger calls")
	}

	containerID := "abc123"
	key := "nginx:alpine|80:8080"
	form := url.Values{
		"appname":        {"watchcow.nginx"},
		"display_name":   {"Nginx"},
		"entry_protocol": {"http"},
		"entry_port":     {"80"},
		"entry_path":     {"/"},
	}

	req := httptest.NewRequest("POST", "/containers/"+containerID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", containerID)
	w := httptest.NewRecorder()

	handler.handleContainerSave(w, req)

	// Verify config was saved
	if !storage.Has(ContainerKey(key)) {
		t.Fatal("config should be saved")
	}

	// Verify trigger was called
	if len(trigger.triggerCalls) != 1 {
		t.Fatalf("expected 1 trigger call, got %d", len(trigger.triggerCalls))
	}

	call := trigger.triggerCalls[0]
	if call.containerID != "abc123" {
		t.Errorf("containerID = %q, want %q", call.containerID, "abc123")
	}
	if call.storedConfig == nil {
		t.Fatal("storedConfig should not be nil")
	}
	if call.storedConfig.AppName != "watchcow.nginx" {
		t.Errorf("storedConfig.AppName = %q, want %q", call.storedConfig.AppName, "watchcow.nginx")
	}
}

func TestDashboardHandler_NilTrigger(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	defer os.Unsetenv("TRIM_PKGETC")

	storage, _ := NewDashboardStorage()
	lister := &mockContainerLister{
		containers: []RawContainerInfo{
			{
				ID:     "abc123",
				Name:   "nginx",
				Image:  "nginx:alpine",
				State:  "running",
				Ports:  map[string]string{"80": "8080"},
				Labels: map[string]string{},
			},
		},
	}

	// Pass nil trigger
	handler, err := NewDashboardHandler(storage, lister, nil)
	if err != nil {
		t.Fatalf("NewDashboardHandler() error = %v", err)
	}

	containerID := "abc123"
	key := "nginx:alpine|80:8080"
	form := url.Values{
		"appname":      {"watchcow.nginx"},
		"display_name": {"Nginx"},
	}

	req := httptest.NewRequest("POST", "/containers/"+containerID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", containerID)
	w := httptest.NewRecorder()

	// Should not panic with nil trigger
	handler.handleContainerSave(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	// Config should be saved
	if !storage.Has(ContainerKey(key)) {
		t.Fatal("config should be saved with nil trigger")
	}
}
