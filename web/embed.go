package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var distFS embed.FS

// DistFS returns the embedded filesystem for the built web UI.
// The FS is sub'd to "dist" so that files are served at root.
func DistFS() http.FileSystem {
	sub, _ := fs.Sub(distFS, "dist")
	return http.FS(sub)
}

// Handler returns an http.Handler that serves the embedded web UI.
func Handler() http.Handler {
	return http.FileServer(DistFS())
}
