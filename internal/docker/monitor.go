package docker

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"watchcow/internal/interceptor"
)

// Monitor watches Docker containers and converts them to app list
type Monitor struct {
	cli          *client.Client
	interceptor  Interceptor // Interface for sending notifications
	updateCh     chan<- []interceptor.AppInfo
	stopCh       chan struct{}
	pollInterval time.Duration

	// Track previous state to detect changes
	previousContainers map[string]string // map[containerID]containerName
}

// Interceptor interface for sending notifications
type Interceptor interface {
	SendContainerNotification(containerName string, state string) error
}

// NewMonitor creates a new Docker monitor
func NewMonitor(updateCh chan<- []interceptor.AppInfo, intcpt Interceptor) (*Monitor, error) {
	// Connect to Docker daemon
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &Monitor{
		cli:                cli,
		interceptor:        intcpt,
		updateCh:           updateCh,
		stopCh:             make(chan struct{}),
		pollInterval:       10 * time.Second, // Poll every 10 seconds
		previousContainers: make(map[string]string),
	}, nil
}

// Start starts monitoring Docker containers
func (m *Monitor) Start(ctx context.Context) {
	log.Println("🐳 Starting Docker monitor...")

	// Initial scan to get current state
	m.scanContainers(ctx)

	// Start listening to Docker events for real-time updates
	go m.listenToDockerEvents(ctx)
}

// listenToDockerEvents listens to Docker daemon events for real-time updates
func (m *Monitor) listenToDockerEvents(ctx context.Context) {
	// Set up event filters - only interested in container events
	eventFilters := filters.NewArgs()
	eventFilters.Add("type", "container")
	eventFilters.Add("event", "start")
	eventFilters.Add("event", "stop")
	eventFilters.Add("event", "die")
	eventFilters.Add("event", "destroy")

	eventChan, errChan := m.cli.Events(ctx, events.ListOptions{
		Filters: eventFilters,
	})

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case err := <-errChan:
			if err != nil {
				log.Printf("⚠️  Docker event stream error: %v, reconnecting...", err)
				time.Sleep(5 * time.Second)
				// Restart event listener
				go m.listenToDockerEvents(ctx)
				return
			}
		case event := <-eventChan:
			m.handleDockerEvent(ctx, event)
		}
	}
}

// handleDockerEvent processes a Docker event
func (m *Monitor) handleDockerEvent(ctx context.Context, event events.Message) {
	containerName := event.Actor.Attributes["name"]
	containerID := event.Actor.ID
	if len(containerID) > 12 {
		containerID = containerID[:12]
	}

	switch event.Action {
	case "start":
		// Container started - add to tracking
		m.previousContainers[containerID] = containerName
		log.Printf("▶️  Container started: %s", containerName)

		// Send notification to fnOS clients
		// Only send "running" state (frontend only responds to "running" and "stopped")
		if m.interceptor != nil {
			go func() {
				time.Sleep(2 * time.Second) // Wait for container to fully start
				if err := m.interceptor.SendContainerNotification(containerName, "running"); err != nil {
					// If trim_sac is not available during runtime, it's a critical error
					// Trigger restart to re-establish connection
					log.Printf("⚠️  Failed to send running notification for %s: %v", containerName, err)
					log.Printf("💥 Communication with trim_sac lost, triggering restart...")
					panic(fmt.Sprintf("failed to communicate with trim_sac: %v", err))
				}
			}()
		}

		// Rescan to update app list (in case it has exposed ports)
		m.scanContainers(ctx)

	case "stop", "die", "destroy":
		// Container stopped - remove from tracking
		if _, exists := m.previousContainers[containerID]; exists {
			delete(m.previousContainers, containerID)
			log.Printf("⏹️  Container stopped: %s", containerName)

			// Send notification to fnOS clients
			if m.interceptor != nil {
				if err := m.interceptor.SendContainerNotification(containerName, "stopped"); err != nil {
					log.Printf("⚠️  Failed to send stopped notification: %v", err)
				}
			}

			// Rescan to update app list
			m.scanContainers(ctx)
		}
	}
}

// scanContainers scans all running containers and sends updates
func (m *Monitor) scanContainers(ctx context.Context) {
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		log.Printf("[Docker] Error listing containers: %v", err)
		return
	}

	// Build current state and detect changes
	currentContainers := make(map[string]string) // map[containerID]containerName
	var addedContainers []string
	var removedContainers []string // Will contain container names, not IDs

	for _, ctr := range containers {
		containerID := ctr.ID[:12]
		name := strings.TrimPrefix(ctr.Names[0], "/")
		currentContainers[containerID] = name

		// Check if this is a new container
		if _, exists := m.previousContainers[containerID]; !exists {
			addedContainers = append(addedContainers, name)
		}
	}

	// Check for removed containers
	for oldID, oldName := range m.previousContainers {
		if _, exists := currentContainers[oldID]; !exists {
			removedContainers = append(removedContainers, oldName)
		}
	}

	// Update previous state
	m.previousContainers = currentContainers

	// Convert to apps
	apps := make([]interceptor.AppInfo, 0)
	skippedCount := 0
	for _, ctr := range containers {
		app := m.containerToAppInfo(&ctr)
		if app != nil {
			apps = append(apps, *app)
		} else {
			skippedCount++
		}
	}

	// Send update to app list
	select {
	case m.updateCh <- apps:
	default:
		log.Println("⚠️  Update channel full, skipping")
	}

	// Send notifications for newly discovered containers (e.g., on initial scan)
	// If trim_sac is not ready, the container will restart and retry
	for _, containerName := range addedContainers {
		if m.interceptor != nil {
			if err := m.interceptor.SendContainerNotification(containerName, "running"); err != nil {
				// trim_sac process not ready yet
				// Let Docker restart this container to retry
				log.Printf("⚠️  Failed to send notification for %s: %v", containerName, err)
				log.Printf("💥 trim_sac not ready, triggering restart to retry...")
				panic(fmt.Sprintf("trim_sac process not available: %v", err))
			}
			log.Printf("✅ Sent initial notification for container: %s", containerName)
		}
	}
}

// containerToAppInfo converts a Docker container to AppInfo
func (m *Monitor) containerToAppInfo(ctr *types.Container) *interceptor.AppInfo {
	// Check if WatchCow is enabled for this container
	if ctr.Labels["watchcow.enable"] != "true" {
		// Skip containers without watchcow.enable=true
		return nil
	}

	// Extract container name (remove leading /)
	name := strings.TrimPrefix(ctr.Names[0], "/")

	// Read all watchcow labels with fallbacks
	appName := getLabel(ctr.Labels, "watchcow.appName", fmt.Sprintf("docker-%s", name))
	appID := getLabel(ctr.Labels, "watchcow.appID", ctr.ID[:12])
	entryName := getLabel(ctr.Labels, "watchcow.entryName", appName)
	title := getLabel(ctr.Labels, "watchcow.title", prettifyName(name))
	desc := getLabel(ctr.Labels, "watchcow.desc", fmt.Sprintf("Docker: %s", ctr.Image))
	icon := getLabel(ctr.Labels, "watchcow.icon", guessIcon(ctr.Image))
	category := getLabel(ctr.Labels, "watchcow.category", "Docker")

	// Network configuration
	protocol := getLabel(ctr.Labels, "watchcow.protocol", "http")
	host := getLabel(ctr.Labels, "watchcow.host", "")
	port := getLabel(ctr.Labels, "watchcow.port", "")
	path := getLabel(ctr.Labels, "watchcow.path", "/")
	fnDomain := getLabel(ctr.Labels, "watchcow.fnDomain", fmt.Sprintf("docker-%s", name))

	// If port not specified in label, try to get from container ports
	if port == "" {
		port = getFirstPublicPort(ctr)
		if port == "" {
			// No port found, skip this container
			return nil
		}
	}

	// App type flags (parse as boolean)
	microApp := getBoolLabel(ctr.Labels, "watchcow.microApp", false)
	nativeApp := getBoolLabel(ctr.Labels, "watchcow.nativeApp", false)
	isDisplay := getBoolLabel(ctr.Labels, "watchcow.isDisplay", true)

	// Build app info
	app := &interceptor.AppInfo{
		AppName:   appName,
		AppID:     appID,
		EntryName: entryName,
		Title:     title,
		Desc:      desc,
		Icon:      icon,
		Type:      "url",
		URI: map[string]interface{}{
			"protocol": protocol,
			"host":     host,
			"port":     port,
			"path":     path,
			"fnDomain": fnDomain,
		},
		MicroApp:  microApp,
		NativeApp: nativeApp,
		FullURL:   "",
		Status:    "running",
		FileTypes: []string{},
		IsDisplay: isDisplay,
		Category:  category,
	}

	return app
}

// getLabel gets a label value with fallback
func getLabel(labels map[string]string, key, fallback string) string {
	if val, ok := labels[key]; ok && val != "" {
		return val
	}
	return fallback
}

// getBoolLabel gets a boolean label value with fallback
func getBoolLabel(labels map[string]string, key string, fallback bool) bool {
	if val, ok := labels[key]; ok {
		return val == "true" || val == "1" || val == "yes"
	}
	return fallback
}

// getFirstPublicPort gets the first public port from container
func getFirstPublicPort(ctr *types.Container) string {
	for _, port := range ctr.Ports {
		if port.PublicPort > 0 {
			return strconv.Itoa(int(port.PublicPort))
		}
	}
	return ""
}

// guessIcon tries to guess an appropriate icon URL based on image name
func guessIcon(image string) string {
	// Extract base image name
	parts := strings.Split(image, "/")
	imageName := parts[len(parts)-1]
	imageName = strings.Split(imageName, ":")[0]

	// Map common images to dashboard-icons
	iconMap := map[string]string{
		"jellyfin":   "jellyfin",
		"portainer":  "portainer",
		"nginx":      "nginx",
		"postgres":   "postgresql",
		"mysql":      "mysql",
		"redis":      "redis",
		"mongodb":    "mongodb",
		"plex":       "plex",
		"sonarr":     "sonarr",
		"radarr":     "radarr",
		"traefik":    "traefik",
		"grafana":    "grafana",
		"prometheus": "prometheus",
	}

	if iconName, ok := iconMap[imageName]; ok {
		return fmt.Sprintf("https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/%s.png", iconName)
	}

	// Default Docker icon
	return "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/docker.png"
}

// prettifyName converts container name to a nice title
func prettifyName(name string) string {
	// Remove common suffixes
	name = strings.TrimSuffix(name, "-1")
	name = strings.TrimSuffix(name, "_1")

	// Replace underscores and hyphens with spaces
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "-", " ")

	// Capitalize first letter of each word
	words := strings.Fields(name)
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}

	return strings.Join(words, " ")
}

// Stop stops the monitor
func (m *Monitor) Stop() {
	close(m.stopCh)
	if m.cli != nil {
		m.cli.Close()
	}
}
