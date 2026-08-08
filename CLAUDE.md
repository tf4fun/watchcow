# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**WatchCow** is a fnOS App Generator for Docker that monitors running Docker containers and automatically converts them into official fnOS applications using the `appcenter-cli install-local` command.

The system works by:
1. Monitoring Docker events (container start/stop/die/destroy)
2. Detecting containers with `watchcow.enable=true` label
3. Generating fnOS-compliant application package using Go templates
4. Installing/starting/stopping/uninstalling via `appcenter-cli`

## Build & Development Commands

```bash
# Build for current platform
go build -o watchcow ./cmd/watchcow

# Cross-compile for fnOS (Linux amd64)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o watchcow ./cmd/watchcow

# Run with debug logging
./watchcow --debug

# Build fpk package for fnOS distribution
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o fnos-app/app/watchcow ./cmd/watchcow
cd fnos-app && fnpack build
```

### Manual Testing

```bash
# Create a test container with watchcow labels
docker run -d \
  --name test-nginx \
  --label watchcow.enable=true \
  --label watchcow.display_name="Test Nginx" \
  --label watchcow.service_port=80 \
  -p 8080:80 \
  nginx:alpine

# Use debug-generator to test package generation without Docker events
go run ./cmd/debug-generator
```

## Architecture

### Core Components

**1. Docker Monitor (`internal/docker/monitor.go`)**
- Listens to Docker daemon events via Docker API
- Maintains operation queue for serializing appcenter-cli calls
- Tracks container states (installed/not installed)
- Event handling: start → install/start app, stop/die → stop app, destroy → uninstall app

**2. FPK Generator (`internal/fpkgen/`)**
- `generator.go` - Main generator, extracts config from container inspection, coordinates template rendering
- `template.go` - Template engine using embedded Go templates, converts AppConfig to TemplateData
- `types.go` - Core types: AppConfig, Entry, EntryControl, VolumeMapping
- `icons.go` - Downloads icons from URL or reads from `file://` local path
- `installer.go` - Wraps appcenter-cli commands (install-local, start, stop, uninstall)
- `templates/*.tmpl` - Embedded Go templates for manifest, cmd scripts, config files

**3. Templates (`internal/fpkgen/templates/`)**
- `manifest.tmpl` - fnOS app manifest
- `cmd_main.tmpl` - Main lifecycle script (start/stop/status)
- `cmd_empty.tmpl` - Empty scripts for install/uninstall callbacks
- `config_privilege.json.tmpl` - Run-as user configuration
- `config_resource.json.tmpl` - Docker project configuration

### Generated App Structure

```
<temp-dir>/
├── manifest                    # App metadata (name, version, description)
├── app/ui/
│   ├── config                 # UI entry JSON (supports multiple entries)
│   └── images/                # Entry icons
├── cmd/
│   ├── main                   # Lifecycle script (start/stop/status)
│   └── *_init, *_callback     # Empty hook scripts
├── config/
│   ├── privilege              # Run-as configuration
│   └── resource               # Docker project config
└── LICENSE
```

### Key Data Flow

```
Docker Event (container start)
        ↓
Monitor.handleDockerEvent() → shouldInstall() check
        ↓
Generator.GenerateFromContainer() → extractConfig() → parseEntries()
        ↓
NewTemplateData() → generateFromTemplates() + handleIcons()
        ↓
queueOperation("install") → Installer.InstallLocal()
        ↓
App appears in fnOS App Center
```

### Container Lifecycle Mapping

| Docker Event | fnOS Action |
|--------------|-------------|
| start (not installed) | Generate package + `appcenter-cli install-local` |
| start (already installed) | `appcenter-cli start` |
| stop/die | `appcenter-cli stop` |
| destroy | `appcenter-cli uninstall` |

## WatchCow Labels (v0.2)

### App-Level Labels
| Label | Default | Description |
|-------|---------|-------------|
| `watchcow.enable` | - | Required: set to `"true"` to enable |
| `watchcow.appname` | `watchcow.<container>` | Unique app identifier; 3-32 ASCII characters using letters, numbers, `.`, `_`, and `-` |
| `watchcow.display_name` | Container name | Human-readable name |
| `watchcow.desc` | Image name | App description |
| `watchcow.version` | `1.0.0` | App version |
| `watchcow.maintainer` | `WatchCow` | Maintainer name |
| `watchcow.install_volume` | System default volume or `1` | fnOS install volume index; overrides `appcenter-cli default-volume` |

### Entry Labels (default entry)
| Label | Default | Description |
|-------|---------|-------------|
| `watchcow.service_port` | First exposed port | Web UI port |
| `watchcow.protocol` | `http` | `http` or `https` |
| `watchcow.path` | `/` | URL path |
| `watchcow.ui_type` | `url` | `url` (new tab) or `iframe` (desktop window) |
| `watchcow.all_users` | `true` | Access permission |
| `watchcow.icon` | Auto-guessed | Icon URL or `file://` path |
| `watchcow.file_types` | - | Comma-separated file types for right-click menu |
| `watchcow.no_display` | `false` | Hide from desktop |

### Multi-Entry Support
Named entries use `watchcow.<entry>.<field>` format (e.g., `watchcow.admin.service_port`).
Entry fields: `service_port`, `protocol`, `path`, `ui_type`, `all_users`, `icon`, `title`, `file_types`, `no_display`, `control.*`

## Development Guidelines

### When Adding Features
- Container config extraction: `generator.go:extractConfig()` and `parseEntries()`
- Template data conversion: `template.go:NewTemplateData()`
- New config fields: Add to `types.go:AppConfig/Entry`, then update `extractConfig()` and `NewTemplateData()`
- New templates: Add `.tmpl` file to `templates/`, update `generateFromTemplates()` mappings

### Icon Handling
- Icons are downloaded via HTTP or read from `file://` paths
- Auto-guessing uses Dashboard Icons CDN based on image name mapping in `guessIcon()`
- Icons are resized to 256x256 PNG

### Debugging
- `--debug` flag enables slog.LevelDebug
- `cmd/debug-generator` - Test package generation with mock AppConfig
- Generated packages are in temp directories (cleaned up after install)
