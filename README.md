# WatchCow

**飞牛OS (fnOS) Docker 容器自动应用化工具**

WatchCow 监控 Docker 容器事件，自动将带有 `watchcow.enable=true` 标签的容器转换为 fnOS 原生应用，通过 `appcenter-cli install-local` 安装到应用中心。

## 功能特性

- **自动发现** - 监听 Docker 事件，自动检测启用的容器
- **自动安装** - 生成 fnOS 应用包并自动安装
- **生命周期同步** - 容器启动/停止/销毁与 fnOS 应用状态同步
- **灵活配置** - 通过 Docker labels 自定义应用信息
- **图标支持** - 支持 HTTP URL 或本地文件 (`file://...`) 作为图标，自动转换多种格式

## 工作原理

```
Docker 容器启动 (watchcow.enable=true)
        ↓
WatchCow 检测到容器事件
        ↓
提取容器配置，生成 fnOS 应用包
        ↓
appcenter-cli install-local
        ↓
应用出现在 fnOS 应用中心
```

### 容器生命周期

| Docker 事件 | fnOS 操作 |
|-------------|-----------|
| 容器启动 (已安装) | `appcenter-cli start` |
| 容器启动 (未安装) | 生成应用包 + `appcenter-cli install-local` |
| 容器停止 | `appcenter-cli stop` |
| 容器销毁 | `appcenter-cli uninstall` |

## 安装

从 [Releases](https://github.com/tf4fun/watchcow/releases) 下载 `watchcow.fpk`，在 fnOS 应用中心使用"本地安装"功能安装。

## 使用方法

### 基本用法

在容器上添加 `watchcow.enable=true` 标签：

```yaml
services:
  nginx:
    image: nginx:alpine
    ports:
      - "8080:80"
    labels:
      watchcow.enable: "true"
```

启动容器后，WatchCow 会自动将其安装为 fnOS 应用。

### 完整配置示例

```yaml
services:
  memos:
    image: neosmemo/memos:stable
    ports:
      - "5230:5230"
    volumes:
      - /vol1/1000/docker/memos:/var/opt/memos
    labels:
      watchcow.enable: "true"
      watchcow.display_name: "Memos"
      watchcow.desc: "轻量级笔记应用"
      watchcow.service_port: "5230"
      watchcow.protocol: "http"
      watchcow.path: "/"
      watchcow.icon: "https://example.com/memos-icon.png"
```

## 配置标签

### 应用级配置

| 标签 | 必需 | 默认值 | 说明 |
|------|------|--------|------|
| `watchcow.enable` | 是 | - | 设为 `"true"` 启用 |
| `watchcow.appname` | 否 | `watchcow.<容器名>` | 应用唯一标识 |
| `watchcow.display_name` | 否 | 容器名 | 应用显示名称（manifest） |
| `watchcow.desc` | 否 | 镜像名 | 应用描述 |
| `watchcow.version` | 否 | `1.0.0` | 应用版本 |
| `watchcow.maintainer` | 否 | `WatchCow` | 维护者 |

### 入口配置（默认入口）

| 标签 | 必需 | 默认值 | 说明 |
|------|------|--------|------|
| `watchcow.service_port` | 否 | 首个暴露端口 | Web UI 端口 |
| `watchcow.protocol` | 否 | `http` | 协议 (`http`/`https`) |
| `watchcow.path` | 否 | `/` | URL 路径 |
| `watchcow.ui_type` | 否 | `url` | UI 类型 (`url` 新标签页 / `iframe` 桌面窗口) |
| `watchcow.all_users` | 否 | `true` | 访问权限 (`true` 所有用户 / `false` 仅管理员) |
| `watchcow.title` | 否 | `display_name` | 入口标题 |
| `watchcow.icon` | 否 | 自动猜测 | 图标 URL 或 `file://` 本地路径 |
| `watchcow.file_types` | 否 | - | 支持的文件类型（逗号分隔），用于文件右键菜单 |
| `watchcow.no_display` | 否 | `false` | 设为 `true` 则不在桌面显示 |
| `watchcow.control.access_perm` | 否 | `readonly` | 访问权限设置权限 |
| `watchcow.control.port_perm` | 否 | `readonly` | 端口设置权限 |
| `watchcow.control.path_perm` | 否 | `readonly` | 路径设置权限 |

### 多入口配置

WatchCow 支持为单个应用配置多个入口。使用 `watchcow.<entry>.<field>` 格式定义命名入口：

| 标签 | 说明 |
|------|------|
| `watchcow.<entry>.service_port` | 入口端口 |
| `watchcow.<entry>.protocol` | 入口协议 |
| `watchcow.<entry>.path` | 入口路径 |
| `watchcow.<entry>.ui_type` | 入口 UI 类型 |
| `watchcow.<entry>.all_users` | 入口访问权限 |
| `watchcow.<entry>.title` | 入口标题（默认：`display_name - entry`） |
| `watchcow.<entry>.icon` | 入口图标 |
| `watchcow.<entry>.file_types` | 支持的文件类型（逗号分隔），用于文件右键菜单 |
| `watchcow.<entry>.no_display` | 设为 `true` 则不在桌面显示，仅在右键菜单显示 |
| `watchcow.<entry>.control.access_perm` | 访问权限设置权限：`editable`/`readonly`/`hidden` |
| `watchcow.<entry>.control.port_perm` | 端口设置权限：`editable`/`readonly`/`hidden` |
| `watchcow.<entry>.control.path_perm` | 路径设置权限：`editable`/`readonly`/`hidden` |

**多入口示例：**

```yaml
services:
  myapp:
    image: myapp:latest
    ports:
      - "8080:8080"
      - "8081:8081"
    labels:
      watchcow.enable: "true"
      watchcow.display_name: "我的应用"

      # 主入口
      watchcow.main.service_port: "8080"
      watchcow.main.path: "/"
      watchcow.main.title: "我的应用"

      # 管理后台入口
      watchcow.admin.service_port: "8081"
      watchcow.admin.path: "/admin"
      watchcow.admin.title: "管理后台"
      watchcow.admin.all_users: "false"
      watchcow.admin.ui_type: "iframe"
```

**文件右键菜单示例：**

```yaml
labels:
  watchcow.enable: "true"
  watchcow.display_name: "文本编辑器"

  # 编辑器入口（文件右键菜单）
  watchcow.editor.service_port: "8080"
  watchcow.editor.path: "/edit"
  watchcow.editor.title: "用文本编辑器打开"
  watchcow.editor.file_types: "txt,md,json,xml"
  watchcow.editor.no_display: "true"
```

### 图标配置

支持两种图标来源：

```yaml
# HTTP/HTTPS URL
watchcow.icon: "https://example.com/icon.png"

# 本地文件（绝对路径）
watchcow.icon: "file:///path/to/icon.png"

# 本地文件（相对路径，相对于 compose 文件所在目录）
watchcow.icon: "file://./icons/icon.png"
watchcow.icon: "file://icons/icon.png"
```

**支持的图标格式：**

| 格式 | 说明 |
|------|------|
| PNG | 直接使用 |
| JPEG/JPG | 自动转换为 PNG |
| WebP | 自动转换为 PNG |
| BMP | 自动转换为 PNG |
| ICO | 自动选择最高分辨率图像并转换为 PNG |

图标格式通过文件内容（magic bytes）自动检测，不依赖文件扩展名。

**相对路径说明：**

使用 Docker Compose 部署时，`file://` 相对路径会相对于 compose 文件所在目录解析。这是通过读取容器的 `com.docker.compose.project.working_dir` 标签实现的。

```
project/
├── compose.yaml
└── icons/
    └── myapp.png    # file://icons/myapp.png 或 file://./icons/myapp.png
```

## 开发

### 编译

```bash
# 编译当前平台
go build -o watchcow ./cmd/watchcow

# 交叉编译 fnOS (Linux amd64)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o watchcow ./cmd/watchcow
```

### 构建 fpk 包

```bash
# 编译并放入 fnos-app
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o fnos-app/app/watchcow ./cmd/watchcow

# 使用 fnpack 打包
cd fnos-app
fnpack build
```

### 调试

```bash
./watchcow --debug
```

## 项目结构

```
watchcow/
├── cmd/watchcow/           # 程序入口
├── internal/
│   ├── docker/             # Docker 事件监控
│   └── fpkgen/             # fnOS 应用包生成
├── fnos-app/               # WatchCow 的 fnOS 应用包模板
└── examples/               # 示例配置
```

## 从 0.1 升级到 0.2

v0.2 版本进行了重构，标签名称有以下变更：

| 旧标签 (v0.1) | 新标签 (v0.2) |
|---------------|---------------|
| `watchcow.title` | `watchcow.display_name` |
| `watchcow.description` | `watchcow.desc` |
| `watchcow.port` | `watchcow.service_port` |
| `watchcow.appName` | `watchcow.appname` |

升级后需要更新容器的 labels 配置。

## 许可证

MIT License

## 致谢

- [Docker](https://www.docker.com/)
- [飞牛OS](https://www.fnnas.com/)
- [Dashboard Icons](https://github.com/homarr-labs/dashboard-icons)
