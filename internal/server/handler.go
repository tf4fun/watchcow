package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"watchcow/internal/app"
	"watchcow/internal/docker"
	"watchcow/web"
)

const (
	maxDashboardIconBytes = 10 << 20
	maxDashboardFormBytes = maxDashboardIconBytes + (1 << 20)
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
	TriggerUninstall(containerID, appName, configKey string)
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
	legacyCandidates := make([][]ContainerKey, len(containers))
	namedCandidates := make([][]ContainerKey, len(containers))
	legacyClaims := make(map[ContainerKey]int)
	exactKeys := make(map[ContainerKey]bool)
	for i, c := range containers {
		hasCanonicalIdentity := c.IdentityPorts != nil
		identityPorts := c.IdentityPorts
		if identityPorts == nil {
			identityPorts = c.Ports
		}
		key := NewContainerKey(c.Image, identityPorts)
		if hasCanonicalIdentity {
			key = NewContainerKeyForContainer(c.Image, c.Name, identityPorts)
		}
		legacyCandidates[i] = h.storage.FindCompatibleKeys(c.Image, identityPorts, c.LegacyPortOptions)
		namedCandidates[i] = h.storage.FindNamedKeys(c.Name)
		hasLabelConfig := c.Labels["watchcow.enable"] == "true"

		info := ContainerInfo{
			ID:             c.ID,
			Name:           c.Name,
			Image:          c.Image,
			State:          c.State,
			Ports:          c.Ports,
			Labels:         c.Labels,
			NetworkMode:    c.NetworkMode,
			Key:            key,
			HasLabelConfig: hasLabelConfig,
			ConfigKey:      key,
		}
		if h.storage.Has(key) {
			exactKeys[key] = true
		}
		result = append(result, info)
	}
	for i, candidates := range legacyCandidates {
		filtered := candidates[:0]
		for _, candidate := range candidates {
			if exactKeys[candidate] {
				continue
			}
			filtered = append(filtered, candidate)
			legacyClaims[candidate]++
		}
		legacyCandidates[i] = filtered
	}

	for i := range result {
		info := &result[i]
		if h.storage.Has(info.Key) {
			info.HasStoredConfig = true
			info.Config = h.storage.Get(info.Key)
			for _, candidate := range legacyCandidates[i] {
				if legacyClaims[candidate] != 1 || isNamedContainerKey(candidate) {
					continue
				}
				if candidateConfig := h.storage.Get(candidate); candidateConfig != nil && candidateConfig.AppName == info.Config.AppName {
					info.LegacyConfigKeys = appendUniqueContainerKey(info.LegacyConfigKeys, candidate)
				} else {
					info.LegacyConfigConflict = true
				}
			}
			for _, candidate := range namedCandidates[i] {
				if candidate == info.Key {
					continue
				}
				if candidateConfig := h.storage.Get(candidate); candidateConfig != nil && candidateConfig.AppName == info.Config.AppName {
					info.LegacyConfigKeys = appendUniqueContainerKey(info.LegacyConfigKeys, candidate)
				} else {
					info.LegacyConfigConflict = true
				}
			}
			continue
		}
		if len(namedCandidates[i]) == 1 {
			info.ConfigKey = namedCandidates[i][0]
			info.LegacyConfigKeys = []ContainerKey{namedCandidates[i][0]}
			info.HasStoredConfig = true
			info.Config = h.storage.Get(info.ConfigKey)
			continue
		}
		if len(namedCandidates[i]) > 1 {
			info.LegacyConfigConflict = true
			info.LegacyConfigKeys = append(info.LegacyConfigKeys, namedCandidates[i]...)
			continue
		}
		if len(legacyCandidates[i]) == 1 && legacyClaims[legacyCandidates[i][0]] == 1 {
			info.ConfigKey = legacyCandidates[i][0]
			info.LegacyConfigKeys = []ContainerKey{legacyCandidates[i][0]}
			info.HasStoredConfig = true
			info.Config = h.storage.Get(info.ConfigKey)
		} else if len(legacyCandidates[i]) > 0 {
			info.LegacyConfigConflict = true
			for _, candidate := range legacyCandidates[i] {
				if legacyClaims[candidate] == 1 {
					info.LegacyConfigKeys = append(info.LegacyConfigKeys, candidate)
				}
			}
		}
	}

	// Sort by name
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

func appendUniqueContainerKey(keys []ContainerKey, candidate ContainerKey) []ContainerKey {
	for _, key := range keys {
		if key == candidate {
			return keys
		}
	}
	return append(keys, candidate)
}

func isNamedContainerKey(key ContainerKey) bool {
	_, identity, ok := strings.Cut(string(key), "|")
	return ok && strings.HasPrefix(identity, "@")
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
	ThemeCSS template.CSS
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
	themeBytes, err := web.Assets.ReadFile("css/dashboard.css")
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "加载主题样式失败")
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
		ThemeCSS: template.CSS(themeBytes),
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
	Entry     *StoredEntry
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
	config := h.storage.Get(container.ConfigKey)
	if config == nil {
		// Create default config from container info
		config = h.createDefaultConfig(container)
	}

	entry := defaultDashboardEntry(config, container)
	data := containerFormData{
		Container: container,
		Config:    config,
		Entry:     &entry,
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
	if container.LegacyConfigConflict && !container.HasStoredConfig {
		h.renderError(w, http.StatusConflict, "检测到无法唯一归属的旧版配置，请停止或移除冲突容器后重试")
		return
	}

	// Parse form (supports both multipart and urlencoded)
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, maxDashboardFormBytes)
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			h.renderError(w, http.StatusBadRequest, "解析表单失败")
			return
		}
		defer r.MultipartForm.RemoveAll()
	} else {
		if err := r.ParseForm(); err != nil {
			h.renderError(w, http.StatusBadRequest, "解析表单失败")
			return
		}
	}

	entry := h.parseEntryFromForm(r)
	if err := validateDashboardEntry(entry); err != nil {
		h.renderError(w, http.StatusBadRequest, err.Error())
		return
	}

	var uploadedIcon string
	if strings.HasPrefix(contentType, "multipart/form-data") {
		file, header, err := r.FormFile("icon")
		switch {
		case err == nil:
			defer file.Close()
			slog.Debug("Icon file received", "filename", header.Filename, "size", header.Size)
			uploadedIcon, err = h.processIcon(file)
			if err != nil {
				slog.Warn("Failed to process icon", "error", err)
				h.renderError(w, http.StatusBadRequest, "图标文件无效")
				return
			}
		case err != http.ErrMissingFile:
			slog.Warn("Failed to read icon upload", "error", err)
			h.renderError(w, http.StatusBadRequest, "读取图标失败")
			return
		}
	}

	key := container.Key

	// Get existing config or create new
	config := h.storage.Get(container.ConfigKey)
	isNew := config == nil
	var previousContentRevision string
	retryApply := false
	if !isNew {
		previousContentRevision = dashboardConfigRevision(config)
		retryApply = config.Deleting || (config.Pending && config.LastError != "")
	}
	if config == nil {
		config = &StoredConfig{
			Key:       key,
			CreatedAt: time.Now(),
		}
	}
	config.Key = key

	// AppName is an installed-package identity. Generate it once and preserve it
	// across later edits and port changes.
	if config.AppName == "" {
		config.AppName = app.DashboardAppName(container.Name, firstHostPort(container.Ports))
	}

	config.DisplayName = r.FormValue("display_name")
	config.Description = r.FormValue("description")
	config.Version = r.FormValue("version")
	config.Maintainer = r.FormValue("maintainer")

	// The dashboard edits the unnamed compatibility entry. Preserve fields and
	// named entries that are not represented by the current form.
	config.Entries = mergeDashboardEntries(config.Entries, entry)

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

	if uploadedIcon != "" {
		config.IconBase64 = uploadedIcon
		// Older dashboard configs may carry an entry-level icon. Keep named-entry
		// icons intact, but make a newly uploaded app icon authoritative for the
		// unnamed entry shown by this form.
		for i := range config.Entries {
			if config.Entries[i].Name == "" {
				if config.Entries[i].IconBase64 != uploadedIcon {
					config.Entries[i].IconBase64 = ""
				}
				break
			}
		}
		slog.Debug("Icon processed successfully", "base64_len", len(uploadedIcon))
	}
	// Saving cancels any previous delete intent and acknowledges the last error;
	// content changes below will schedule a fresh apply.
	config.Deleting = false
	config.LastError = ""

	contentRevision := dashboardConfigRevision(config)
	contentChanged := isNew || contentRevision != previousContentRevision
	applyNeeded := contentChanged || retryApply
	if applyNeeded {
		config.UpdatedAt = time.Now()
		config.Pending = true
		config.Revision = contentRevision
	}

	// Save
	if err := h.storage.Replace(container.LegacyConfigKeys, config); err != nil {
		slog.Error("Failed to save config", "key", key, "error", err)
		h.renderError(w, http.StatusInternalServerError, "保存配置失败")
		return
	}

	slog.Info("Saved container config", "key", key, "appname", config.AppName, "has_icon", config.IconBase64 != "", "content_changed", contentChanged, "retry", retryApply)

	// Trigger installation for new content or an explicit retry/cancel-delete save.
	if applyNeeded && h.trigger != nil {
		// Convert to docker.StoredConfig for trigger
		dockerConfig := h.convertToDockerConfig(config)
		h.trigger.TriggerInstall(containerID, dockerConfig)
	}

	// Return success message
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<article class="notification is-success">
	<p>配置已保存！</p>
	<button class="button is-small mt-2" hx-get="containers" hx-target="#main-content" hx-swap="innerHTML show:top">返回列表</button>
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
	if container.HasLabelConfig {
		h.renderError(w, http.StatusForbidden, "标签配置的容器无法从 Dashboard 卸载")
		return
	}
	if container.LegacyConfigConflict && !container.HasStoredConfig && len(container.LegacyConfigKeys) == 0 {
		h.renderError(w, http.StatusConflict, "旧版配置同时匹配多个容器，无法安全删除；请先停止或移除冲突容器")
		return
	}

	deleteKeys := append([]ContainerKey{container.Key}, container.LegacyConfigKeys...)
	appKeys := make(map[string]ContainerKey)
	for _, key := range deleteKeys {
		if config := h.storage.Get(key); config != nil && config.AppName != "" {
			appKeys[config.AppName] = key
		}
	}
	if len(appKeys) == 0 {
		h.renderError(w, http.StatusNotFound, "未找到可删除的配置")
		return
	}
	if err := h.storage.MarkDeleting(deleteKeys); err != nil {
		slog.Error("Failed to persist delete intent", "key", container.ConfigKey, "error", err)
		h.renderError(w, http.StatusInternalServerError, "提交删除操作失败")
		return
	}

	slog.Info("Queued container config deletion", "key", container.ConfigKey)

	if h.trigger != nil {
		for appName, key := range appKeys {
			h.trigger.TriggerUninstall(containerID, appName, string(key))
		}
	} else {
		for appName := range appKeys {
			if err := h.storage.CompleteDelete(appName); err != nil {
				h.renderError(w, http.StatusInternalServerError, "删除配置失败")
				return
			}
		}
	}

	// Return success message with button to go back
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<article class="notification is-info">
	<p>删除任务已提交。</p>
	<button class="button is-small mt-2" hx-get="containers" hx-target="#main-content" hx-swap="innerHTML show:top">返回列表</button>
</article>`))
}

// processIcon validates an uploaded image and returns base64 encoded data.
// Image processing (square padding, resizing) is handled by fpkgen.handleIcons
// during app generation, keeping the install flow consistent with label-based icons.
func (h *DashboardHandler) processIcon(file io.Reader) (string, error) {
	imgData, err := readLimited(file, maxDashboardIconBytes)
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

func readLimited(r io.Reader, maxBytes int64) ([]byte, error) {
	limited := &io.LimitedReader{R: r, N: maxBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds maximum size of %d bytes", maxBytes)
	}
	return data, nil
}

// parseEntryFromForm extracts the unnamed entry from form data.
func (h *DashboardHandler) parseEntryFromForm(r *http.Request) StoredEntry {
	entry := StoredEntry{
		Name:          "",
		Title:         r.FormValue("entry_title"),
		Protocol:      r.FormValue("entry_protocol"),
		Port:          r.FormValue("entry_port"),
		Path:          r.FormValue("entry_path"),
		UIType:        r.FormValue("entry_ui_type"),
		AllUsers:      r.FormValue("entry_all_users") == "true",
		Redirect:      r.FormValue("entry_redirect"),
		ForceExternal: r.FormValue("entry_redirect_force_external") == "true",
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

	return entry
}

func validateDashboardEntry(entry StoredEntry) error {
	if entry.Protocol != "http" && entry.Protocol != "https" {
		return fmt.Errorf("协议必须是 http 或 https")
	}
	if entry.UIType != "url" && entry.UIType != "iframe" {
		return fmt.Errorf("打开方式必须是 url 或 iframe")
	}
	if entry.Redirect != "" {
		redirectURL, err := url.ParseRequestURI(entry.Redirect)
		if err != nil || redirectURL.Host == "" || (redirectURL.Scheme != "http" && redirectURL.Scheme != "https") {
			return fmt.Errorf("外部跳转地址必须是有效的 HTTP(S) URL")
		}
	}
	if entry.Port == "" {
		if strings.TrimSpace(entry.Redirect) == "" {
			return fmt.Errorf("普通入口必须填写端口")
		}
		return nil
	}

	port, err := strconv.Atoi(entry.Port)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("端口必须是 1 到 65535 之间的整数")
	}
	return nil
}

func mergeDashboardEntries(existing []StoredEntry, submitted StoredEntry) []StoredEntry {
	result := make([]StoredEntry, 0, max(1, len(existing)))
	updatedDefault := false
	for _, entry := range existing {
		if entry.Name == "" && !updatedDefault {
			entry.Title = submitted.Title
			entry.Protocol = submitted.Protocol
			entry.Port = submitted.Port
			entry.Path = submitted.Path
			entry.UIType = submitted.UIType
			entry.AllUsers = submitted.AllUsers
			entry.Redirect = submitted.Redirect
			entry.ForceExternal = submitted.ForceExternal
			updatedDefault = true
		}
		entry.FileTypes = append([]string(nil), entry.FileTypes...)
		result = append(result, entry)
	}
	if !updatedDefault {
		result = append([]StoredEntry{submitted}, result...)
	}
	return result
}

func defaultDashboardEntry(config *StoredConfig, container *ContainerInfo) StoredEntry {
	for _, entry := range config.Entries {
		if entry.Name == "" {
			entry.FileTypes = append([]string(nil), entry.FileTypes...)
			return entry
		}
	}
	return createDefaultEntry(container)
}

func createDefaultEntry(container *ContainerInfo) StoredEntry {
	return StoredEntry{
		Protocol: "http",
		Port:     firstHostPort(container.Ports),
		Path:     "/",
		UIType:   "url",
		AllUsers: true,
	}
}

// createDefaultConfig creates a default configuration for a container.
func (h *DashboardHandler) createDefaultConfig(container *ContainerInfo) *StoredConfig {
	config := &StoredConfig{
		Key:         container.Key,
		AppName:     app.DashboardAppName(container.Name, firstHostPort(container.Ports)),
		DisplayName: container.Name,
		Description: container.Image,
		Version:     "1.0.0",
		Maintainer:  "WatchCow",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	config.Entries = []StoredEntry{createDefaultEntry(container)}
	return config
}

// convertToDockerConfig converts server.StoredConfig to docker.StoredConfig.
func (h *DashboardHandler) convertToDockerConfig(config *StoredConfig) *docker.StoredConfig {
	result := &docker.StoredConfig{
		Key:         string(config.Key),
		AppName:     config.AppName,
		DisplayName: config.DisplayName,
		Description: config.Description,
		Version:     config.Version,
		Maintainer:  config.Maintainer,
		IconBase64:  config.IconBase64,
		Revision:    config.Revision,
		Pending:     config.Pending,
		Deleting:    config.Deleting,
		LastError:   config.LastError,
		Entries:     make([]docker.StoredEntry, 0, len(config.Entries)),
	}

	for _, e := range config.Entries {
		converted := docker.StoredEntry(e)
		converted.FileTypes = append([]string(nil), e.FileTypes...)
		result.Entries = append(result.Entries, converted)
	}

	return result
}

func dashboardConfigRevision(config *StoredConfig) string {
	snapshot := struct {
		AppName     string
		DisplayName string
		Description string
		Version     string
		Maintainer  string
		Entries     []StoredEntry
		IconBase64  string
	}{
		AppName:     config.AppName,
		DisplayName: config.DisplayName,
		Description: config.Description,
		Version:     config.Version,
		Maintainer:  config.Maintainer,
		Entries:     config.Entries,
		IconBase64:  config.IconBase64,
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:16])
}

// renderError renders an error message.
func (h *DashboardHandler) renderError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<article class="notification is-danger">%s</article>`, template.HTMLEscapeString(msg))
}
