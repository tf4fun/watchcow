package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html/template"
	"image"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"watchcow/internal/docker"
	"watchcow/web"
)

// ContainerLister provides container listing capability.
type ContainerLister interface {
	ListAllContainers(ctx context.Context) ([]docker.ContainerInfo, error)
}

// AppTrigger triggers app installation/uninstallation for containers.
type AppTrigger interface {
	// TriggerInstall triggers app installation for a container using stored config.
	TriggerInstall(containerID string, storedConfig *docker.StoredConfig)
	// TriggerUninstall triggers app uninstallation by app name.
	TriggerUninstall(appName string)
}

// DashboardHandler provides HTTP handlers for the dashboard.
type DashboardHandler struct {
	storage *DashboardStorage
	lister  ContainerLister
	trigger AppTrigger
	tmpl    *template.Template
}

// NewDashboardHandler creates a new dashboard handler.
func NewDashboardHandler(storage *DashboardStorage, lister ContainerLister, trigger AppTrigger) (*DashboardHandler, error) {
	// Parse templates from embedded FS
	funcMap := template.FuncMap{
		"js":        template.JSEscapeString,
		"hasPrefix": strings.HasPrefix,
	}

	tmpl := template.New("").Funcs(funcMap)

	// Parse all dashboard templates
	templateFiles := []string{
		"templates/dashboard.tmpl",
		"templates/container_list.tmpl",
		"templates/container_form.tmpl",
	}

	for _, file := range templateFiles {
		content, err := web.Assets.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("failed to read template %s: %w", file, err)
		}
		// Use the base filename as template name
		name := strings.TrimPrefix(file, "templates/")
		name = strings.TrimSuffix(name, ".tmpl")
		if _, err := tmpl.New(name).Parse(string(content)); err != nil {
			return nil, fmt.Errorf("failed to parse template %s: %w", file, err)
		}
	}

	return &DashboardHandler{
		storage: storage,
		lister:  lister,
		trigger: trigger,
		tmpl:    tmpl,
	}, nil
}

// Mount registers the dashboard routes on the given router.
func (h *DashboardHandler) Mount(r chi.Router) {
	r.Get("/", h.handleDashboard)
	r.Get("/containers", h.handleContainerList)
	// Use container ID in URL path (safe characters, no encoding issues)
	r.Get("/containers/{id}", h.handleContainerForm)
	r.Post("/containers/{id}", h.handleContainerSave)
	r.Delete("/containers/{id}", h.handleContainerDelete)
}

// listContainers fetches containers and enriches with storage info.
func (h *DashboardHandler) listContainers(ctx context.Context) ([]ContainerInfo, error) {
	containers, err := h.lister.ListAllContainers(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]ContainerInfo, 0, len(containers))
	for _, c := range containers {
		key := NewContainerKey(c.Image, c.Ports)
		hasLabelConfig := c.Labels["watchcow.enable"] == "true"
		hasStoredConfig := h.storage.Has(key)

		info := ContainerInfo{
			ID:              c.ID,
			Name:            c.Name,
			Image:           c.Image,
			State:           c.State,
			Ports:           c.Ports,
			Labels:          c.Labels,
			NetworkMode:     c.NetworkMode,
			Key:             key,
			HasLabelConfig:  hasLabelConfig,
			HasStoredConfig: hasStoredConfig,
			Config:          h.storage.Get(key),
		}
		result = append(result, info)
	}

	// Sort by name
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

// getContainer fetches a single container by key.
func (h *DashboardHandler) getContainer(ctx context.Context, key ContainerKey) (*ContainerInfo, error) {
	containers, err := h.listContainers(ctx)
	if err != nil {
		return nil, err
	}

	for i := range containers {
		if containers[i].Key == key {
			return &containers[i], nil
		}
	}

	return nil, fmt.Errorf("container not found: %s", key)
}

// getContainerByID fetches a single container by its ID.
func (h *DashboardHandler) getContainerByID(ctx context.Context, id string) (*ContainerInfo, error) {
	containers, err := h.listContainers(ctx)
	if err != nil {
		return nil, err
	}

	for i := range containers {
		if containers[i].ID == id {
			return &containers[i], nil
		}
	}

	return nil, fmt.Errorf("container not found: %s", id)
}

// dashboardData holds data for the main dashboard template.
type dashboardData struct {
	BulmaCSS template.CSS
	HtmxJS   template.JS
}

// handleDashboard renders the main dashboard page.
func (h *DashboardHandler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Load CSS
	cssBytes, err := web.Assets.ReadFile("css/bulma.min.css")
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "加载 CSS 失败")
		return
	}

	// Load HTMX JS
	htmxBytes, err := web.Assets.ReadFile("js/htmx.min.js")
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "加载 HTMX 失败")
		return
	}

	data := dashboardData{
		BulmaCSS: template.CSS(cssBytes),
		HtmxJS:   template.JS(htmxBytes),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "dashboard", data); err != nil {
		slog.Error("Failed to render dashboard", "error", err)
	}
}

// containerListData holds data for the container list partial.
type containerListData struct {
	Containers []ContainerInfo
}

// handleContainerList renders the container list partial (HTMX).
func (h *DashboardHandler) handleContainerList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	containers, err := h.listContainers(ctx)
	if err != nil {
		slog.Error("获取容器列表失败", "error", err)
		h.renderError(w, http.StatusInternalServerError, "获取容器列表失败")
		return
	}

	data := containerListData{
		Containers: containers,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "container_list", data); err != nil {
		slog.Error("Failed to render container list", "error", err)
	}
}

// containerFormData holds data for the container form partial.
type containerFormData struct {
	Container *ContainerInfo
	Config    *StoredConfig
}

// handleContainerForm renders the container config form partial (HTMX).
func (h *DashboardHandler) handleContainerForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	containerID := chi.URLParam(r, "id")
	if containerID == "" {
		h.renderError(w, http.StatusBadRequest, "无效的容器 ID")
		return
	}

	container, err := h.getContainerByID(ctx, containerID)
	if err != nil {
		h.renderError(w, http.StatusNotFound, "未找到容器")
		return
	}

	// Get stored config or create default
	config := h.storage.Get(container.Key)
	if config == nil {
		// Create default config from container info
		config = h.createDefaultConfig(container)
	}

	data := containerFormData{
		Container: container,
		Config:    config,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "container_form", data); err != nil {
		slog.Error("Failed to render container form", "error", err)
	}
}

// handleContainerSave saves the container configuration.
func (h *DashboardHandler) handleContainerSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	containerID := chi.URLParam(r, "id")
	if containerID == "" {
		h.renderError(w, http.StatusBadRequest, "无效的容器 ID")
		return
	}

	// Get container to verify it exists and isn't label-configured
	container, err := h.getContainerByID(ctx, containerID)
	if err != nil {
		h.renderError(w, http.StatusNotFound, "未找到容器")
		return
	}

	if container.HasLabelConfig {
		h.renderError(w, http.StatusForbidden, "标签配置的容器无法修改")
		return
	}

	// Parse form (supports both multipart and urlencoded)
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			h.renderError(w, http.StatusBadRequest, "解析表单失败")
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			h.renderError(w, http.StatusBadRequest, "解析表单失败")
			return
		}
	}

	key := container.Key

	// Get existing config or create new
	config := h.storage.Get(key)
	if config == nil {
		config = &StoredConfig{
			Key:       key,
			CreatedAt: time.Now(),
		}
	}

	// Auto-generate appName (not user-editable)
	// Include first host port for uniqueness
	appName := "watchcow." + container.Name
	for _, hostPort := range container.Ports {
		appName = appName + "." + hostPort
		break
	}
	config.AppName = appName

	config.DisplayName = r.FormValue("display_name")
	config.Description = r.FormValue("description")
	config.Version = r.FormValue("version")
	config.Maintainer = r.FormValue("maintainer")
	config.UpdatedAt = time.Now()

	// Parse entries
	config.Entries = h.parseEntriesFromForm(r)

	// Validate defaults
	if config.DisplayName == "" {
		config.DisplayName = container.Name
	}
	if config.Version == "" {
		config.Version = "1.0.0"
	}
	if config.Maintainer == "" {
		config.Maintainer = "WatchCow"
	}

	// Handle icon upload if provided
	if file, header, err := r.FormFile("icon"); err == nil {
		defer file.Close()
		slog.Debug("Icon file received", "filename", header.Filename, "size", header.Size)
		if iconBase64, err := h.processIcon(file); err == nil {
			config.IconBase64 = iconBase64
			slog.Debug("Icon processed successfully", "base64_len", len(iconBase64))
		} else {
			slog.Warn("Failed to process icon", "error", err)
		}
	} else if err != http.ErrMissingFile {
		slog.Debug("FormFile error", "error", err)
	}

	// Save
	if err := h.storage.Set(config); err != nil {
		slog.Error("Failed to save config", "key", key, "error", err)
		h.renderError(w, http.StatusInternalServerError, "保存配置失败")
		return
	}

	slog.Info("Saved container config", "key", key, "appname", config.AppName, "has_icon", config.IconBase64 != "")

	// Trigger installation if container is running
	if h.trigger != nil {
		// Convert to docker.StoredConfig for trigger
		dockerConfig := h.convertToDockerConfig(config)
		h.trigger.TriggerInstall(containerID, dockerConfig)
	}

	// Return success message
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<article class="notification is-success">
	<p>配置已保存！</p>
	<button class="button is-small mt-2" hx-get="containers" hx-target="#main-content" hx-swap="innerHTML">返回列表</button>
</article>`))
}

// handleContainerDelete deletes the stored configuration.
func (h *DashboardHandler) handleContainerDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	containerID := chi.URLParam(r, "id")
	if containerID == "" {
		h.renderError(w, http.StatusBadRequest, "无效的容器 ID")
		return
	}

	// Get container to find its key
	container, err := h.getContainerByID(ctx, containerID)
	if err != nil {
		h.renderError(w, http.StatusNotFound, "未找到容器")
		return
	}

	key := container.Key

	// Get config before deletion to find app name
	config := h.storage.Get(key)
	appName := ""
	if config != nil {
		appName = config.AppName
	}

	if err := h.storage.Delete(key); err != nil {
		slog.Error("Failed to delete config", "key", key, "error", err)
		h.renderError(w, http.StatusInternalServerError, "删除配置失败")
		return
	}

	slog.Info("Deleted container config", "key", key)

	// Trigger uninstall if app was configured
	if appName != "" && h.trigger != nil {
		h.trigger.TriggerUninstall(appName)
	}

	// Return success message with button to go back
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<article class="notification is-success">
	<p>配置已删除！</p>
	<button class="button is-small mt-2" hx-get="containers" hx-target="#main-content" hx-swap="innerHTML">返回列表</button>
</article>`))
}

// processIcon validates an uploaded image and returns base64 encoded data.
// Image processing (square padding, resizing) is handled by fpkgen.handleIcons
// during app generation, keeping the install flow consistent with label-based icons.
func (h *DashboardHandler) processIcon(file io.Reader) (string, error) {
	imgData, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}

	// Validate it's a decodable image
	_, _, err = image.Decode(bytes.NewReader(imgData))
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}

	return base64.StdEncoding.EncodeToString(imgData), nil
}

// parseEntriesFromForm extracts entries from form data.
func (h *DashboardHandler) parseEntriesFromForm(r *http.Request) []StoredEntry {
	// Dashboard supports single entry only
	// For multi-entry, use Docker labels
	entry := StoredEntry{
		Name:     "", // Default entry
		Title:    r.FormValue("entry_title"),
		Protocol: r.FormValue("entry_protocol"),
		Port:     r.FormValue("entry_port"),
		Path:     r.FormValue("entry_path"),
		UIType:   r.FormValue("entry_ui_type"),
		AllUsers: r.FormValue("entry_all_users") == "true",
		Redirect: r.FormValue("entry_redirect"),
	}

	// Default protocol
	if entry.Protocol == "" {
		entry.Protocol = "http"
	}

	// Default path
	if entry.Path == "" {
		entry.Path = "/"
	}

	// Default UI type
	if entry.UIType == "" {
		entry.UIType = "url"
	}

	return []StoredEntry{entry}
}

// createDefaultConfig creates a default configuration for a container.
func (h *DashboardHandler) createDefaultConfig(container *ContainerInfo) *StoredConfig {
	config := &StoredConfig{
		Key:         container.Key,
		AppName:     "watchcow." + container.Name,
		DisplayName: container.Name,
		Description: container.Image,
		Version:     "1.0.0",
		Maintainer:  "WatchCow",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// Create default entry with first available port
	entry := StoredEntry{
		Protocol: "http",
		Path:     "/",
		UIType:   "url",
		AllUsers: true,
	}

	// Find first host port
	for _, hostPort := range container.Ports {
		entry.Port = hostPort
		break
	}

	config.Entries = []StoredEntry{entry}
	return config
}

// convertToDockerConfig converts server.StoredConfig to docker.StoredConfig.
func (h *DashboardHandler) convertToDockerConfig(config *StoredConfig) *docker.StoredConfig {
	result := &docker.StoredConfig{
		AppName:     config.AppName,
		DisplayName: config.DisplayName,
		Description: config.Description,
		Version:     config.Version,
		Maintainer:  config.Maintainer,
		IconBase64:  config.IconBase64,
		Entries:     make([]docker.StoredEntry, 0, len(config.Entries)),
	}

	for _, e := range config.Entries {
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

// renderError renders an error message.
func (h *DashboardHandler) renderError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<article class="notification is-danger">%s</article>`, template.HTMLEscapeString(msg))
}
