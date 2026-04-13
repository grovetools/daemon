// Package web embeds the static web assets for the daemon HTTP server.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:terminal
var staticFiles embed.FS

// TerminalFileServer returns an http.Handler serving the embedded
// terminal web viewer static files.
func TerminalFileServer() http.Handler {
	sub, err := fs.Sub(staticFiles, "terminal")
	if err != nil {
		panic("failed to create sub filesystem: " + err.Error())
	}
	return http.FileServerFS(sub)
}
