// Package docs embeds the documentation that the web UI shows.
package docs

import _ "embed"

// Integrations is integrations.md, rendered on the UI's Setup page.
//
//go:embed integrations.md
var Integrations []byte
