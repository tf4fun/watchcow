package fpkgen

// EntryControl represents permission settings for an entry
type EntryControl struct {
	AccessPerm string // "editable", "readonly", "hidden" - who can access setting
	PortPerm   string // "editable", "readonly", "hidden" - port setting permission
	PathPerm   string // "editable", "readonly", "hidden" - path setting permission
}

// Entry represents a single UI entry point
type Entry struct {
	Name      string        // Entry identifier (empty for default, "admin" for admin entry, etc.)
	Title     string        // Display title in UI config
	Protocol  string        // http or https
	Port      string        // service_port
	Path      string        // URL path
	UIType    string        // "url" or "iframe"
	AllUsers  bool          // Access permission
	Icon      string        // Icon URL or file path
	FileTypes []string      // Supported file types for right-click menu
	NoDisplay bool          // Hide from desktop (only show in right-click menu)
	Control   *EntryControl // Permission control settings
	Redirect  string        // External redirect host for CGI mode (watchcow.redirect)
}

// AppConfig holds all configuration for generating an fnOS app
type AppConfig struct {
	// Identity (matches fnOS manifest fields)
	AppName     string // manifest.appname - Unique app identifier
	Version     string // manifest.version - App version (e.g., "1.0.0")
	DisplayName string // manifest.display_name - Human-readable name
	Description string // manifest.desc - App description
	Maintainer  string // manifest.maintainer - Developer/maintainer name

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

	// Entries - UI entry points (supports multiple entries)
	Entries []Entry

	// Volumes
	Volumes []VolumeMapping

	// Environment
	Environment []string

	// Metadata
	Icon          string
	RestartPolicy string

	// Labels (original watchcow labels)
	Labels map[string]string
}

// VolumeMapping represents a container volume mount
type VolumeMapping struct {
	Source      string
	Destination string
	ReadOnly    bool
	Type        string // "bind" or "volume"
}
