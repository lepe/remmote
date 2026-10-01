package hub

import "embed"

// Assets is the connection manager's interface: HTML, CSS and JS, served
// from inside the binary by the webview window. Kept here rather than in
// the window's package so the embed is checked by every build, not only
// by the one that has a webview.
//
//go:embed all:frontend
var Assets embed.FS
