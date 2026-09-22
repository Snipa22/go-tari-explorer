// Package docs embeds this repo's checked-in OpenAPI 3.0 spec (openapi.yaml,
// alongside this file) so internal/server can serve it live (GET /api/spec) without
// reading it off disk at runtime - same //go:embed convention
// internal/server/server.go already uses for templates/*.html.
package docs

import _ "embed"

// OpenAPISpecYAML is the raw contents of openapi.yaml, embedded at build time. Kept
// as a package-level []byte (rather than a string) since callers (internal/server's
// GET /api/spec handler) write it directly to an http.ResponseWriter.
//
//go:embed openapi.yaml
var OpenAPISpecYAML []byte
