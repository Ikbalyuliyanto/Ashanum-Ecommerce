// Package web embeds templates and static assets into the binary so
// production runs from a single file.
package web

import "embed"

//go:embed templates static
var FS embed.FS
