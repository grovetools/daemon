// Package web embeds the static web assets for the daemon HTTP server.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:treemux
var staticFiles embed.FS

// TreemuxFileServer returns an http.Handler serving the embedded
// treemux web viewer static files.
func TreemuxFileServer() http.Handler {
	sub, err := fs.Sub(staticFiles, "treemux")
	if err != nil {
		panic("failed to create sub filesystem: " + err.Error())
	}
	return http.FileServerFS(sub)
}
