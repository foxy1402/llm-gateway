package dashboard

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var staticFS embed.FS

// staticFiles returns the static subtree rooted at static/.
func staticFiles() (fs.FS, error) {
	return fs.Sub(staticFS, "static")
}
