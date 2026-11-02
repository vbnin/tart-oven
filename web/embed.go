// Package web provides the embedded web UI for tart-oven.
package web

import "embed"

// Content embeds index.html at build time.
//
//go:embed index.html
var Content embed.FS
