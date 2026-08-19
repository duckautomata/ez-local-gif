// Package web embeds the built Svelte SPA (web/dist). The Docker build
// produces dist in the node stage; only dist/.gitkeep is committed so the
// module always compiles. When index.html is absent the server answers with
// a plain "frontend not built" notice.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built SPA as a filesystem rooted at dist/.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
