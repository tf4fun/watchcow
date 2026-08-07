package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"watchcow/internal/app"
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
	triggerCalls   []triggerCall
	uninstallCalls []uninstallTriggerCall
}

type triggerCall struct {
	containerID  string
	storedConfig *docker.StoredConfig
}

type uninstallTriggerCall struct {
	containerID string
	appName     string
	configKey   string
}

func (m *mockAppTrigger) TriggerInstall(containerID string, storedConfig *docker.StoredConfig) {
	m.triggerCalls = append(m.triggerCalls, triggerCall{containerID, storedConfig})
}

func (m *mockAppTrigger) TriggerUninstall(containerID, appName, configKey string) {
	m.uninstallCalls = append(m.uninstallCalls, uninstallTriggerCall{containerID, appName, configKey})
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

func newMultipartSaveRequest(t *testing.T, path string, form url.Values, filename string, fileData []byte) *http.Request {
	t.Helper()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for key, values := range form {
		for _, value := range values {
			if err := w.WriteField(key, value); err != nil {
				t.Fatalf("WriteField(%q) error = %v", key, err)
			}
		}
	}
	if filename != "" {
		part, err := w.CreateFormFile("icon", filename)
		if err != nil {
			t.Fatalf("CreateFormFile() error = %v", err)
		}
		if _, err := part.Write(fileData); err != nil {
			t.Fatalf("write icon error = %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("multipart.Close() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func testPNG(t *testing.T, c color.Color) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, c)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}
	return buf.Bytes()
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
	if !strings.Contains(body, "<th>应用配置</th>") {
		t.Error("response should combine application status and actions")
	}
	if strings.Contains(body, "<th>应用状态</th>") || strings.Contains(body, ">操作</th>") {
		t.Error("response should not render separate application status and action columns")
	}
	if !strings.Contains(body, "由标签配置") {
		t.Error("label-managed containers should explain why dashboard actions are unavailable")
	}
	if !strings.Contains(body, "添加配置") || !strings.Contains(body, "add-config-button") {
		t.Error("unconfigured containers should offer a distinct add action")
	}
}

func TestDashboardHandler_ContainerListShowsAsyncFailure(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	key := ContainerKey("nginx:alpine|80:8080")
	if err := storage.Set(&StoredConfig{
		Key: key, AppName: "watchcow.nginx", Pending: true, LastError: "install failed",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/containers", nil)
	w := httptest.NewRecorder()
	handler.handleContainerList(w, req)
	if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, "应用失败") ||
		!strings.Contains(body, "install failed") || strings.Contains(body, "修改配置") {
		t.Fatalf("async failure was not visible: status=%d body=%s", w.Code, body)
	}
}

func TestDashboardHandler_ContainerListUsesSingleAppliedConfigAction(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	key := ContainerKey("nginx:alpine|80:8080")
	if err := storage.Set(&StoredConfig{Key: key, AppName: "watchcow.nginx"}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/containers", nil)
	w := httptest.NewRecorder()
	handler.handleContainerList(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "修改配置") || !strings.Contains(body, "is-primary is-outlined") ||
		strings.Contains(body, "已应用") {
		t.Fatalf("applied config rendered duplicate state and action: status=%d body=%s", w.Code, body)
	}
}

func TestDashboardHandler_BlocksActiveConfigOperations(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusText string
		config     StoredConfig
	}{
		{name: "apply", statusText: "应用中", config: StoredConfig{Pending: true}},
		{name: "delete", statusText: "删除中", config: StoredConfig{Deleting: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, storage, trigger := setupTestHandler(t)
			key := ContainerKey("nginx:alpine|80:8080")
			test.config.Key = key
			test.config.AppName = "watchcow.nginx"
			if err := storage.Set(&test.config); err != nil {
				t.Fatal(err)
			}

			listReq := httptest.NewRequest(http.MethodGet, "/containers", nil)
			listW := httptest.NewRecorder()
			handler.handleContainerList(listW, listReq)
			listBody := listW.Body.String()
			if listW.Code != http.StatusOK || !strings.Contains(listBody, test.statusText) ||
				strings.Contains(listBody, `hx-get="containers/abc123"`) {
				t.Fatalf("active operation remained editable: status=%d body=%s", listW.Code, listBody)
			}

			formReq := httptest.NewRequest(http.MethodGet, "/containers/abc123", nil)
			formReq = setChiURLParam(formReq, "id", "abc123")
			formW := httptest.NewRecorder()
			handler.handleContainerForm(formW, formReq)
			if formW.Code != http.StatusOK || !strings.Contains(formW.Body.String(), `class="container-view"`) ||
				strings.Contains(formW.Body.String(), "<form") {
				t.Fatalf("active operation opened a stale form: status=%d body=%s", formW.Code, formW.Body.String())
			}

			saveReq := httptest.NewRequest(http.MethodPost, "/containers/abc123", nil)
			saveReq = setChiURLParam(saveReq, "id", "abc123")
			saveW := httptest.NewRecorder()
			handler.handleContainerSave(saveW, saveReq)
			if saveW.Code != http.StatusConflict || len(trigger.triggerCalls) != 0 {
				t.Fatalf("active operation accepted save: status=%d calls=%d", saveW.Code, len(trigger.triggerCalls))
			}

			deleteReq := httptest.NewRequest(http.MethodDelete, "/containers/abc123", nil)
			deleteReq = setChiURLParam(deleteReq, "id", "abc123")
			deleteW := httptest.NewRecorder()
			handler.handleContainerDelete(deleteW, deleteReq)
			if deleteW.Code != http.StatusConflict || len(trigger.uninstallCalls) != 0 {
				t.Fatalf("active operation accepted delete: status=%d calls=%d", deleteW.Code, len(trigger.uninstallCalls))
			}
		})
	}
}

func TestDashboardHandler_NonActiveConfigStatesRemainEditable(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  string
		config StoredConfig
	}{
		{name: "waiting for stopped container", state: "exited", config: StoredConfig{Pending: true}},
		{name: "failed apply", state: "running", config: StoredConfig{Pending: true, LastError: "install failed"}},
		{name: "failed delete", state: "running", config: StoredConfig{Deleting: true, LastError: "uninstall failed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, storage, _ := setupTestHandler(t)
			handler.lister.(*mockContainerLister).containers[0].State = test.state
			key := ContainerKey("nginx:alpine|80:8080")
			test.config.Key = key
			test.config.AppName = "watchcow.nginx"
			if err := storage.Set(&test.config); err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodGet, "/containers/abc123", nil)
			req = setChiURLParam(req, "id", "abc123")
			w := httptest.NewRecorder()
			handler.handleContainerForm(w, req)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<form") {
				t.Fatalf("non-active config state was not editable: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDashboardHandler_ContainerListShowsBlockedConfig(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	key := ContainerKey("nginx:alpine|80:8080")
	if err := storage.Set(&StoredConfig{
		Key: key, AppName: "watchcow.nginx", LastError: "需要重新配置端口",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/containers", nil)
	w := httptest.NewRecorder()
	handler.handleContainerList(w, req)
	if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, "待修复") || strings.Contains(body, "已应用") {
		t.Fatalf("blocked config status was misleading: status=%d body=%s", w.Code, body)
	}
}

func TestDashboardHandler_NoPortContainerAcceptsManualPortOrRedirect(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{{
		ID: "worker", Name: "worker", Image: "worker:latest", State: "running",
		IdentityPorts: map[string]string{}, Labels: map[string]string{}, NetworkMode: "bridge",
	}}}

	listReq := httptest.NewRequest(http.MethodGet, "/containers", nil)
	listW := httptest.NewRecorder()
	handler.handleContainerList(listW, listReq)
	if listW.Code != http.StatusOK || !strings.Contains(listW.Body.String(), `hx-get="containers/worker"`) {
		t.Fatalf("no-port container was not configurable: status=%d body=%s", listW.Code, listW.Body.String())
	}

	formReq := httptest.NewRequest(http.MethodGet, "/containers/worker", nil)
	formReq = setChiURLParam(formReq, "id", "worker")
	formW := httptest.NewRecorder()
	handler.handleContainerForm(formW, formReq)
	body := formW.Body.String()
	if formW.Code != http.StatusOK || !strings.Contains(body, `type="number" name="entry_port" id="entry-port"`) ||
		!strings.Contains(body, `id="entry-redirect"`) || !strings.Contains(body, `syncPortRequirement()`) {
		t.Fatalf("manual-port form was not rendered: status=%d body=%s", formW.Code, body)
	}
	if strings.Contains(body, `type="hidden" name="entry_port"`) {
		t.Fatalf("no-port form still forced an empty port: %s", body)
	}

	form := url.Values{
		"display_name": {"Worker"}, "entry_protocol": {"http"},
		"entry_port": {"18080"}, "entry_path": {"/"}, "entry_ui_type": {"url"},
	}
	saveReq := httptest.NewRequest(http.MethodPost, "/containers/worker", strings.NewReader(form.Encode()))
	saveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saveReq = setChiURLParam(saveReq, "id", "worker")
	saveW := httptest.NewRecorder()
	handler.handleContainerSave(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("no-port manual-port save status=%d body=%s", saveW.Code, saveW.Body.String())
	}
	saved := storage.Get(ContainerKey("worker:latest|@worker"))
	if saved == nil || len(saved.Entries) != 1 || saved.Entries[0].Port != "18080" || len(trigger.triggerCalls) != 1 {
		t.Fatalf("no-port manual port was not applied: config=%+v calls=%v", saved, trigger.triggerCalls)
	}
}

func TestDashboardHandler_HostNetworkContainerAcceptsManualPort(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{{
		ID: "host-app", Name: "host-app", Image: "host-app:latest", State: "running",
		IdentityPorts: map[string]string{}, Labels: map[string]string{}, NetworkMode: "host",
	}}}

	form := url.Values{
		"display_name": {"Host App"}, "entry_protocol": {"http"},
		"entry_port": {"8080"}, "entry_path": {"/"}, "entry_ui_type": {"url"},
	}
	req := httptest.NewRequest(http.MethodPost, "/containers/host-app", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", "host-app")
	w := httptest.NewRecorder()
	handler.handleContainerSave(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("host-network save status=%d body=%s", w.Code, w.Body.String())
	}
	key := ContainerKey("host-app:latest|@host-app")
	saved := storage.Get(key)
	if saved == nil || len(saved.Entries) != 1 || saved.Entries[0].Port != "8080" || len(trigger.triggerCalls) != 1 {
		t.Fatalf("host-network manual port was not applied: config=%+v calls=%v", saved, trigger.triggerCalls)
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

func TestDashboardHandler_ContainerListFindsConfigAfterImageAndPortChange(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	oldKey := ContainerKey("nginx:1.25|@nginx;80/tcp:8080")
	if err := storage.Set(&StoredConfig{Key: oldKey, AppName: "watchcow.nginx.8080"}); err != nil {
		t.Fatal(err)
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{{
		ID: "nginx", Name: "nginx", Image: "nginx:1.26", State: "running",
		Ports: map[string]string{"80": "9090"}, IdentityPorts: map[string]string{"80/tcp": "9090"},
		LegacyPortOptions: map[string][]string{"80": {"9090"}}, Labels: map[string]string{},
	}}}

	containers, err := handler.listContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || !containers[0].HasStoredConfig || containers[0].ConfigKey != oldKey {
		t.Fatalf("same-name config disappeared after runtime change: %+v", containers)
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
	configA := storage.Get(keyA)
	if w.Code != http.StatusOK || configA == nil || !configA.Deleting || storage.Get(keyB) == nil || storage.Get(keyB).Deleting {
		t.Fatalf("delete crossed named owners: status=%d a=%+v b=%+v", w.Code, storage.Get(keyA), storage.Get(keyB))
	}
}

func TestDashboardHandler_ExactOwnerPreservesDifferentAppHistory(t *testing.T) {
	handler, storage, _ := setupTestHandler(t)
	currentKey := ContainerKey("web:2|@web;8080/tcp:18080")
	historyKey := ContainerKey("web:1|@web;8080/tcp:18080")
	current := &StoredConfig{
		Key: currentKey, AppName: "watchcow.web-current", DisplayName: "Web", Description: "web:2",
		Version: "1.0.0", Maintainer: "WatchCow",
		Entries: []StoredEntry{{Protocol: "http", Port: "18080", Path: "/", UIType: "url", AllUsers: true}},
	}
	for _, config := range []*StoredConfig{current, {Key: historyKey, AppName: "watchcow.web-history"}} {
		if err := storage.Set(config); err != nil {
			t.Fatal(err)
		}
	}
	handler.lister = &mockContainerLister{containers: []docker.ContainerInfo{{
		ID: "web", Name: "web", Image: "web:2", State: "running",
		Ports: map[string]string{"8080": "18080"}, IdentityPorts: map[string]string{"8080/tcp": "18080"},
		LegacyPortOptions: map[string][]string{"8080": {"18080"}}, Labels: map[string]string{},
	}}}

	containers, err := handler.listContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || !containers[0].HasStoredConfig || !containers[0].LegacyConfigConflict || len(containers[0].LegacyConfigKeys) != 0 {
		t.Fatalf("different app history was treated as a removable alias: %+v", containers)
	}
	form := url.Values{
		"display_name": {"Web"}, "description": {"web:2"}, "version": {"1.0.0"}, "maintainer": {"WatchCow"},
		"entry_protocol": {"http"}, "entry_port": {"18080"}, "entry_path": {"/"},
		"entry_ui_type": {"url"}, "entry_all_users": {"true"},
	}
	req := httptest.NewRequest(http.MethodPost, "/containers/web", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", "web")
	w := httptest.NewRecorder()
	handler.handleContainerSave(w, req)
	if w.Code != http.StatusOK || storage.Get(historyKey) == nil {
		t.Fatalf("save removed different app history: status=%d history=%+v", w.Code, storage.Get(historyKey))
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/containers/web", nil)
	deleteReq = setChiURLParam(deleteReq, "id", "web")
	deleteW := httptest.NewRecorder()
	handler.handleContainerDelete(deleteW, deleteReq)
	if deleteW.Code != http.StatusOK || storage.Get(historyKey) == nil || storage.Get(historyKey).Deleting {
		t.Fatalf("delete crossed different app history: status=%d history=%+v", deleteW.Code, storage.Get(historyKey))
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
	formReq := httptest.NewRequest(http.MethodGet, "/containers/dns", nil)
	formReq = setChiURLParam(formReq, "id", "dns")
	formW := httptest.NewRecorder()
	handler.handleContainerForm(formW, formReq)
	if !strings.Contains(formW.Body.String(), `hx-delete="containers/dns"`) || !strings.Contains(formW.Body.String(), "多个历史配置") {
		t.Fatalf("conflict cleanup is not reachable from dashboard: %s", formW.Body.String())
	}

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
		if config := storage.Get(key); config == nil || !config.Deleting {
			t.Errorf("explicit conflict delete did not persist intent for %q: %+v", key, config)
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
	if canonical, legacy := storage.Get(canonicalKey), storage.Get(legacyKey); canonical == nil || legacy == nil || !canonical.Deleting || !legacy.Deleting {
		t.Fatalf("delete intent did not cover all aliases: canonical=%+v legacy=%+v", canonical, legacy)
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
	if !strings.Contains(body, `id="entry-port"`) || !strings.Contains(body, `syncPortRequirement()`) {
		t.Error("response should enforce the conditional port requirement")
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

func TestDashboardHandler_ContainerSaveRejectsInvalidEntry(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(url.Values)
	}{
		{
			name: "protocol",
			mutate: func(form url.Values) {
				form.Set("entry_protocol", "ftp")
			},
		},
		{
			name: "ui type",
			mutate: func(form url.Values) {
				form.Set("entry_ui_type", "popup")
			},
		},
		{
			name: "non numeric port",
			mutate: func(form url.Values) {
				form.Set("entry_port", "abc")
			},
		},
		{
			name: "port below range",
			mutate: func(form url.Values) {
				form.Set("entry_port", "0")
			},
		},
		{
			name: "port above range",
			mutate: func(form url.Values) {
				form.Set("entry_port", "65536")
			},
		},
		{
			name: "ordinary entry without port",
			mutate: func(form url.Values) {
				form.Del("entry_port")
			},
		},
		{
			name: "whitespace redirect without port",
			mutate: func(form url.Values) {
				form.Del("entry_port")
				form.Set("entry_redirect", "   ")
			},
		},
		{
			name: "redirect without host",
			mutate: func(form url.Values) {
				form.Set("entry_redirect", "https://")
			},
		},
		{
			name: "redirect with unsupported scheme",
			mutate: func(form url.Values) {
				form.Set("entry_redirect", "javascript:alert(1)")
			},
		},
		{
			name: "malformed redirect",
			mutate: func(form url.Values) {
				form.Set("entry_redirect", "not a url")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, storage, trigger := setupTestHandler(t)
			form := url.Values{
				"display_name":   {"Nginx"},
				"entry_protocol": {"http"},
				"entry_port":     {"8080"},
				"entry_path":     {"/"},
				"entry_ui_type":  {"url"},
			}
			tt.mutate(form)

			req := httptest.NewRequest(http.MethodPost, "/containers/abc123", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req = setChiURLParam(req, "id", "abc123")
			w := httptest.NewRecorder()
			handler.handleContainerSave(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("save status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if storage.Has(ContainerKey("nginx:alpine|80:8080")) {
				t.Fatal("invalid entry was persisted")
			}
			if len(trigger.triggerCalls) != 0 {
				t.Fatalf("invalid entry triggered %d installs", len(trigger.triggerCalls))
			}
		})
	}
}

func TestDashboardHandler_ContainerSaveAllowsRedirectWithoutPort(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)
	form := url.Values{
		"display_name":   {"Nginx"},
		"entry_protocol": {"https"},
		"entry_path":     {"/"},
		"entry_ui_type":  {"url"},
		"entry_redirect": {"https://example.com"},
	}
	req := httptest.NewRequest(http.MethodPost, "/containers/abc123", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", "abc123")
	w := httptest.NewRecorder()
	handler.handleContainerSave(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("save status = %d, want 200: %s", w.Code, w.Body.String())
	}
	saved := storage.Get(ContainerKey("nginx:alpine|80:8080"))
	if saved == nil || len(saved.Entries) != 1 || saved.Entries[0].Port != "" {
		t.Fatalf("redirect-only entry was not saved: %+v", saved)
	}
	if len(trigger.triggerCalls) != 1 {
		t.Fatalf("redirect-only save triggered %d installs, want 1", len(trigger.triggerCalls))
	}
	if err := storage.MarkApplied(string(saved.Key), saved.Revision); err != nil {
		t.Fatal(err)
	}

	formReq := httptest.NewRequest(http.MethodGet, "/containers/abc123", nil)
	formReq = setChiURLParam(formReq, "id", "abc123")
	formW := httptest.NewRecorder()
	handler.handleContainerForm(formW, formReq)
	if !strings.Contains(formW.Body.String(), `<option value="" selected>仅外部跳转</option>`) {
		t.Fatalf("redirect-only port was not preserved in form: %s", formW.Body.String())
	}
}

func TestDashboardHandler_ContainerSaveRejectsInvalidIconWithoutPersistence(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "not an image", data: []byte("not an image")},
		{name: "oversized", data: bytes.Repeat([]byte{'x'}, maxDashboardIconBytes+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, storage, trigger := setupTestHandler(t)
			key := ContainerKey("nginx:alpine|80:8080")
			existing := &StoredConfig{
				Key:         key,
				AppName:     "watchcow.nginx",
				DisplayName: "Before",
				Version:     "1.0.0",
				Maintainer:  "WatchCow",
				Entries: []StoredEntry{{
					Protocol: "http", Port: "8080", Path: "/", UIType: "url", AllUsers: true,
				}},
				Revision:  "applied-revision",
				CreatedAt: time.Unix(100, 0),
				UpdatedAt: time.Unix(200, 0),
			}
			if err := storage.Set(existing); err != nil {
				t.Fatal(err)
			}
			form := url.Values{
				"display_name":    {"After"},
				"version":         {"1.0.0"},
				"maintainer":      {"WatchCow"},
				"entry_protocol":  {"http"},
				"entry_port":      {"8080"},
				"entry_path":      {"/"},
				"entry_ui_type":   {"url"},
				"entry_all_users": {"true"},
			}
			req := newMultipartSaveRequest(t, "/containers/abc123", form, "icon.png", tt.data)
			req = setChiURLParam(req, "id", "abc123")
			w := httptest.NewRecorder()
			handler.handleContainerSave(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("save status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if got := storage.Get(key); !reflect.DeepEqual(got, existing) {
				t.Fatalf("invalid icon changed storage\n got: %+v\nwant: %+v", got, existing)
			}
			if len(trigger.triggerCalls) != 0 {
				t.Fatalf("invalid icon triggered %d installs", len(trigger.triggerCalls))
			}
		})
	}
}

func TestDashboardHandler_ContainerSaveNewIconOverridesCompatibilityEntryIcon(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)
	key := ContainerKey("nginx:alpine|80:8080")
	existing := &StoredConfig{
		Key:         key,
		AppName:     "watchcow.nginx",
		DisplayName: "Nginx",
		Version:     "1.0.0",
		Maintainer:  "WatchCow",
		IconBase64:  "old-global-icon",
		Entries: []StoredEntry{
			{Protocol: "http", Port: "8080", Path: "/", UIType: "url", AllUsers: true, IconBase64: "old-entry-icon"},
			{Name: "admin", IconBase64: "admin-icon"},
		},
	}
	if err := storage.Set(existing); err != nil {
		t.Fatal(err)
	}
	iconData := testPNG(t, color.RGBA{R: 0xff, A: 0xff})
	form := url.Values{
		"display_name":    {"Nginx"},
		"version":         {"1.0.0"},
		"maintainer":      {"WatchCow"},
		"entry_protocol":  {"http"},
		"entry_port":      {"8080"},
		"entry_path":      {"/"},
		"entry_ui_type":   {"url"},
		"entry_all_users": {"true"},
	}
	req := newMultipartSaveRequest(t, "/containers/abc123", form, "icon.png", iconData)
	req = setChiURLParam(req, "id", "abc123")
	w := httptest.NewRecorder()
	handler.handleContainerSave(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("save status = %d, want 200: %s", w.Code, w.Body.String())
	}
	wantIcon := base64.StdEncoding.EncodeToString(iconData)
	saved := storage.Get(key)
	if saved.IconBase64 != wantIcon || saved.Entries[0].IconBase64 != "" {
		t.Fatalf("uploaded icon did not become authoritative: %+v", saved)
	}
	if saved.Entries[1].IconBase64 != "admin-icon" {
		t.Fatalf("named entry icon changed: %+v", saved.Entries[1])
	}
	if len(trigger.triggerCalls) != 1 {
		t.Fatalf("icon save triggered %d installs, want 1", len(trigger.triggerCalls))
	}
	if err := storage.MarkApplied(string(key), saved.Revision); err != nil {
		t.Fatal(err)
	}
	trigger.triggerCalls = nil

	req = newMultipartSaveRequest(t, "/containers/abc123", form, "icon.png", iconData)
	req = setChiURLParam(req, "id", "abc123")
	w = httptest.NewRecorder()
	handler.handleContainerSave(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("repeat icon save status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(trigger.triggerCalls) != 0 || storage.Get(key).Pending {
		t.Fatalf("re-uploading identical icon was not a no-op: calls=%d config=%+v", len(trigger.triggerCalls), storage.Get(key))
	}
}

func TestDashboardHandler_ContainerSaveNoContentChangeDoesNotReinstall(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)
	key := ContainerKey("nginx:alpine|80:8080")
	form := url.Values{
		"display_name":    {"Nginx"},
		"description":     {"nginx:alpine"},
		"version":         {"1.0.0"},
		"maintainer":      {"WatchCow"},
		"entry_protocol":  {"http"},
		"entry_port":      {"8080"},
		"entry_path":      {"/"},
		"entry_ui_type":   {"url"},
		"entry_all_users": {"true"},
	}
	save := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/containers/abc123", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = setChiURLParam(req, "id", "abc123")
		w := httptest.NewRecorder()
		handler.handleContainerSave(w, req)
		return w
	}

	if w := save(); w.Code != http.StatusOK {
		t.Fatalf("initial save status = %d: %s", w.Code, w.Body.String())
	}
	first := storage.Get(key)
	if err := storage.MarkApplied(string(key), first.Revision); err != nil {
		t.Fatal(err)
	}
	applied := storage.Get(key)
	trigger.triggerCalls = nil

	if w := save(); w.Code != http.StatusOK {
		t.Fatalf("no-op save status = %d: %s", w.Code, w.Body.String())
	}
	got := storage.Get(key)
	if got.Pending {
		t.Fatal("no-op save marked config pending")
	}
	if got.Revision != applied.Revision || !got.UpdatedAt.Equal(applied.UpdatedAt) {
		t.Fatalf("no-op save changed revision metadata\n got: %+v\nwant: %+v", got, applied)
	}
	if len(trigger.triggerCalls) != 0 {
		t.Fatalf("no-op save triggered %d installs", len(trigger.triggerCalls))
	}
}

func TestDashboardHandler_UnchangedSaveRetriesFailedConfig(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)
	key := ContainerKey("nginx:alpine|80:8080")
	form := url.Values{
		"display_name": {"Nginx"}, "description": {"nginx:alpine"},
		"version": {"1.0.0"}, "maintainer": {"WatchCow"},
		"entry_protocol": {"http"}, "entry_port": {"8080"},
		"entry_path": {"/"}, "entry_ui_type": {"url"}, "entry_all_users": {"true"},
	}
	save := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/containers/abc123", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = setChiURLParam(req, "id", "abc123")
		w := httptest.NewRecorder()
		handler.handleContainerSave(w, req)
		return w
	}

	if w := save(); w.Code != http.StatusOK {
		t.Fatalf("initial save status = %d: %s", w.Code, w.Body.String())
	}
	current := storage.Get(key)
	if err := storage.MarkFailed(string(key), current.Revision, "install failed"); err != nil {
		t.Fatal(err)
	}
	trigger.triggerCalls = nil
	if w := save(); w.Code != http.StatusOK {
		t.Fatalf("retry save status = %d: %s", w.Code, w.Body.String())
	}
	got := storage.Get(key)
	if len(trigger.triggerCalls) != 1 || !got.Pending || got.Deleting || got.LastError != "" {
		t.Fatalf("retry was not scheduled: calls=%d config=%+v", len(trigger.triggerCalls), got)
	}
}

func TestDashboardConfigRevisionOnlyTracksPackageContent(t *testing.T) {
	base := &StoredConfig{
		Key:         "old-key",
		AppName:     "watchcow.test",
		DisplayName: "Test",
		Description: "description",
		Version:     "1.0.0",
		Maintainer:  "WatchCow",
		Entries: []StoredEntry{{
			Protocol: "http", Port: "8080", Path: "/", UIType: "url", AllUsers: true,
		}},
		Revision:  "old-revision",
		Pending:   true,
		CreatedAt: time.Unix(100, 0),
		UpdatedAt: time.Unix(200, 0),
	}
	want := dashboardConfigRevision(base)

	metadataOnly := *base
	metadataOnly.Key = "new-key"
	metadataOnly.Revision = "new-revision"
	metadataOnly.Pending = false
	metadataOnly.CreatedAt = time.Unix(300, 0)
	metadataOnly.UpdatedAt = time.Unix(400, 0)
	if got := dashboardConfigRevision(&metadataOnly); got != want {
		t.Fatalf("metadata changed content revision: got %q, want %q", got, want)
	}

	contentChanged := metadataOnly
	contentChanged.DisplayName = "Changed"
	if got := dashboardConfigRevision(&contentChanged); got == want {
		t.Fatalf("content change kept revision %q", got)
	}
}

func TestDashboardHandler_ContainerSavePreservesExistingAppName(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)
	key := ContainerKey("nginx:alpine|80:8080")
	if err := storage.Set(&StoredConfig{
		Key:         key,
		AppName:     "watchcow.stable-name",
		DisplayName: "Before",
		Version:     "1.0.0",
		Maintainer:  "WatchCow",
		Entries: []StoredEntry{{
			Protocol: "http", Port: "8080", Path: "/", UIType: "url", AllUsers: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"display_name":    {"After"},
		"version":         {"1.0.0"},
		"maintainer":      {"WatchCow"},
		"entry_protocol":  {"http"},
		"entry_port":      {"8080"},
		"entry_path":      {"/"},
		"entry_ui_type":   {"url"},
		"entry_all_users": {"true"},
	}
	req := httptest.NewRequest(http.MethodPost, "/containers/abc123", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = setChiURLParam(req, "id", "abc123")
	w := httptest.NewRecorder()
	handler.handleContainerSave(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
	}
	if got := storage.Get(key).AppName; got != "watchcow.stable-name" {
		t.Fatalf("existing AppName = %q, want %q", got, "watchcow.stable-name")
	}
	if len(trigger.triggerCalls) != 1 {
		t.Fatalf("changed config triggered %d installs, want 1", len(trigger.triggerCalls))
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
	wantAppName := app.DashboardAppName("my_app", "8080")
	if saved.AppName != wantAppName {
		t.Errorf("AppName = %q, want %q", saved.AppName, wantAppName)
	}
	if len(trigger.triggerCalls) != 1 {
		t.Fatalf("expected 1 TriggerInstall call, got %d", len(trigger.triggerCalls))
	}
	if trigger.triggerCalls[0].storedConfig.AppName != wantAppName {
		t.Errorf("trigger AppName = %q, want %q", trigger.triggerCalls[0].storedConfig.AppName, wantAppName)
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

func TestDashboardHandler_ContainerDelete_LabelConfigured(t *testing.T) {
	handler, storage, trigger := setupTestHandler(t)
	key := ContainerKey("redis:latest|6379:6379")
	if err := storage.Set(&StoredConfig{Key: key, AppName: "watchcow.redis"}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/containers/def456", nil)
	req = setChiURLParam(req, "id", "def456")
	w := httptest.NewRecorder()
	handler.handleContainerDelete(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if storage.Get(key) == nil || len(trigger.uninstallCalls) != 0 {
		t.Fatalf("label-owned config was changed: config=%+v calls=%+v", storage.Get(key), trigger.uninstallCalls)
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

	// The config remains as a durable delete intent until uninstall succeeds.
	if config := storage.Get(key); config == nil || !config.Deleting {
		t.Errorf("config should be marked deleting: %+v", config)
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
		"entry_port":   {"8080"},
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
