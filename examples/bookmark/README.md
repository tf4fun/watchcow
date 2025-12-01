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

## 图标查找

[Dashboard Icons](https://github.com/homarr-labs/dashboard-icons) 提供常用服务图标。
