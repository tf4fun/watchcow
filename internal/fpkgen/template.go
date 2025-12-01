package fpkgen

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// TemplateEngine handles template loading and rendering
type TemplateEngine struct {
	templates map[string]*template.Template
}

// NewTemplateEngine creates a new template engine with embedded templates
func NewTemplateEngine() (*TemplateEngine, error) {
	engine := &TemplateEngine{
		templates: make(map[string]*template.Template),
	}

	// Load all embedded templates
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		return nil, fmt.Errorf("failed to read templates directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		content, err := templateFS.ReadFile("templates/" + name)
		if err != nil {
			return nil, fmt.Errorf("failed to read template %s: %w", name, err)
		}

		tmpl, err := template.New(name).Parse(string(content))
		if err != nil {
			return nil, fmt.Errorf("failed to parse template %s: %w", name, err)
		}

		engine.templates[name] = tmpl
	}

	return engine, nil
}

// Render renders a template with the given data
func (e *TemplateEngine) Render(templateName string, data interface{}) ([]byte, error) {
	tmpl, ok := e.templates[templateName]
	if !ok {
		return nil, fmt.Errorf("template not found: %s", templateName)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("failed to execute template %s: %w", templateName, err)
	}

	return buf.Bytes(), nil
}

// RenderToFile renders a template and writes to file
func (e *TemplateEngine) RenderToFile(templateName, filePath string, data interface{}, perm os.FileMode) error {
	content, err := e.Render(templateName, data)
	if err != nil {
		return err
	}

	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	return os.WriteFile(filePath, content, perm)
}

// ListTemplates returns all available template names
func (e *TemplateEngine) ListTemplates() []string {
	names := make([]string, 0, len(e.templates))
	for name := range e.templates {
		names = append(names, name)
	}
	return names
}

// UIConfigControl represents control settings in UI config JSON
type UIConfigControl struct {
	AccessPerm string `json:"accessPerm,omitempty"`
	PortPerm   string `json:"portPerm,omitempty"`
	PathPerm   string `json:"pathPerm,omitempty"`
}

// UIConfigEntry represents a single entry in UI config JSON
type UIConfigEntry struct {
	Title     string           `json:"title"`
	Icon      string           `json:"icon"`
	Type      string           `json:"type"`
	Protocol  string           `json:"protocol"`
	Port      string           `json:"port"`
	URL       string           `json:"url"`
	AllUsers  bool             `json:"allUsers"`
	FileTypes []string         `json:"fileTypes,omitempty"`
	NoDisplay bool             `json:"noDisplay,omitempty"`
	Control   *UIConfigControl `json:"control,omitempty"`
}

// UIConfig represents the complete UI config JSON structure
type UIConfig struct {
	URL map[string]*UIConfigEntry `json:".url"`
}

// GenerateUIConfigJSON generates the UI config JSON content
func GenerateUIConfigJSON(data *TemplateData) ([]byte, error) {
	config := &UIConfig{
		URL: make(map[string]*UIConfigEntry),
	}

	for _, entry := range data.Entries {
		var control *UIConfigControl
		if entry.Control != nil {
			control = &UIConfigControl{
				AccessPerm: entry.Control.AccessPerm,
				PortPerm:   entry.Control.PortPerm,
				PathPerm:   entry.Control.PathPerm,
			}
		}

		config.URL[entry.FullName] = &UIConfigEntry{
			Title:     entry.Title,
			Icon:      entry.Icon,
			Type:      entry.UIType,
			Protocol:  entry.Protocol,
			Port:      entry.Port,
			URL:       entry.Path,
			AllUsers:  entry.AllUsers,
			FileTypes: entry.FileTypes,
			NoDisplay: entry.NoDisplay,
			Control:   control,
		}
	}

	return json.MarshalIndent(config, "", "    ")
}

// EntryControlData holds permission control data for template rendering
type EntryControlData struct {
	AccessPerm string
	PortPerm   string
	PathPerm   string
}

// EntryData holds data for a single UI entry in template rendering
type EntryData struct {
	Name      string // Entry name (empty for default)
	FullName  string // Full entry name: AppName or AppName.EntryName
	Title     string // Display title
	Protocol  string
	Port      string
	Path      string
	UIType    string
	AllUsers  bool
	Icon      string            // Icon path (e.g., "images/icon_{0}.png")
	FileTypes []string          // Supported file types
	NoDisplay bool              // Hide from desktop
	Control   *EntryControlData // Permission control
}

// TemplateData holds all data needed for template rendering
type TemplateData struct {
	// Identity
	AppName     string
	Version     string
	DisplayName string
	Description string
	Maintainer  string

	// Container
	ContainerID   string
	ContainerName string
	Image         string

	// Network/UI (legacy single entry - kept for backward compatibility)
	Protocol string
	Port     string
	Path     string
	UIType   string
	AllUsers bool

	// Multi-entry support
	Entries            []EntryData
	DefaultLaunchEntry string // The entry name to use for desktop_applaunchname (first entry's FullName)

	// Collections
	Ports       []string
	Volumes     []VolumeMapping
	Environment []string

	// Other
	RestartPolicy string
	Icon          string
}

// NewTemplateData creates TemplateData from AppConfig
func NewTemplateData(config *AppConfig) *TemplateData {
	data := &TemplateData{
		AppName:       config.AppName,
		Version:       config.Version,
		DisplayName:   config.DisplayName,
		Description:   escapeForTemplate(config.Description),
		Maintainer:    config.Maintainer,
		ContainerID:   config.ContainerID,
		ContainerName: config.ContainerName,
		Image:         config.Image,
		Protocol:      config.Protocol,
		Port:          config.Port,
		Path:          config.Path,
		UIType:        config.UIType,
		AllUsers:      config.AllUsers,
		Volumes:       config.Volumes,
		Environment:   config.Environment,
		RestartPolicy: config.RestartPolicy,
		Icon:          config.Icon,
	}

	// Set defaults
	if data.UIType == "" {
		data.UIType = "url"
	}
	if data.Protocol == "" {
		data.Protocol = "http"
	}
	if data.Path == "" {
		data.Path = "/"
	}
	if data.RestartPolicy == "" {
		data.RestartPolicy = "unless-stopped"
	}

	// Build ports list
	if config.Port != "" {
		data.Ports = []string{config.Port + ":" + config.Port}
	}

	// Convert entries to template data
	var defaultLaunchEntry string
	for _, entry := range config.Entries {
		// Generate icon filename: "icon_{0}.png" for default, "icon_<name>_{0}.png" for named
		iconFilename := "icon_{0}.png"
		if entry.Name != "" {
			iconFilename = "icon_" + entry.Name + "_{0}.png"
		}

		// Generate full entry name: AppName for default, AppName.EntryName for named
		fullName := config.AppName
		if entry.Name != "" {
			fullName = config.AppName + "." + entry.Name
		}

		// Apply defaults for entry fields
		protocol := entry.Protocol
		if protocol == "" {
			protocol = "http"
		}
		path := entry.Path
		if path == "" {
			path = "/"
		}
		uiType := entry.UIType
		if uiType == "" {
			uiType = "url"
		}

		// Convert control settings
		var controlData *EntryControlData
		if entry.Control != nil {
			controlData = &EntryControlData{
				AccessPerm: entry.Control.AccessPerm,
				PortPerm:   entry.Control.PortPerm,
				PathPerm:   entry.Control.PathPerm,
			}
		}

		data.Entries = append(data.Entries, EntryData{
			Name:      entry.Name,
			FullName:  fullName,
			Title:     entry.Title,
			Protocol:  protocol,
			Port:      entry.Port,
			Path:      path,
			UIType:    uiType,
			AllUsers:  entry.AllUsers,
			Icon:      "images/" + iconFilename,
			FileTypes: entry.FileTypes,
			NoDisplay: entry.NoDisplay,
			Control:   controlData,
		})

		// Track first displayable entry for default launch entry
		if defaultLaunchEntry == "" && !entry.NoDisplay {
			defaultLaunchEntry = fullName
		}
	}

	// Set default launch entry to first displayable entry's full name
	data.DefaultLaunchEntry = defaultLaunchEntry

	return data
}

// escapeForTemplate escapes special characters for template output
func escapeForTemplate(s string) string {
	// For manifest, replace newlines
	s = replaceAll(s, "\n", " ")
	return s
}

func replaceAll(s, old, new string) string {
	for {
		idx := indexOf(s, old)
		if idx < 0 {
			break
		}
		s = s[:idx] + new + s[idx+len(old):]
	}
	return s
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
