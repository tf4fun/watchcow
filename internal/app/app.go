// Package app provides the core App model for watchcow.
// App represents a watchcow-managed application, parsed from container labels.
package app

import "sync"

// Status represents the current state of an app
type Status string

const (
	StatusPending     Status = "pending"     // Waiting to be installed
	StatusInstalled   Status = "installed"   // Installed but not running
	StatusRunning     Status = "running"     // Running
	StatusStopped     Status = "stopped"     // Stopped
	StatusUninstalled Status = "uninstalled" // Uninstalled
)

// EntryControl represents permission settings for an entry
type EntryControl struct {
	AccessPerm string // "editable", "readonly", "hidden" - who can access setting
	PortPerm   string // "editable", "readonly", "hidden" - port setting permission
	PathPerm   string // "editable", "readonly", "hidden" - path setting permission
}

// RedirectConfig holds redirect configuration for an entry
type RedirectConfig struct {
	Host string // External redirect host (e.g., "https://example.com")
	Port string // Container port to use when on local network
}

// Entry represents a UI entry point for an app
type Entry struct {
	Name      string        // Entry identifier (empty for default entry)
	Title     string        // Display title
	Protocol  string        // http or https
	Port      string        // Service port
	Path      string        // URL path
	UIType    string        // "url" (new tab) or "iframe" (desktop window)
	AllUsers  bool          // Access permission (true = all users)
	Icon      string        // Icon source: URL (file:// or http://) from labels, or base64 data from dashboard
	FileTypes []string      // Supported file types for right-click menu
	NoDisplay bool          // Hide from desktop (only show in right-click menu)
	Control   *EntryControl // Permission control settings
	Redirect  string        // External redirect host for CGI mode
}

// GetRedirectConfig returns RedirectConfig if redirect is enabled, nil otherwise
func (e *Entry) GetRedirectConfig() *RedirectConfig {
	if e.Redirect == "" {
		return nil
	}
	return &RedirectConfig{
		Host: e.Redirect,
		Port: e.Port,
	}
}

// VolumeMapping represents a container volume mount
type VolumeMapping struct {
	Source      string
	Destination string
	ReadOnly    bool
	Type        string // "bind" or "volume"
}

// App represents a watchcow-managed application.
// This is the core model parsed from container labels, used for both
// package generation (fpkgen) and runtime management (monitor/server).
type App struct {
	// Identity (matches fnOS manifest fields)
	AppName     string // Unique app identifier (e.g., "watchcow.nginx")
	Version     string // App version (e.g., "1.0.0")
	DisplayName string // Human-readable name
	Description string // App description
	Maintainer  string // Developer/maintainer name

	// Container Info
	ContainerID   string
	ContainerName string
	Image         string

	// Network / UI (legacy single entry - kept for backward compatibility)
	Protocol string // http or https
	Port     string // service_port
	Path     string // URL path
	UIType   string // "url" (new tab) or "iframe" (desktop window)
	AllUsers bool   // true = all users can access, false = admin only

	// UI Entries (supports multiple entries)
	Entries []Entry

	// Volumes
	Volumes []VolumeMapping

	// Environment
	Environment []string

	// Metadata
	Icon          string // Icon source: URL (file:// or http://) from labels, or base64 data from dashboard
	RestartPolicy string

	// Labels (original watchcow labels for reference)
	Labels map[string]string

	// Runtime state (not from labels, managed by monitor)
	Status Status
}

// GetEntry returns the entry by name, or nil if not found.
// Pass empty string to get the default entry.
func (a *App) GetEntry(name string) *Entry {
	for i := range a.Entries {
		if a.Entries[i].Name == name {
			return &a.Entries[i]
		}
	}
	return nil
}

// GetDefaultEntry returns the default entry (name="") or the first entry if no default.
func (a *App) GetDefaultEntry() *Entry {
	if entry := a.GetEntry(""); entry != nil {
		return entry
	}
	if len(a.Entries) > 0 {
		return &a.Entries[0]
	}
	return nil
}

// HasRedirect returns true if any entry has redirect configured.
func (a *App) HasRedirect() bool {
	for _, entry := range a.Entries {
		if entry.Redirect != "" {
			return true
		}
	}
	return false
}

// Registry manages app instances with thread-safe access.
type Registry struct {
	apps sync.Map // map[string]*App (appName -> *App)
}

// NewRegistry creates a new app registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds or updates an app in the registry.
func (r *Registry) Register(app *App) {
	r.apps.Store(app.AppName, app)
}

// Unregister removes an app from the registry.
func (r *Registry) Unregister(appName string) {
	r.apps.Delete(appName)
}

// Get retrieves an app by name, returns nil if not found.
func (r *Registry) Get(appName string) *App {
	if v, ok := r.apps.Load(appName); ok {
		return v.(*App)
	}
	return nil
}

// GetByContainerID retrieves an app by container ID, returns nil if not found.
func (r *Registry) GetByContainerID(containerID string) *App {
	var found *App
	r.apps.Range(func(key, value any) bool {
		app := value.(*App)
		if app.ContainerID == containerID {
			found = app
			return false // stop iteration
		}
		return true
	})
	return found
}

// List returns all registered apps.
func (r *Registry) List() []*App {
	var apps []*App
	r.apps.Range(func(key, value any) bool {
		apps = append(apps, value.(*App))
		return true
	})
	return apps
}

// UpdateStatus updates the status of an app.
func (r *Registry) UpdateStatus(appName string, status Status) bool {
	if app := r.Get(appName); app != nil {
		app.Status = status
		return true
	}
	return false
}
