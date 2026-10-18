// Package webui embeds the compiled SPA (Vite output) into the binary so the
// admin dashboard ships as a single artifact. During development the embed
// returns nothing — internal/server/static.go swaps to disk via the `dev` build
// tag — so iteration doesn't require a full rebuild.
package webui

import (
	"embed"
	"errors"
	"io/fs"
)

//go:embed dist
var distFS embed.FS

// ErrNoAssets is returned by Open / Index when the embedded SPA bundle is
// empty. This happens in test/dev builds where the dist tree hasn't been
// generated yet. Handlers must catch it and fall back to a stub page.
var ErrNoAssets = errors.New("webui: no embedded assets (run `make web-build`)")

// FS returns the embedded SPA filesystem rooted at `dist`. The returned
// fs.FS only contains entries when the Vite build has populated
// internal/webui/dist; otherwise Open("index.html") returns ErrNoAssets.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return emptyFS{}
	}
	return sub
}

// emptyFS is returned by FS() when the embedded directory is missing.
// Open returns ErrNoAssets so the dev path can fall back to disk.
type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, ErrNoAssets }