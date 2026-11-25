// Package web holds the interface: plain HTML, CSS and ES modules, embedded
// into the binary and served at / by the Z-Dash server, which also serves
// Z-Dash itself (/z-dash/) and its shell (/z-dash/server/). The logo and
// icons (img) are the ones of Socrates (SocratesAgent), and so is the code
// font.
package web

import "embed"

// FS is the interface as served at /.
//
//go:embed index.html manifest.json theme.css app.css app img fonts
var FS embed.FS
