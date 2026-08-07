package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"watchcow/internal/docker"
)

// mockContainerLister implements ContainerLister for testing
type mockContainerLister struct {
	containers []docker.ContainerInfo
}

func (m *mockContainerLister) ListAllContainers(ctx context.Context) ([]docker.ContainerInfo, error) {
	return m.containers, nil
}

// mockAppTrigger implements AppTrigger for testing
type mockAppTrigger struct {
	triggerCalls []triggerCall
}

type triggerCall struct {
	containerID  string
	storedConfig *docker.StoredConfig
}

func (m *mockAppTrigger) TriggerInstall(containerID string, storedConfig *docker.StoredConfig) {
	m.triggerCalls = append(m.triggerCalls, triggerCall{containerID, storedConfig})
}

func (m *mockAppTrigger) TriggerUninstall(containerID, appName, configKey string) {
	// Track uninstall calls if needed
}

func newMockAppTrigger() *mockAppTrigger {
	return &mockAppTrigger{
		triggerCalls: make([]triggerCall, 0),
	}
}

// setChiURLParam sets chi URL params on a request for testing
func setChiURLParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func setupTestHandler(t *testing.T) (*DashboardHandler, *DashboardStorage, *mockAppTrigger) {
	t.Helper()

	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	t.Cleanup(func() { os.Unsetenv("TRIM_PKGETC") })

	storage, err := NewDashboardStorage()
	if err != nil {
		t.Fatalf("NewDashboardStorage() error = %v", err)
	}

	lister := &mockContainerLister{
		containers: []docker.ContainerInfo{
			{
				ID:     "abc123",
				Name:   "nginx",
				Image:  "nginx:alpine",
				State:  "running",
				Ports:  map[string]string{"80": "8080"},
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

	trigger := newMockAppTrigger()

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

func TestDashboardHandler_ContainerListUsesIdentityPortsForStorage(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	key := ContainerKey("dns-web:latest|53:5353,80:8080")
	if err := storage.Set(&StoredConfig{Key: key, AppName: "watchcow.dns-web"}); err != nil {
		t.Fatalf("storage.Set() error = %v", err)
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{{
		ID:            "dns123",
		Name:          "dns-web",
		Image:         "dns-web:latest",
		State:         "running",
		Ports:         map[string]string{"80": "8080"},
		IdentityPorts: map[string]string{"53/tcp": "1053", "53/udp": "5353", "80/tcp": "8080"},
		LegacyPortOptions: map[string][]string{
			"53": {"1053", "5353"},
			"80": {"8080"},
		},
	}}}

	containers, err := handler.listContainers(context.Background())
	if err != nil {
		t.Fatalf("listContainers() error = %v", err)
	}
	canonicalKey := ContainerKey("dns-web:latest|@dns-web;53/tcp:1053,53/udp:5353,80/tcp:8080")
	if len(containers) != 1 || !containers[0].HasStoredConfig ||
		containers[0].Key != canonicalKey || containers[0].ConfigKey != key {
		t.Errorf("identity port storage lookup failed: %+v", containers)
	}
	if !reflect.DeepEqual(containers[0].Ports, map[string]string{"80": "8080"}) {
		t.Errorf("dashboard exposed non-web ports: %v", containers[0].Ports)
	}
}

func TestDashboardHandler_DoesNotShareAmbiguousLegacyKey(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	legacyKey := ContainerKey("dns:latest|53:1053")
	if err := storage.Set(&StoredConfig{Key: legacyKey, AppName: "watchcow.dns"}); err != nil {
		t.Fatal(err)
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{
		{
			ID:                "tcp",
			Image:             "dns:latest",
			IdentityPorts:     map[string]string{"53/tcp": "1053"},
			LegacyPortOptions: map[string][]string{"53": {"1053"}},
			Labels:            map[string]string{},
		},
		{
			ID:                "udp",
			Image:             "dns:latest",
			IdentityPorts:     map[string]string{"53/udp": "1053"},
			LegacyPortOptions: map[string][]string{"53": {"1053"}},
			Labels:            map[string]string{},
		},
	}}

	containers, err := handler.listContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, container := range containers {
		if container.HasStoredConfig || container.Config != nil || len(container.LegacyConfigKeys) != 0 {
			t.Errorf("ambiguous legacy config was claimed by %s: %+v", container.ID, container)
		}
	}
	deleteReq := httptest.NewRequest("DELETE", "/containers/tcp", nil)
	deleteReq = setChiURLParam(deleteReq, "id", "tcp")
	deleteW := httptest.NewRecorder()
	handler.handleContainerDelete(deleteW, deleteReq)
	if deleteW.Code != http.StatusConflict || storage.Get(legacyKey) == nil {
		t.Errorf("shared legacy delete was not safely blocked: status=%d config=%+v", deleteW.Code, storage.Get(legacyKey))
	}
}

func TestDashboardHandler_DoesNotShareNoPortConfig(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	legacyKey := ContainerKey("redis:latest|")
	if err := storage.Set(&StoredConfig{Key: legacyKey, AppName: "watchcow.redis"}); err != nil {
		t.Fatal(err)
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{
		{ID: "a", Name: "redis-a", Image: "redis:latest", IdentityPorts: map[string]string{}, Labels: map[string]string{}},
		{ID: "b", Name: "redis-b", Image: "redis:latest", IdentityPorts: map[string]string{}, Labels: map[string]string{}},
	}}

	containers, err := handler.listContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if containers[0].Key == containers[1].Key {
		t.Fatalf("no-port canonical keys collided: %+v", containers)
	}
	for _, container := range containers {
		if container.HasStoredConfig || !container.LegacyConfigConflict {
			t.Errorf("shared no-port legacy config was claimed by %s: %+v", container.ID, container)
		}
	}
}

func TestDashboardHandler_DoesNotShareNamedCanonicalKey(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	keyA := ContainerKey("web:latest|@web-a;8080/tcp:18080")
	if err := storage.Set(&StoredConfig{Key: keyA, AppName: "watchcow.web-a"}); err != nil {
		t.Fatal(err)
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{
		{ID: "a", Name: "web-a", Image: "web:latest", IdentityPorts: map[string]string{"8080/tcp": "18080"}, LegacyPortOptions: map[string][]string{"8080": {"18080"}}, Labels: map[string]string{}},
		{ID: "b", Name: "web-b", Image: "web:latest", IdentityPorts: map[string]string{"8080/tcp": "18080"}, LegacyPortOptions: map[string][]string{"8080": {"18080"}}, Labels: map[string]string{}},
	}}

	containers, err := handler.listContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ContainerInfo{containers[0].ID: containers[0], containers[1].ID: containers[1]}
	if !byID["a"].HasStoredConfig || byID["a"].ConfigKey != keyA {
		t.Errorf("exact owner lost its config: %+v", byID["a"])
	}
	if byID["b"].HasStoredConfig || byID["b"].LegacyConfigConflict {
		t.Errorf("same-mapping container was not left independently configurable: %+v", byID["b"])
	}
}

func TestDashboardHandler_ExactOwnerDoesNotDeleteAbsentNamedConfig(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	keyA := ContainerKey("web:latest|@web-a;8080/tcp:18080")
	keyB := ContainerKey("web:latest|@web-b;8080/tcp:18080")
	for _, config := range []*StoredConfig{
		{Key: keyA, AppName: "watchcow.web-a"},
		{Key: keyB, AppName: "watchcow.web-b"},
	} {
		if err := storage.Set(config); err != nil {
			t.Fatal(err)
		}
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{{
		ID: "a", Name: "web-a", Image: "web:latest",
		IdentityPorts: map[string]string{"8080/tcp": "18080"}, LegacyPortOptions: map[string][]string{"8080": {"18080"}}, Labels: map[string]string{},
	}}}

	containers, err := handler.listContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || len(containers[0].LegacyConfigKeys) != 0 {
		t.Fatalf("absent named config was treated as an alias: %+v", containers)
	}
	req := httptest.NewRequest("DELETE", "/containers/a", nil)
	req = setChiURLParam(req, "id", "a")
	w := httptest.NewRecorder()
	handler.handleContainerDelete(w, req)
	if w.Code != http.StatusOK || storage.Get(keyA) != nil || storage.Get(keyB) == nil {
		t.Fatalf("delete crossed named owners: status=%d a=%+v b=%+v", w.Code, storage.Get(keyA), storage.Get(keyB))
	}
}

func TestDashboardHandler_RenamedContainerFindsUniqueNamedKey(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	oldKey := ContainerKey("web:latest|@old-name;8080/tcp:18080")
	if err := storage.Set(&StoredConfig{Key: oldKey, AppName: "watchcow.web"}); err != nil {
		t.Fatal(err)
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{{
		ID: "web", Name: "new-name", Image: "web:latest",
		IdentityPorts: map[string]string{"8080/tcp": "18080"}, LegacyPortOptions: map[string][]string{"8080": {"18080"}}, Labels: map[string]string{},
	}}}

	containers, err := handler.listContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || !containers[0].HasStoredConfig || containers[0].ConfigKey != oldKey {
		t.Fatalf("renamed container did not recover unique config: %+v", containers)
	}
}

func TestDashboardHandler_SaveRejectsMultipleLegacyCandidates(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	for _, key := range []ContainerKey{"dns:latest|53:1053", "dns:latest|53:2053"} {
		if err := storage.Set(&StoredConfig{Key: key, AppName: "watchcow.dns"}); err != nil {
			t.Fatal(err)
		}
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{{
		ID:                "dns",
		Name:              "dns",
		Image:             "dns:latest",
		IdentityPorts:     map[string]string{"53/tcp": "1053", "53/udp": "2053"},
		LegacyPortOptions: map[string][]string{"53": {"1053", "2053"}},
		Labels:            map[string]string{},
	}}}

	req := httptest.NewRequest("POST", "/containers/dns", strings.NewReader(url.Values{"display_name": {"DNS"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", "dns")
	w := httptest.NewRecorder()
	handler.handleContainerSave(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("save status = %d, want conflict", w.Code)
	}
	for _, key := range []ContainerKey{"dns:latest|53:1053", "dns:latest|53:2053"} {
		if storage.Get(key) == nil {
			t.Errorf("conflict save deleted legacy config %q", key)
		}
	}

	deleteReq := httptest.NewRequest("DELETE", "/containers/dns", nil)
	deleteReq = setChiURLParam(deleteReq, "id", "dns")
	deleteW := httptest.NewRecorder()
	handler.handleContainerDelete(deleteW, deleteReq)
	if deleteW.Code != http.StatusOK {
		t.Fatalf("conflict delete status = %d", deleteW.Code)
	}
	for _, key := range []ContainerKey{"dns:latest|53:1053", "dns:latest|53:2053"} {
		if storage.Get(key) != nil {
			t.Errorf("explicit conflict delete left legacy config %q", key)
		}
	}
}

func TestDashboardHandler_SaveMigratesUniqueLegacyKey(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	legacyKey := ContainerKey("dns:latest|53:1053")
	canonicalKey := ContainerKey("dns:latest|@dns;53/tcp:1053")
	if err := storage.Set(&StoredConfig{Key: legacyKey, AppName: "watchcow.dns"}); err != nil {
		t.Fatal(err)
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{{
		ID:                "dns",
		Name:              "dns",
		Image:             "dns:latest",
		State:             "running",
		Ports:             map[string]string{"53": "1053"},
		IdentityPorts:     map[string]string{"53/tcp": "1053"},
		LegacyPortOptions: map[string][]string{"53": {"1053"}},
		Labels:            map[string]string{},
	}}}

	form := url.Values{"display_name": {"DNS"}, "entry_port": {"1053"}}
	req := httptest.NewRequest("POST", "/containers/dns", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", "dns")
	w := httptest.NewRecorder()
	handler.handleContainerSave(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
	}
	if storage.Get(legacyKey) != nil || storage.Get(canonicalKey) == nil {
		t.Fatalf("legacy config was not migrated: legacy=%+v canonical=%+v", storage.Get(legacyKey), storage.Get(canonicalKey))
	}
	if saved := storage.Get(canonicalKey); !saved.Pending || saved.Revision == "" {
		t.Errorf("migrated config is not pending application: %+v", saved)
	}
}

func TestDashboardHandler_DeleteRemovesExactAndUniqueLegacyKeys(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	legacyKey := ContainerKey("web:latest|8080:18080")
	canonicalKey := ContainerKey("web:latest|@web;8080/tcp:18080")
	for _, key := range []ContainerKey{legacyKey, canonicalKey} {
		if err := storage.Set(&StoredConfig{Key: key, AppName: "watchcow.web"}); err != nil {
			t.Fatal(err)
		}
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{{
		ID:                "web",
		Name:              "web",
		Image:             "web:latest",
		State:             "running",
		Ports:             map[string]string{"8080": "18080"},
		IdentityPorts:     map[string]string{"8080/tcp": "18080"},
		LegacyPortOptions: map[string][]string{"8080": {"18080"}},
		Labels:            map[string]string{},
	}}}

	req := httptest.NewRequest("DELETE", "/containers/web", nil)
	req = setChiURLParam(req, "id", "web")
	w := httptest.NewRecorder()
	handler.handleContainerDelete(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", w.Code, w.Body.String())
	}
	if storage.Get(canonicalKey) != nil || storage.Get(legacyKey) != nil {
		t.Fatalf("delete left a storage alias: canonical=%+v legacy=%+v", storage.Get(canonicalKey), storage.Get(legacyKey))
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
	if !strings.Contains(body, `name="entry_redirect_force_external"`) {
		t.Error("response should contain force-external redirect control")
	}
}

func TestDashboardHandler_ContainerFormUsesUnnamedEntry(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	key := ContainerKey("nginx:alpine|80:8080")
	admin := StoredEntry{Name: "admin", Title: "Admin", Redirect: "https://admin.example", ForceExternal: false}
	defaultEntry := StoredEntry{Title: "Default", Protocol: "http", Port: "8080", Path: "/", UIType: "url", Redirect: "https://default.example", ForceExternal: true}
	if err := storage.Set(&StoredConfig{
		Key:         key,
		DisplayName: "Nginx",
		Entries:     []StoredEntry{admin, defaultEntry},
	}); err != nil {
		t.Fatalf("storage.Set() error = %v", err)
	}

	req := httptest.NewRequest("GET", "/containers/abc123", nil)
	req = setChiURLParam(req, "id", "abc123")
	w := httptest.NewRecorder()
	handler.handleContainerForm(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `value="https://default.example"`) || strings.Contains(body, `value="https://admin.example"`) {
		t.Errorf("form did not bind to unnamed entry: %s", body)
	}
	if !strings.Contains(body, `name="entry_redirect_force_external" value="true" checked`) {
		t.Error("form did not render unnamed entry's force-external state")
	}
}

func TestDashboardHandler_ContainerSave(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)

	containerID := "abc123"
	key := "nginx:alpine|80:8080"
	form := url.Values{
		"display_name":                  {"Nginx Test"},
		"description":                   {"Web server test"},
		"version":                       {"1.0.0"},
		"maintainer":                    {"Tester"},
		"entry_title":                   {"Nginx"},
		"entry_protocol":                {"http"},
		"entry_port":                    {"8080"},
		"entry_path":                    {"/"},
		"entry_ui_type":                 {"url"},
		"entry_redirect":                {"https://example.com"},
		"entry_redirect_force_external": {"true"},
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
	if saved.AppName != "watchcow.nginx.8080" {
		t.Errorf("AppName = %q, want %q", saved.AppName, "watchcow.nginx.8080")
	}
	if saved.DisplayName != "Nginx Test" {
		t.Errorf("DisplayName = %q, want %q", saved.DisplayName, "Nginx Test")
	}
	if len(saved.Entries) != 1 || !saved.Entries[0].ForceExternal {
		t.Errorf("force-external entry setting was not saved: %+v", saved.Entries)
	}
	if !saved.Pending || saved.Revision == "" {
		t.Errorf("saved config is not marked pending: %+v", saved)
	}

	// Verify TriggerInstall was called
	if len(trigger.triggerCalls) != 1 {
		t.Errorf("expected 1 TriggerInstall call, got %d", len(trigger.triggerCalls))
	} else {
		if trigger.triggerCalls[0].containerID != "abc123" {
			t.Errorf("TriggerInstall containerID = %q, want %q", trigger.triggerCalls[0].containerID, "abc123")
		}
		if trigger.triggerCalls[0].storedConfig.AppName != "watchcow.nginx.8080" {
			t.Errorf("TriggerInstall storedConfig.AppName = %q, want %q", trigger.triggerCalls[0].storedConfig.AppName, "watchcow.nginx.8080")
		}
		if !trigger.triggerCalls[0].storedConfig.Pending || trigger.triggerCalls[0].storedConfig.Revision != saved.Revision {
			t.Errorf("trigger did not receive pending revision: %+v", trigger.triggerCalls[0].storedConfig)
		}
	}
}

func TestDashboardHandler_ContainerSavePreservesHiddenAndNamedEntries(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	key := ContainerKey("nginx:alpine|80:8080")
	existing := &StoredConfig{
		Key:         key,
		AppName:     "watchcow.nginx.8080",
		DisplayName: "Old title",
		Entries: []StoredEntry{
			{
				Title:      "Old entry",
				Protocol:   "http",
				Port:       "8080",
				Path:       "/old",
				UIType:     "url",
				AllUsers:   true,
				FileTypes:  []string{"txt", "md"},
				NoDisplay:  true,
				IconBase64: "entry-icon",
			},
			{
				Name:       "admin",
				Title:      "Admin",
				Protocol:   "https",
				Port:       "8443",
				Path:       "/admin",
				UIType:     "iframe",
				FileTypes:  []string{"json"},
				IconBase64: "admin-icon",
			},
		},
	}
	if err := storage.Set(existing); err != nil {
		t.Fatalf("storage.Set() error = %v", err)
	}

	form := url.Values{
		"display_name":   {"Updated title"},
		"entry_title":    {"Updated entry"},
		"entry_protocol": {"https"},
		"entry_port":     {"8080"},
		"entry_path":     {"/new"},
		"entry_ui_type":  {"iframe"},
	}
	req := httptest.NewRequest("POST", "/containers/abc123", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", "abc123")
	w := httptest.NewRecorder()

	handler.handleContainerSave(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", w.Code, w.Body.String())
	}

	saved := storage.Get(key)
	if len(saved.Entries) != 2 {
		t.Fatalf("saved entries = %+v, want default and named entry", saved.Entries)
	}
	defaultEntry := saved.Entries[0]
	if defaultEntry.Title != "Updated entry" || defaultEntry.Path != "/new" ||
		!slices.Equal(defaultEntry.FileTypes, []string{"txt", "md"}) ||
		!defaultEntry.NoDisplay || defaultEntry.IconBase64 != "entry-icon" {
		t.Errorf("default entry lost hidden fields: %+v", defaultEntry)
	}
	if !reflect.DeepEqual(saved.Entries[1], existing.Entries[1]) {
		t.Errorf("named entry changed\n got: %+v\nwant: %+v", saved.Entries[1], existing.Entries[1])
	}
}

func TestDashboardHandler_ContainerSave_SanitizesAppName(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)

	handler.lister = &mockContainerLister{
		containers: []docker.ContainerInfo{
			{
				ID:     "ghi789",
				Name:   "my_app",
				Image:  "my_app:latest",
				State:  "running",
				Ports:  map[string]string{"80": "8080"},
				Labels: map[string]string{},
			},
		},
	}

	form := url.Values{
		"display_name": {"My App"},
		"entry_port":   {"8080"},
	}
	req := httptest.NewRequest("POST", "/containers/ghi789", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", "ghi789")
	w := httptest.NewRecorder()

	handler.handleContainerSave(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", resp.StatusCode, w.Body.String())
	}

	saved := storage.Get(ContainerKey("my_app:latest|80:8080"))
	if saved == nil {
		t.Fatal("config should be saved")
	}
	if saved.AppName != "watchcow.my-app.8080" {
		t.Errorf("AppName = %q, want %q", saved.AppName, "watchcow.my-app.8080")
	}
	if len(trigger.triggerCalls) != 1 {
		t.Fatalf("expected 1 TriggerInstall call, got %d", len(trigger.triggerCalls))
	}
	if trigger.triggerCalls[0].storedConfig.AppName != "watchcow.my-app.8080" {
		t.Errorf("trigger AppName = %q, want %q", trigger.triggerCalls[0].storedConfig.AppName, "watchcow.my-app.8080")
	}
}

func TestDashboardHandler_ProcessIconRejectsOversizedFile(t *testing.T) {
	handler, _, _ := setupTestHandler(t)

	_, err := handler.processIcon(strings.NewReader(strings.Repeat("x", maxDashboardIconBytes+1)))
	if err == nil {
		t.Fatal("processIcon should reject oversized icon data")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("error = %q, want size limit error", err.Error())
	}
}

func TestDashboardHandler_ContainerSave_LabelConfigured(t *testing.T) {
	handler, _, _ := setupTestHandler(t)

	// redis has watchcow.enable=true label
	containerID := "def456"
	form := url.Values{
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
				Name:          "",
				Title:         "Default",
				Protocol:      "http",
				Port:          "80",
				Path:          "/",
				UIType:        "url",
				AllUsers:      true,
				FileTypes:     []string{".html"},
				NoDisplay:     false,
				Redirect:      "https://example.com",
				ForceExternal: true,
				IconBase64:    "entry-icon",
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
	if !entry.ForceExternal {
		t.Error("Entry.ForceExternal should be true")
	}
	if entry.IconBase64 != "entry-icon" {
		t.Errorf("Entry.IconBase64 = %q, want %q", entry.IconBase64, "entry-icon")
	}

	dockerCfg.Entries[0].FileTypes[0] = "mutated"
	if config.Entries[0].FileTypes[0] != ".html" {
		t.Errorf("convertToDockerConfig returned shared FileTypes: %v", config.Entries[0].FileTypes)
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
		"display_name":   {"Nginx"},
		"entry_protocol": {"http"},
		"entry_port":     {"8080"},
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
	if call.storedConfig.AppName != "watchcow.nginx.8080" {
		t.Errorf("storedConfig.AppName = %q, want %q", call.storedConfig.AppName, "watchcow.nginx.8080")
	}
}

func TestDashboardHandler_NilTrigger(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("TRIM_PKGETC", tmpDir)
	defer os.Unsetenv("TRIM_PKGETC")

	storage, _ := NewDashboardStorage()
	lister := &mockContainerLister{
		containers: []docker.ContainerInfo{
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
