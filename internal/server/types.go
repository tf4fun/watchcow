// Package dashboard provides the web dashboard for configuring Docker containers as fnOS apps.
package server

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ContainerKey uniquely identifies a container by its image and port mappings.
// Format: "image|containerPort:hostPort,containerPort:hostPort,..." (ports sorted)
// Example: "nginx:alpine|80:8080,443:8443"
type ContainerKey string

// NewContainerKey creates a ContainerKey from image and port mappings.
// The ports map is containerPort -> hostPort.
func NewContainerKey(image string, ports map[string]string) ContainerKey {
	if len(ports) == 0 {
		return ContainerKey(image + "|")
	}

	// Sort port mappings by container port for consistent keys
	var portPairs []string
	for containerPort, hostPort := range ports {
		portPairs = append(portPairs, fmt.Sprintf("%s:%s", containerPort, hostPort))
	}
	sort.Strings(portPairs)

	return ContainerKey(image + "|" + strings.Join(portPairs, ","))
}

// String returns the string representation of the key.
func (k ContainerKey) String() string {
	return string(k)
}

// Image returns the image part of the key.
func (k ContainerKey) Image() string {
	parts := strings.SplitN(string(k), "|", 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// StoredEntry represents a saved entry configuration.
type StoredEntry struct {
	Name       string   // Entry identifier (empty for default entry)
	Title      string   // Display title
	Protocol   string   // http or https
	Port       string   // Service port
	Path       string   // URL path
	UIType     string   // "url" (new tab) or "iframe" (desktop window)
	AllUsers   bool     // Access permission (true = all users)
	FileTypes  []string // Supported file types for right-click menu
	NoDisplay  bool     // Hide from desktop
	Redirect   string   // External redirect host
	IconBase64 string   // Base64-encoded PNG icon for this entry
}

// StoredConfig represents a saved container configuration.
type StoredConfig struct {
	Key         ContainerKey  // Unique container identifier
	AppName     string        // Unique app identifier
	DisplayName string        // Human-readable name
	Description string        // App description
	Version     string        // App version
	Maintainer  string        // Maintainer name
	Entries     []StoredEntry // UI entries
	IconBase64  string        // Base64-encoded PNG icon
	CreatedAt   time.Time     // When config was created
	UpdatedAt   time.Time     // When config was last updated
}

// ContainerInfo represents runtime container information.
type ContainerInfo struct {
	ID              string            // Container ID (truncated)
	Name            string            // Container name
	Image           string            // Image name
	State           string            // Container state (running, stopped, etc.)
	Ports           map[string]string // containerPort -> hostPort
	Labels          map[string]string // Container labels
	Key             ContainerKey      // Computed container key
	HasLabelConfig  bool              // watchcow.enable=true in labels
	HasStoredConfig bool              // Has config in dashboard storage
	Config          *StoredConfig     // Merged config (labels take priority)
}

// IsConfigurable returns true if the container can be configured via dashboard.
// Label-configured containers cannot be modified via dashboard.
func (c *ContainerInfo) IsConfigurable() bool {
	return !c.HasLabelConfig
}

// IsEnabled returns true if the container is enabled for watchcow.
func (c *ContainerInfo) IsEnabled() bool {
	return c.HasLabelConfig || c.HasStoredConfig
}
