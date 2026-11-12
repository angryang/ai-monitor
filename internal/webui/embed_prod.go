// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

//go:build prod

package webui

import (
	"embed"
	"io/fs"
)

// distFS holds the Vite bundle produced by `make web-build`. It is only
// compiled into production builds; the tree must exist at build time or the
// compiler fails with "pattern dist: no matching files found".
//
//go:embed dist
var distFS embed.FS

// distRoot returns the SPA bundle rooted at the embedded dist directory.
func distRoot() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return emptyFS{}
	}
	return sub
}
