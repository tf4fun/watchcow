package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html/template"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/image/draw"

	"watchcow/internal/docker"
	"watchcow/web"
)

// ContainerLister provides container listing capability.
type ContainerLister interface {
	ListAllContainers(ctx context.Context) ([]RawContainerInfo, error)
}

// InstallTrigger triggers app installation for containers.
type InstallTrigger interface {
	// TriggerInstall triggers app installation for a container using stored config.
	TriggerInstall(containerID string, storedConfig *docker.StoredConfig)
}

// RawContainerInfo is the raw container info from Docker.
type RawContainerInfo struct {
	ID     string
	Name   string
	Image  string
	State  string
	Ports  map[string]string
	Labels map[string]string
}

// DashboardHandler provides HTTP handlers for the dashboard.
type DashboardHandler struct {
	storage *DashboardStorage
	lister  ContainerLister
	trigger InstallTrigger
	tmpl    *template.Template
}

// NewDashboardHandler creates a new dashboard handler.
func NewDashboardHandler(storage *DashboardStorage, lister ContainerLister, trigger InstallTrigger) (*DashboardHandler, error) {
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
	r.Post("/containers/{id}/icon", h.handleIconUpload)
}

// listContainers fetches containers and enriches with storage info.
func (h *DashboardHandler) listContainers(ctx context.Context) ([]ContainerInfo, error) {
	raw, err := h.lister.ListAllContainers(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]ContainerInfo, 0, len(raw))
	for _, r := range raw {
		key := NewContainerKey(r.Image, r.Ports)
		hasLabelConfig := r.Labels["watchcow.enable"] == "true"
		hasStoredConfig := h.storage.Has(key)

		info := ContainerInfo{
			ID:              r.ID,
			Name:            r.Name,
			Image:           r.Image,
			State:           r.State,
			Ports:           r.Ports,
			Labels:          r.Labels,
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
	BulmaCSS   template.CSS
	HtmxJS     template.JS
	Containers []ContainerInfo
}

// handleDashboard renders the main dashboard page.
func (h *DashboardHandler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Load CSS
	cssBytes, err := web.Assets.ReadFile("css/bulma.min.css")
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load CSS")
		return
	}

	// Load HTMX JS
	htmxBytes, err := web.Assets.ReadFile("js/htmx.min.js")
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load HTMX")
		return
	}

	// Get containers
	containers, err := h.listContainers(ctx)
	if err != nil {
		slog.Error("Failed to list containers", "error", err)
		h.renderError(w, http.StatusInternalServerError, "Failed to list containers")
		return
	}

	data := dashboardData{
		BulmaCSS:   template.CSS(cssBytes),
		HtmxJS:     template.JS(htmxBytes),
		Containers: containers,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "dashboard", data); err != nil {
		slog.Error("Failed to render dashboard", "error", err)
	}
}

// containerListData holds data for the container list partial.
type containerListData struct {
	Containers []ContainerInfo
	Selected   string // Container ID
}

// handleContainerList renders the container list partial (HTMX).
func (h *DashboardHandler) handleContainerList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	containers, err := h.listContainers(ctx)
	if err != nil {
		slog.Error("Failed to list containers", "error", err)
		h.renderError(w, http.StatusInternalServerError, "Failed to list containers")
		return
	}

	selected := r.URL.Query().Get("selected")

	data := containerListData{
		Containers: containers,
		Selected:   selected,
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
		h.renderError(w, http.StatusBadRequest, "Invalid container ID")
		return
	}

	container, err := h.getContainerByID(ctx, containerID)
	if err != nil {
		h.renderError(w, http.StatusNotFound, "Container not found")
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
		h.renderError(w, http.StatusBadRequest, "Invalid container ID")
		return
	}

	// Get container to verify it exists and isn't label-configured
	container, err := h.getContainerByID(ctx, containerID)
	if err != nil {
		h.renderError(w, http.StatusNotFound, "Container not found")
		return
	}

	if container.HasLabelConfig {
		h.renderError(w, http.StatusForbidden, "Label-configured containers cannot be modified")
		return
	}

	// Parse form
	if err := r.ParseForm(); err != nil {
		h.renderError(w, http.StatusBadRequest, "Failed to parse form")
		return
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

	// Update config from form
	config.AppName = r.FormValue("appname")
	config.DisplayName = r.FormValue("display_name")
	config.Description = r.FormValue("description")
	config.Version = r.FormValue("version")
	config.Maintainer = r.FormValue("maintainer")
	config.UpdatedAt = time.Now()

	// Parse entries
	config.Entries = h.parseEntriesFromForm(r)

	// Validate
	if config.AppName == "" {
		config.AppName = "watchcow." + container.Name
	}
	if config.DisplayName == "" {
		config.DisplayName = container.Name
	}
	if config.Version == "" {
		config.Version = "1.0.0"
	}
	if config.Maintainer == "" {
		config.Maintainer = "WatchCow"
	}

	// Save
	if err := h.storage.Set(config); err != nil {
		slog.Error("Failed to save config", "key", key, "error", err)
		h.renderError(w, http.StatusInternalServerError, "Failed to save configuration")
		return
	}

	slog.Info("Saved container config", "key", key, "appname", config.AppName)

	// Trigger installation if container is running
	if h.trigger != nil {
		// Convert to docker.StoredConfig for trigger
		dockerConfig := h.convertToDockerConfig(config)
		h.trigger.TriggerInstall(containerID, dockerConfig)
	}

	// Return success message
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<article class="notification is-success">Configuration saved successfully!</article>`))
}

// handleContainerDelete deletes the stored configuration.
func (h *DashboardHandler) handleContainerDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	containerID := chi.URLParam(r, "id")
	if containerID == "" {
		h.renderError(w, http.StatusBadRequest, "Invalid container ID")
		return
	}

	// Get container to find its key
	container, err := h.getContainerByID(ctx, containerID)
	if err != nil {
		h.renderError(w, http.StatusNotFound, "Container not found")
		return
	}

	key := container.Key

	if err := h.storage.Delete(key); err != nil {
		slog.Error("Failed to delete config", "key", key, "error", err)
		h.renderError(w, http.StatusInternalServerError, "Failed to delete configuration")
		return
	}

	slog.Info("Deleted container config", "key", key)

	// Return empty response to clear the form
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<article class="notification is-info">Configuration deleted. Select a container from the list.</article>`))
}

// handleIconUpload handles icon upload and resizing.
func (h *DashboardHandler) handleIconUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	containerID := chi.URLParam(r, "id")
	if containerID == "" {
		h.renderError(w, http.StatusBadRequest, "Invalid container ID")
		return
	}

	// Get container to find its key
	container, err := h.getContainerByID(ctx, containerID)
	if err != nil {
		h.renderError(w, http.StatusNotFound, "Container not found")
		return
	}

	key := container.Key

	// Parse multipart form (max 10MB)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		h.renderError(w, http.StatusBadRequest, "Failed to parse upload")
		return
	}

	file, _, err := r.FormFile("icon")
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "No file uploaded")
		return
	}
	defer file.Close()

	// Read and decode image
	imgData, err := io.ReadAll(file)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Failed to read file")
		return
	}

	img, _, err := image.Decode(bytes.NewReader(imgData))
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid image format")
		return
	}

	// Resize to 256x256
	resized := resizeImage(img, 256, 256)

	// Encode as PNG
	var buf bytes.Buffer
	if err := png.Encode(&buf, resized); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to encode image")
		return
	}

	// Convert to base64
	base64Icon := base64.StdEncoding.EncodeToString(buf.Bytes())

	// Update config
	config := h.storage.Get(key)
	if config == nil {
		h.renderError(w, http.StatusNotFound, "Configuration not found, save configuration first")
		return
	}

	config.IconBase64 = base64Icon
	config.UpdatedAt = time.Now()

	if err := h.storage.Set(config); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to save icon")
		return
	}

	// Return icon preview
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<img src="data:image/png;base64,%s" alt="Icon" style="max-width: 64px; max-height: 64px;">`, base64Icon)
}

// parseEntriesFromForm extracts entries from form data.
func (h *DashboardHandler) parseEntriesFromForm(r *http.Request) []StoredEntry {
	// For now, support a single default entry
	// Multi-entry support can be added later
	entry := StoredEntry{
		Name:      "", // Default entry
		Title:     r.FormValue("entry_title"),
		Protocol:  r.FormValue("entry_protocol"),
		Port:      r.FormValue("entry_port"),
		Path:      r.FormValue("entry_path"),
		UIType:    r.FormValue("entry_ui_type"),
		AllUsers:  r.FormValue("entry_all_users") == "true",
		NoDisplay: r.FormValue("entry_no_display") == "true",
		Redirect:  r.FormValue("entry_redirect"),
	}

	// Parse file types
	if ft := r.FormValue("entry_file_types"); ft != "" {
		for _, t := range strings.Split(ft, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				entry.FileTypes = append(entry.FileTypes, t)
			}
		}
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

	// Find first port
	for containerPort := range container.Ports {
		entry.Port = containerPort
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

// resizeImage resizes an image to the specified dimensions.
func resizeImage(src image.Image, width, height int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}
