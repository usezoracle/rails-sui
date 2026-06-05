// Package docs embeds the hand-written OpenAPI spec so it ships inside the
// compiled binary. The production image copies only the binary (not the repo
// tree), so reading docs/openapi.yaml from disk 404s there — the embedded copy
// is the prod fallback.
package docs

import _ "embed"

//go:embed openapi.yaml
var OpenAPISpec []byte
