// Package webui embeds the built dashboard so the agent ships as one binary
// and serves the UI from the same origin as the API.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built dashboard, or nil when the binary was compiled without
// one (the placeholder build). Serving from the same origin as the API is what
// keeps the browser's same-origin policy on our side.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
