// Package dashboard provides the web dashboard for configuring Docker containers as fnOS apps.
package server

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ContainerKey uniquely identifies a container by its image and port mappings.
// Format: "image|@containerName;containerPort/protocol:hostPort,..." (ports sorted).
// Legacy keys may omit the protocol.
// Example: "nginx:alpine|@nginx;80/tcp:8080,443/tcp:8443"
type ContainerKey string

// NewContainerKey creates a ContainerKey from image and port mappings.
// The ports map is containerPort -> hostPort.
func NewContainerKey(image string, ports map[string]string) ContainerKey {
	return NewContainerKeyForContainer(image, "", ports)
}

// NewContainerKeyForContainer includes the container name so same-image
// containers cannot collide even when their published ports are identical.
func NewContainerKeyForContainer(image, containerName string, ports map[string]string) ContainerKey {
	if containerName != "" {
		key := image + "|@" + strings.TrimPrefix(containerName, "/")
		if len(ports) == 0 {
			return ContainerKey(key)
		}
		return ContainerKey(key + ";" + encodeContainerPorts(ports))
	}
	if len(ports) == 0 {
		return ContainerKey(image + "|")
	}
	return ContainerKey(image + "|" + encodeContainerPorts(ports))
}

func encodeContainerPorts(ports map[string]string) string {
	// Sort port mappings by container port for consistent keys
	var portPairs []string
	for containerPort, hostPort := range ports {
		portPairs = append(portPairs, fmt.Sprintf("%s:%s", containerPort, hostPort))
	}
	sort.Strings(portPairs)

	return strings.Join(portPairs, ",")
}

func firstHostPort(ports map[string]string) string {
	containerPorts := make([]string, 0, len(ports))
	for containerPort := range ports {
		containerPorts = append(containerPorts, containerPort)
	}
	sort.Slice(containerPorts, func(i, j int) bool {
		iPort, iErr := strconv.Atoi(containerPorts[i])
		jPort, jErr := strconv.Atoi(containerPorts[j])
		if iErr == nil && jErr == nil && iPort != jPort {
			return iPort < jPort
		}
		return containerPorts[i] < containerPorts[j]
	})
	if len(containerPorts) == 0 {
		return ""
	}
	return ports[containerPorts[0]]
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
	Name          string   // Entry identifier (empty for default entry)
	Title         string   // Display title
	Protocol      string   // http or https
	Port          string   // Service port
	Path          string   // URL path
	UIType        string   // "url" (new tab) or "iframe" (desktop window)
	AllUsers      bool     // Access permission (true = all users)
	FileTypes     []string // Supported file types for right-click menu
	NoDisplay     bool     // Hide from desktop
	Redirect      string   // External redirect host
	ForceExternal bool     // Skip local network detection, always redirect to external URL
	IconBase64    string   // Base64-encoded PNG icon for this entry
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
	Revision    string        // Content revision used to acknowledge the applied package
	Pending     bool          // Desired config has not yet been successfully installed
	Deleting    bool          // Config remains until the fnOS package is actually removed
	LastError   string        // Last asynchronous apply/delete failure
	CreatedAt   time.Time     // When config was created
	UpdatedAt   time.Time     // When config was last updated
}

// ContainerInfo represents runtime container information.
type ContainerInfo struct {
	ID                   string            // Container ID (truncated)
	Name                 string            // Container name
	Image                string            // Image name
	State                string            // Container state (running, stopped, etc.)
	Ports                map[string]string // containerPort -> hostPort
	Labels               map[string]string // Container labels
	NetworkMode          string            // Network mode (host, bridge, etc.)
	Key                  ContainerKey      // Canonical protocol-qualified container key
	ConfigKey            ContainerKey      // Selected exact or unambiguous legacy config key
	LegacyConfigKeys     []ContainerKey    // Legacy keys uniquely owned by this container
	LegacyConfigConflict bool              // Legacy candidates exist but cannot be selected safely
	HasLabelConfig       bool              // watchcow.enable=true in labels
	HasStoredConfig      bool              // Has config in dashboard storage
	Config               *StoredConfig     // Merged config (labels take priority)
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

// HasAccessiblePorts returns true if the container has host port mappings or uses host network mode.
func (c *ContainerInfo) HasAccessiblePorts() bool {
	return len(c.Ports) > 0 || c.NetworkMode == "host"
}
