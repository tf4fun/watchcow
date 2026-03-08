---
name: watchcow-labels-skill
description: >
  Convert Docker Compose files to WatchCow format for fnOS by adding the correct
  `watchcow.*` labels. Use this skill whenever a user wants to deploy a Docker container
  on fnOS (飞牛OS), convert a compose.yaml to WatchCow format, add WatchCow labels,
  create an fnOS native app from Docker, or asks about watchcow label configuration.
  Also trigger when the user mentions "飞牛", "fnOS app", or wants a Docker container
  to show up in the fnOS App Center.
---

# WatchCow Labels — Docker to fnOS App Conversion

WatchCow monitors Docker container events and automatically converts containers with
`watchcow.enable=true` into native fnOS applications. Your job is to add the right
labels to a Docker Compose service so WatchCow can pick it up.

## The Golden Rule

All label values MUST be quoted strings in YAML — `"true"`, `"8080"`, not bare `true` or `8080`.

## Quick Start — Minimal Config

The only required label is `watchcow.enable: "true"`. Everything else has sensible defaults:

```yaml
labels:
  watchcow.enable: "true"
```

WatchCow will auto-detect the port (first exposed port), use the container name as display name, and try to find an icon from CDN based on the image name.

## Label Reference

### App-Level Labels

| Label | Required | Default | Purpose |
|-------|----------|---------|---------|
| `watchcow.enable` | YES | — | Must be `"true"` to activate |
| `watchcow.appname` | no | `watchcow.<container>` | Unique app ID (no spaces allowed) |
| `watchcow.display_name` | no | container name | Name shown on desktop & app store |
| `watchcow.desc` | no | image name | Short description |
| `watchcow.version` | no | `1.0.0` | App version string |
| `watchcow.maintainer` | no | `WatchCow` | Maintainer name |

### Entry Labels (Default Entry)

These control how the app appears and opens in fnOS:

| Label | Default | Purpose |
|-------|---------|---------|
| `watchcow.service_port` | first exposed port | **Host-side** port (left side of `ports` mapping) |
| `watchcow.protocol` | `http` | `http` or `https` |
| `watchcow.path` | `/` | URL path |
| `watchcow.redirect` | — | External URL; when set, port/protocol/path are ignored and the app opens this URL directly |
| `watchcow.ui_type` | `url` | `url` (new browser tab) or `iframe` (desktop window) |
| `watchcow.all_users` | `true` | `"true"` = all users, `"false"` = admin only |
| `watchcow.title` | same as `display_name` | Entry title |
| `watchcow.icon` | auto-guessed from image name via CDN | Icon URL (`https://...`) or local file (`file://...`) |
| `watchcow.file_types` | — | Comma-separated file extensions for right-click menu (e.g. `"txt,md,json"`) |
| `watchcow.no_display` | `false` | `"true"` hides from desktop (useful for right-click-only entries) |

### Entry Control Permissions

| Label | Default | Values |
|-------|---------|--------|
| `watchcow.control.access_perm` | `readonly` | `editable` / `readonly` / `hidden` |
| `watchcow.control.port_perm` | `readonly` | `editable` / `readonly` / `hidden` |
| `watchcow.control.path_perm` | `readonly` | `editable` / `readonly` / `hidden` |

### Multi-Entry Labels

For apps with multiple web UIs (e.g. user-facing + admin panel), use named entries
with `watchcow.<entry>.<field>` format. All entry-level fields are supported:

```
watchcow.<entry>.service_port
watchcow.<entry>.protocol
watchcow.<entry>.path
watchcow.<entry>.redirect
watchcow.<entry>.ui_type
watchcow.<entry>.all_users
watchcow.<entry>.title          # defaults to "<display_name> - <entry>"
watchcow.<entry>.icon
watchcow.<entry>.file_types
watchcow.<entry>.no_display
watchcow.<entry>.control.*
```

Entry names must NOT conflict with reserved top-level fields (enable, appname, display_name, desc, version, maintainer).

## Common Patterns

### Pattern 1: Simple Web App

A single-port web service — the most common case.

```yaml
services:
  memos:
    image: neosmemo/memos:stable
    container_name: memos
    ports:
      - "5230:5230"
    volumes:
      - ./data:/var/opt/memos
    restart: unless-stopped
    network_mode: bridge

    labels:
      watchcow.enable: "true"
      watchcow.display_name: "Memos"
      watchcow.desc: "轻量级笔记与知识管理工具"
      watchcow.service_port: "5230"
      watchcow.icon: "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/memos.png"
```

### Pattern 2: Different Host and Container Ports

When the host port differs from container port, `service_port` must be the **host** port.

```yaml
    ports:
      - "8080:80"        # host 8080 → container 80
    labels:
      watchcow.service_port: "8080"   # use 8080, NOT 80
```

### Pattern 3: External Redirect (Bookmark/Shortcut)

Use `watchcow.redirect` to create an app that simply opens an external URL. No real web server needed in the container — a lightweight `busybox` HTTP redirect works:

```yaml
configs:
  redirect:
    content: |
      #!/bin/sh
      echo "Status: 302 Found"
      echo "Location: https://www.bilibili.com/"
      echo ""

services:
  bilibili:
    image: busybox:uclibc
    container_name: bilibili-redirect
    ports:
      - "3000:3000"
    configs:
      - source: redirect
        target: /www/cgi-bin/index.cgi
        mode: 0755
    command: httpd -f -p 3000 -h /www
    restart: unless-stopped
    stop_signal: SIGKILL
    network_mode: bridge

    labels:
      watchcow.enable: "true"
      watchcow.display_name: "哔哩哔哩"
      watchcow.desc: "一键跳转到 B 站"
      watchcow.service_port: "3000"
      watchcow.path: "/cgi-bin/index.cgi"
      watchcow.icon: "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/bilibili.png"
```

### Pattern 4: Multi-Entry App

An app with a user portal and an admin panel on different ports:

```yaml
    ports:
      - "8080:8080"
      - "8081:8081"
    labels:
      watchcow.enable: "true"
      watchcow.display_name: "我的应用"

      # User-facing entry
      watchcow.main.service_port: "8080"
      watchcow.main.path: "/"
      watchcow.main.title: "我的应用"

      # Admin entry (admin-only, in desktop window)
      watchcow.admin.service_port: "8081"
      watchcow.admin.path: "/admin"
      watchcow.admin.title: "管理后台"
      watchcow.admin.all_users: "false"
      watchcow.admin.ui_type: "iframe"
```

### Pattern 5: File Handler (Right-Click Menu)

Register an app as a file handler so users can right-click files in fnOS file manager:

```yaml
    labels:
      watchcow.enable: "true"
      watchcow.display_name: "文本编辑器"

      watchcow.editor.service_port: "8080"
      watchcow.editor.path: "/edit"
      watchcow.editor.title: "用文本编辑器打开"
      watchcow.editor.file_types: "txt,md,json,xml"
      watchcow.editor.no_display: "true"
```

### Pattern 6: Multi-Bookmark with Local Icons

Multiple entries sharing one container, each entry with its own local icon:

```yaml
    labels:
      watchcow.enable: "true"
      watchcow.display_name: "网站书签"
      watchcow.desc: "常用网站快捷跳转"

      watchcow.bilibili.service_port: "3001"
      watchcow.bilibili.path: "/cgi-bin/index.cgi?bilibili"
      watchcow.bilibili.title: "哔哩哔哩"
      watchcow.bilibili.icon: "file://./icons/bilibili.png"

      watchcow.github.service_port: "3001"
      watchcow.github.path: "/cgi-bin/index.cgi?github"
      watchcow.github.title: "GitHub"
      watchcow.github.icon: "file://icons/github.webp"
```

`file://` relative paths resolve relative to the compose file's directory.

## Icon Lookup Priority

1. User-specified `watchcow.icon` or `watchcow.<entry>.icon`
2. Local icon library: fnOS File Manager → App Files → `icons/` folder
3. CDN: `https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/<name>.png`

Icon naming: default entry uses image name (e.g. `nginx:alpine` → `nginx`), named entries use the entry name.

Supported formats: PNG, JPEG, WebP, BMP, ICO — all auto-converted to 256x256 PNG.

## Conversion Checklist

When converting a plain Docker Compose file to WatchCow format:

1. Add `watchcow.enable: "true"` to the main service (the one with a web UI)
2. Set `watchcow.display_name` and `watchcow.desc` for a good user experience
3. Set `watchcow.service_port` to the **host** port (left side of `ports` mapping) — only needed if the container exposes multiple ports or you want to be explicit
4. Find an icon: check [Dashboard Icons](https://github.com/homarr-labs/dashboard-icons) for the app name, or let WatchCow auto-guess
5. Add `restart: unless-stopped` and `network_mode: bridge` if not already present
6. For multi-service stacks (e.g. app + database), only label the service with a web UI
7. Quote all label values as strings

## Gotchas

- **Labels are immutable after container creation.** Changing labels requires `docker compose down && docker compose up -d` (destroy and recreate), not just restart.
- **`service_port` is the host port**, not the container port. If `ports: "8080:80"`, use `"8080"`.
- **`appname` must not contain spaces.** Use the format `watchcow.<name>`.
- **When using `watchcow.redirect`**, the `service_port`, `protocol`, and `path` labels are ignored — the app opens the redirect URL directly.
- **Multi-entry names must avoid reserved words**: `enable`, `appname`, `display_name`, `desc`, `version`, `maintainer` cannot be used as entry names.
