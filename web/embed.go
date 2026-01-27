package web

import "embed"

//go:embed css/*.css templates/*.tmpl
var Assets embed.FS
