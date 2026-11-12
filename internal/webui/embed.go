// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package webui embeds the compiled SPA (Vite output) into the binary so the
// admin dashboard ships as a single artifact.
//
// The embed is build-tagged. Production builds (`-tags=prod`, which is what
// `make build` uses) bake internal/webui/dist into the binary. Plain
// `go build` / `go test` runs compile without it and serve the SPA from disk
// instead — see internal/server/static.go, which falls back to web/dist.
//
// The split matters because internal/webui/dist is a gitignored build
// artifact. With an unconditional `//go:embed dist` the tree was a hard
// compile-time dependency, so on a fresh checkout every build failed with
//
//	pattern dist: no matching files found
//
// and the disk fallback in static.go was unreachable — the package could not
// compile in the first place.
package webui

import (
	"errors"
	"io/fs"
)

// ErrNoAssets is returned by Open / Index when the embedded SPA bundle is
// empty. This happens in dev/test builds where the dist tree hasn't been
// generated yet. Handlers must catch it and fall back to a stub page.
var ErrNoAssets = errors.New("webui: no embedded assets (run `make web-build`)")

// FS returns the embedded SPA filesystem rooted at `dist`. The returned
// fs.FS only contains entries when the Vite build has populated
// internal/webui/dist (production builds); otherwise Open("index.html")
// returns ErrNoAssets and the caller falls back to the on-disk bundle.
func FS() fs.FS {
	return distRoot()
}

// emptyFS is returned by FS() when the embedded directory is missing.
// Open returns ErrNoAssets so the dev path can fall back to disk.
type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, ErrNoAssets }
