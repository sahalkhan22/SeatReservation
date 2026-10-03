package api

import (
	_ "embed"
	"net/http"
)

// The demo console is embedded in the binary, so the deployed image is still a
// single static file with no assets to serve from disk and no build step.
//
//go:embed ui/index.html
var consoleHTML []byte

// serveConsole answers the root path only. The pattern is "GET /{$}" at the
// registration site - a bare "GET /" in Go's mux is a catch-all and would
// swallow every unmatched path instead of returning 404.
func (s *Server) serveConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(consoleHTML)
}
