# Bookmark 服务

单文件、单容器、多入口的网站书签。

## 快速开始

```bash
cd examples/bookmark
docker compose up -d
```

## 添加新书签

1. 在 `case` 语句中添加映射:

```sh
case "$PATH_INFO" in
  /bilibili) URL="https://www.bilibili.com/" ;;
  /github)   URL="https://github.com/" ;;
  /newsite)  URL="https://newsite.com/" ;;  # 新增
  *)         URL="" ;;
esac
```

2. 添加入口标签:

```yaml
watchcow.newsite.service_port: "3000"
watchcow.newsite.path: "/cgi-bin/index.cgi?newsite"
watchcow.newsite.title: "新网站"
watchcow.newsite.icon: "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/newsite.png"
```

## 图标配置

支持多种图标来源和格式：

```yaml
# HTTP URL
watchcow.bilibili.icon: "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/bilibili.png"

# 本地文件（相对路径，相对于 compose.yaml 所在目录）
watchcow.github.icon: "file://icons/github.webp"

# 本地文件（ICO 格式，自动选择最高分辨率）
watchcow.youtube.icon: "file://./icons/youtube.ico"
```

**支持的格式：** PNG、JPEG、WebP、BMP、ICO（自动转换为 PNG）

## 图标查找

[Dashboard Icons](https://github.com/homarr-labs/dashboard-icons) 提供常用服务图标。
