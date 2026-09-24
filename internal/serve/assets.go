package serve

import (
	"embed"
	"io/fs"
)

// assetsFS embeds the frontend (SPEC 4.8). Most of it is hand-written, no
// bundler, no build step — what's on disk here is exactly what the browser
// loads. The flow view under assets/flow/ is the exception: a committed
// esbuild bundle built from source in web/; nix/checks/flow-bundle.nix keeps
// the two in sync.
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
