package web

import "embed"

//go:embed css/*.css templates/*.tmpl js/*.js
var Assets embed.FS
