package fpkgen

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Generator handles fnOS application package generation from Docker containers
type Generator struct {
	dockerClient   *client.Client  // Docker API client
	templateEngine *TemplateEngine // Template engine for rendering
}

// NewGenerator creates a new application generator
func NewGenerator() (*Generator, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	// Initialize template engine
	tmplEngine, err := NewTemplateEngine()
	if err != nil {
		cli.Close()
		return nil, fmt.Errorf("failed to create template engine: %w", err)
	}

	return &Generator{
		dockerClient:   cli,
		templateEngine: tmplEngine,
	}, nil
}

// GenerateFromContainer creates fnOS app structure from a running container
// Returns the config, temp directory path (caller should clean up after install)
func (g *Generator) GenerateFromContainer(ctx context.Context, containerID string) (*AppConfig, string, error) {
	// 1. Inspect container for full details
	container, err := g.dockerClient.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to inspect container: %w", err)
	}

	// 2. Extract configuration from container
	config := g.extractConfig(&container)

	// 3. Create temp directory for app package
	appDir, err := os.MkdirTemp("", "watchcow-"+config.AppName+"-")
	if err != nil {
		return nil, "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	if err := g.createDirectoryStructure(appDir); err != nil {
		os.RemoveAll(appDir)
		return nil, "", fmt.Errorf("failed to create directory structure: %w", err)
	}

	// 4. Generate all files using templates
	slog.Info("Generating fnOS app package", "appName", config.AppName, "container", config.ContainerName)

	data := NewTemplateData(config)

	if err := g.generateFromTemplates(appDir, data); err != nil {
		return nil, "", err
	}

	if err := g.handleIcons(appDir, config); err != nil {
		return nil, "", fmt.Errorf("failed to handle icons: %w", err)
	}

	slog.Info("Successfully generated fnOS app package", "appDir", appDir)

	return config, appDir, nil
}

// GenerateFromConfig creates fnOS app structure from an AppConfig directly
// This is useful for testing/debugging without needing a real Docker container
func (g *Generator) GenerateFromConfig(config *AppConfig, appDir string) error {
	// Remove existing directory if exists
	if err := os.RemoveAll(appDir); err != nil {
		return fmt.Errorf("failed to remove existing directory: %w", err)
	}

	if err := g.createDirectoryStructure(appDir); err != nil {
		return fmt.Errorf("failed to create directory structure: %w", err)
	}

	// Generate all files using templates
	slog.Info("Generating fnOS app package from config", "appName", config.AppName)

	data := NewTemplateData(config)

	if err := g.generateFromTemplates(appDir, data); err != nil {
		return err
	}

	if err := g.handleIcons(appDir, config); err != nil {
		return fmt.Errorf("failed to handle icons: %w", err)
	}

	slog.Info("Successfully generated fnOS app package", "appDir", appDir)
	return nil
}

// generateFromTemplates generates all files using template engine
func (g *Generator) generateFromTemplates(appDir string, data *TemplateData) error {
	// Define template -> file mappings
	mappings := []struct {
		template string
		path     string
		perm     os.FileMode
	}{
		{"manifest.tmpl", "manifest", 0644},
		{"cmd_main.tmpl", "cmd/main", 0755},
		{"config_privilege.json.tmpl", "config/privilege", 0644},
		{"config_resource.json.tmpl", "config/resource", 0644},
		{"LICENSE.tmpl", "LICENSE", 0644},
	}

	for _, m := range mappings {
		filePath := filepath.Join(appDir, m.path)
		if err := g.templateEngine.RenderToFile(m.template, filePath, data, m.perm); err != nil {
			return fmt.Errorf("failed to generate %s: %w", m.path, err)
		}
	}

	// Generate UI config JSON directly (not using template)
	uiConfigPath := filepath.Join(appDir, "app", "ui", "config")
	uiConfigJSON, err := GenerateUIConfigJSON(data)
	if err != nil {
		return fmt.Errorf("failed to generate UI config: %w", err)
	}
	if err := os.WriteFile(uiConfigPath, uiConfigJSON, 0644); err != nil {
		return fmt.Errorf("failed to write UI config: %w", err)
	}

	// Generate install_callback with CGI symlink support
	installCallbackPath := filepath.Join(appDir, "cmd", "install_callback")
	if err := g.templateEngine.RenderToFile("cmd_install_callback.tmpl", installCallbackPath, data, 0755); err != nil {
		return fmt.Errorf("failed to generate cmd/install_callback: %w", err)
	}

	// Generate other empty cmd scripts
	cmdScripts := []string{"install_init", "uninstall_init", "uninstall_callback",
		"upgrade_init", "upgrade_callback", "config_init", "config_callback"}
	for _, script := range cmdScripts {
		filePath := filepath.Join(appDir, "cmd", script)
		if err := g.templateEngine.RenderToFile("cmd_empty.tmpl", filePath, data, 0755); err != nil {
			return fmt.Errorf("failed to generate cmd/%s: %w", script, err)
		}
	}

	return nil
}

// extractConfig extracts AppConfig from container inspection result
// Label naming follows fnOS manifest conventions:
//
//	watchcow.appname      -> manifest.appname
//	watchcow.display_name -> manifest.display_name
//	watchcow.desc         -> manifest.desc
//	watchcow.version      -> manifest.version
//	watchcow.maintainer   -> manifest.maintainer
//	watchcow.service_port -> manifest.service_port
//	watchcow.protocol     -> UI config (http/https)
//	watchcow.path         -> UI config (url path)
//	watchcow.icon         -> app icon URL
func (g *Generator) extractConfig(container *dockercontainer.InspectResponse) *AppConfig {
	name := strings.TrimPrefix(container.Name, "/")
	labels := container.Config.Labels

	// Generate sanitized app name
	sanitizedName := sanitizeAppName(name)
	appName := getLabel(labels, "watchcow.appname", fmt.Sprintf("watchcow.%s", sanitizedName))

	defaultIcon := getLabel(labels, "watchcow.icon", buildIconURLFromImage(container.Config.Image))
	displayName := getLabel(labels, "watchcow.display_name", prettifyName(name))

	config := &AppConfig{
		AppName:       appName,
		Version:       getLabel(labels, "watchcow.version", "1.0.0"),
		DisplayName:   displayName,
		Description:   getLabel(labels, "watchcow.desc", fmt.Sprintf("Docker container: %s", container.Config.Image)),
		Maintainer:    getLabel(labels, "watchcow.maintainer", "WatchCow"),
		ContainerID:   container.ID[:12],
		ContainerName: name,
		Image:         container.Config.Image,
		Protocol:      getLabel(labels, "watchcow.protocol", "http"),
		Port:          getLabel(labels, "watchcow.service_port", ""),
		Path:          getLabel(labels, "watchcow.path", "/"),
		UIType:        getLabel(labels, "watchcow.ui_type", "url"),
		AllUsers:      getLabel(labels, "watchcow.all_users", "true") == "true",
		Icon:          defaultIcon,
		Environment:   filterEnvironment(container.Config.Env),
		Labels:        labels,
	}

	// Extract port if not specified in label
	if config.Port == "" {
		config.Port = extractFirstPort(container)
	}

	// Parse multi-entry configuration
	config.Entries = ParseEntries(labels, displayName, defaultIcon, config.Port)

	// If no entries configured, create a default entry for backward compatibility
	if len(config.Entries) == 0 {
		config.Entries = []Entry{{
			Name:      "",
			Title:     displayName,
			Protocol:  config.Protocol,
			Port:      config.Port,
			Path:      config.Path,
			UIType:    config.UIType,
			AllUsers:  config.AllUsers,
			Icon:      defaultIcon,
			FileTypes: nil,
			NoDisplay: getLabel(labels, "watchcow.no_display", "false") == "true",
			Control:   nil,
			Redirect:  getLabel(labels, "watchcow.redirect", ""),
		}}
	}

	// Extract volumes
	for _, mount := range container.Mounts {
		config.Volumes = append(config.Volumes, VolumeMapping{
			Source:      mount.Source,
			Destination: mount.Destination,
			ReadOnly:    !mount.RW,
			Type:        string(mount.Type),
		})
	}

	// Extract restart policy
	if container.HostConfig.RestartPolicy.Name != "" {
		config.RestartPolicy = string(container.HostConfig.RestartPolicy.Name)
	} else {
		config.RestartPolicy = "unless-stopped"
	}

	return config
}

// createDirectoryStructure creates all required directories
func (g *Generator) createDirectoryStructure(appDir string) error {
	dirs := []string{
		appDir,
		filepath.Join(appDir, "app", "ui", "images"),
		filepath.Join(appDir, "cmd"),
		filepath.Join(appDir, "config"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	return nil
}

// Close closes the Docker client
func (g *Generator) Close() error {
	if g.dockerClient != nil {
		return g.dockerClient.Close()
	}
	return nil
}

// Helper functions

// sanitizeAppName ensures the app name conforms to fnOS requirements
func sanitizeAppName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	var result strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result.WriteRune(c)
		}
	}
	return result.String()
}

// getLabel gets a label value with fallback
func getLabel(labels map[string]string, key, fallback string) string {
	if val, ok := labels[key]; ok && val != "" {
		return val
	}
	return fallback
}

// filterEnvironment removes sensitive/unwanted environment variables
func filterEnvironment(env []string) []string {
	var filtered []string
	blacklist := []string{"PATH=", "HOME=", "USER=", "HOSTNAME=", "PWD=", "SHLVL="}

	for _, e := range env {
		skip := false
		for _, b := range blacklist {
			if strings.HasPrefix(e, b) {
				skip = true
				break
			}
		}
		if !skip {
			filtered = append(filtered, e)
		}
	}

	return filtered
}

// extractFirstPort extracts the first public port from container
func extractFirstPort(container *dockercontainer.InspectResponse) string {
	if container.HostConfig == nil {
		return ""
	}

	for _, bindings := range container.HostConfig.PortBindings {
		for _, binding := range bindings {
			if binding.HostPort != "" {
				return binding.HostPort
			}
		}
	}

	return ""
}

// getIconCDNTemplate returns the CDN template URL from environment variable
func getIconCDNTemplate() string {
	if tmpl := os.Getenv("WATCHCOW_ICON_CDN_TEMPLATE"); tmpl != "" {
		return tmpl
	}
	return "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/%s.png"
}

// getLocalIconPath checks if icon exists in local data-share folder
// Returns file:// URL if found, empty string otherwise
func getLocalIconPath(imageName string) string {
	dataSharePaths := os.Getenv("TRIM_DATA_SHARE_PATHS")
	if dataSharePaths == "" {
		return ""
	}

	// Try supported icon extensions
	extensions := []string{".png", ".jpg", ".jpeg", ".webp", ".bmp", ".ico"}
	for _, ext := range extensions {
		iconPath := filepath.Join(dataSharePaths, imageName+ext)
		if _, err := os.Stat(iconPath); err == nil {
			return "file://" + iconPath
		}
	}

	return ""
}

// buildIconURL builds icon URL from name
// Priority: local data-share > CDN template
// Returns empty string if no icon source available
func buildIconURL(name string) string {
	name = strings.ToLower(name)

	// Try local data-share first
	if localPath := getLocalIconPath(name); localPath != "" {
		return localPath
	}

	// Fall back to CDN
	cdnTemplate := getIconCDNTemplate()
	if cdnTemplate == "" {
		return ""
	}

	return fmt.Sprintf(cdnTemplate, name)
}

// buildIconURLFromImage builds icon URL from docker image name
func buildIconURLFromImage(image string) string {
	parts := strings.Split(image, "/")
	imageName := parts[len(parts)-1]
	imageName = strings.Split(imageName, ":")[0]
	return buildIconURL(imageName)
}

// prettifyName converts container name to a nice title
func prettifyName(name string) string {
	name = strings.TrimSuffix(name, "-1")
	name = strings.TrimSuffix(name, "_1")
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "-", " ")

	words := strings.Fields(name)
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}

	return strings.Join(words, " ")
}

// Multi-entry parsing functions

// entryFields defines which label suffixes are entry-specific configuration fields
var entryFields = map[string]bool{
	"service_port":        true,
	"protocol":            true,
	"path":                true,
	"ui_type":             true,
	"all_users":           true,
	"icon":                true,
	"title":               true,
	"file_types":          true,
	"no_display":          true,
	"control.access_perm": true,
	"control.port_perm":   true,
	"control.path_perm":   true,
	"redirect":            true,
}

// isEntryField checks if a field name is an entry configuration field
func isEntryField(field string) bool {
	if entryFields[field] {
		return true
	}
	// Also check for control.* prefix
	if strings.HasPrefix(field, "control.") {
		return true
	}
	return false
}

// hasDefaultEntry checks if there's a default entry configuration in labels
func hasDefaultEntry(labels map[string]string) bool {
	_, hasPort := labels["watchcow.service_port"]
	_, hasProtocol := labels["watchcow.protocol"]
	_, hasPath := labels["watchcow.path"]
	_, hasTitle := labels["watchcow.title"]
	_, hasUIType := labels["watchcow.ui_type"]
	return hasPort || hasProtocol || hasPath || hasTitle || hasUIType
}

// parseEntry parses a single entry from labels
// name: entry name (empty string for default entry)
// displayName: app display name for generating default title
// defaultIcon: fallback icon URL (used for default entry)
func parseEntry(labels map[string]string, name string, displayName string, defaultIcon string) Entry {
	prefix := "watchcow."
	if name != "" {
		prefix = "watchcow." + name + "."
	}

	// title default logic:
	// - default entry: use display_name
	// - named entry: use "display_name - entry_name"
	title := getLabel(labels, prefix+"title", "")
	if title == "" {
		if name == "" {
			title = displayName
		} else {
			title = displayName + " - " + name
		}
	}

	// icon default logic:
	// - default entry: use defaultIcon (based on image name)
	// - named entry: use entry name to build icon URL
	iconFallback := defaultIcon
	if name != "" {
		iconFallback = buildIconURL(name)
	}

	// Parse file types (comma-separated list)
	var fileTypes []string
	if ft := getLabel(labels, prefix+"file_types", ""); ft != "" {
		for _, t := range strings.Split(ft, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				fileTypes = append(fileTypes, t)
			}
		}
	}

	// Parse control settings
	var control *EntryControl
	accessPerm := getLabel(labels, prefix+"control.access_perm", "")
	portPerm := getLabel(labels, prefix+"control.port_perm", "")
	pathPerm := getLabel(labels, prefix+"control.path_perm", "")
	if accessPerm != "" || portPerm != "" || pathPerm != "" {
		control = &EntryControl{
			AccessPerm: accessPerm,
			PortPerm:   portPerm,
			PathPerm:   pathPerm,
		}
	}

	return Entry{
		Name:      name,
		Title:     title,
		Protocol:  getLabel(labels, prefix+"protocol", "http"),
		Port:      getLabel(labels, prefix+"service_port", ""),
		Path:      getLabel(labels, prefix+"path", "/"),
		UIType:    getLabel(labels, prefix+"ui_type", "url"),
		AllUsers:  getLabel(labels, prefix+"all_users", "true") == "true",
		Icon:      getLabel(labels, prefix+"icon", iconFallback),
		FileTypes: fileTypes,
		NoDisplay: getLabel(labels, prefix+"no_display", "false") == "true",
		Control:   control,
		Redirect:  getLabel(labels, prefix+"redirect", ""),
	}
}

// ParseEntries extracts all entries from container labels
func ParseEntries(labels map[string]string, displayName string, defaultIcon string, defaultPort string) []Entry {
	entries := []Entry{}
	entryNames := make(map[string]bool)

	// Scan all labels to identify named entries
	for key := range labels {
		if !strings.HasPrefix(key, "watchcow.") {
			continue
		}

		suffix := strings.TrimPrefix(key, "watchcow.")
		parts := strings.SplitN(suffix, ".", 2)

		// Check if this is a named entry field (e.g., "admin.service_port")
		if len(parts) == 2 && isEntryField(parts[1]) {
			entryNames[parts[0]] = true
		}
	}

	// Check for default entry configuration
	if hasDefaultEntry(labels) {
		entry := parseEntry(labels, "", displayName, defaultIcon)
		// Use container's first port as fallback if not specified
		if entry.Port == "" {
			entry.Port = defaultPort
		}
		entries = append(entries, entry)
	}

	// Parse named entries
	for name := range entryNames {
		entry := parseEntry(labels, name, displayName, defaultIcon)
		// Use container's first port as fallback if not specified
		if entry.Port == "" {
			entry.Port = defaultPort
		}
		entries = append(entries, entry)
	}

	return entries
}
