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

// ServeOpenAPISpec handles GET /openapi.yaml. In dev it reads docs/openapi.yaml
// from disk so spec edits hot-reload without a rebuild. If the file is missing
// from disk, it returns 404.
func (ctrl *Controller) ServeOpenAPISpec(c *gin.Context) {
	if b, err := os.ReadFile(filepath.Join("docs", "openapi.yaml")); err == nil {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", b)
		return
	}
	c.Status(http.StatusNotFound)
}
