package serve

import (
	"embed"
	"io/fs"
)

// assetsFS embeds the hand-written frontend (SPEC 4.8): no bundler, no build
// step, so what's on disk here is exactly what the browser loads.
//
//go:embed assets
var assetsFS embed.FS

// staticFS is the sub-filesystem server.go mounts under /static/: everything
// under assets/, rooted so a request for "app.js" resolves to
// "assets/app.js" without the caller needing to know the embed prefix.
func staticFS() (fs.FS, error) {
	return fs.Sub(assetsFS, "assets")
}

// indexHTML is the page served at /.
func indexHTML() ([]byte, error) {
	return assetsFS.ReadFile("assets/index.html")
}
