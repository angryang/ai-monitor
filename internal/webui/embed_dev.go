// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

//go:build !prod

package webui

import "io/fs"

// distRoot returns an empty filesystem in non-production builds.
//
// The SPA is served from disk (web/dist) by internal/server/static.go, so the
// backend can be built and tested without a Vite toolchain or the embedded
// dist tree.
func distRoot() fs.FS {
	return emptyFS{}
}
