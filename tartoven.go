// Package tartoven provides version metadata and embedded documentation for
// tart-oven, a fleet manager for macOS VMs running under Tart.
package tartoven

import "embed"

// Version is the current release version of tart-oven.
const Version = "2.1.0"

// Docs embeds README.md and CHANGELOG.md at build time.
//
//go:embed README.md CHANGELOG.md
var Docs embed.FS

// Icon is the app icon shown in the dashboard header and as the browser tab
// icon: assets/icons/tart-oven-icon-512px.png, scaled down by the browser.
//
//go:embed assets/icons/tart-oven-icon-512px.png
var Icon []byte
