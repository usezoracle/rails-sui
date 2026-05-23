// Swagger UI + raw OpenAPI spec endpoints.
//
//   GET /docs           — Swagger UI HTML (CDN-hosted assets, our spec)
//   GET /openapi.yaml   — the raw spec file
//
// Single-source-of-truth: `docs/openapi.yaml`. Hand-written rather
// than annotation-generated — gives full OpenAPI 3.1 fidelity and
// keeps controllers free of swagger comments.

package controllers

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
)

// swaggerUIHTML is a tiny Swagger UI shell that loads the official
// assets from a CDN and points at our local /openapi.yaml. No npm
// deps, no embedded assets — works in any browser online.
const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Rails API — Swagger</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css" />
  <style>
    body { margin: 0; padding: 0; background: #fafafa; }
    .topbar { display: none; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-standalone-preset.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: '/openapi.yaml',
      dom_id: '#swagger-ui',
      deepLinking: true,
      presets: [
        SwaggerUIBundle.presets.apis,
        SwaggerUIStandalonePreset,
      ],
      plugins: [SwaggerUIBundle.plugins.DownloadUrl],
      layout: 'BaseLayout',
      docExpansion: 'list',
      defaultModelsExpandDepth: 1,
      persistAuthorization: true,
      tryItOutEnabled: true,
    });
  </script>
</body>
</html>`

// ServeSwaggerUI handles GET /docs.
func (ctrl *Controller) ServeSwaggerUI(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(swaggerUIHTML))
}

// ServeOpenAPISpec handles GET /openapi.yaml. We read the file at
// request time (not via go:embed) so spec edits hot-reload without a
// rebuild during dev — production payload is small (~30 KB) so the
// read cost is negligible.
func (ctrl *Controller) ServeOpenAPISpec(c *gin.Context) {
	// CWD-relative path. The server is launched from the repo root in
	// every supported config (Makefile `make run` + production
	// Dockerfile both `WORKDIR /app` at the repo root).
	path := filepath.Join("docs", "openapi.yaml")
	bytes, err := os.ReadFile(path)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"status":  "error",
			"message": "openapi.yaml not found at " + path,
		})
		return
	}
	// application/yaml is the IANA-registered media type; some tools
	// prefer text/yaml. Either works for Swagger UI's url loader.
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", bytes)
}
