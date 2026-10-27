// Package web embeds the HTML templates and static assets into the binary.
package web

import "embed"

// FS holds templates/ and static/.
//
//go:embed templates static
var FS embed.FS
