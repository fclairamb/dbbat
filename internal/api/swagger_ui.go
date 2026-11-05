package api

import (
	"embed"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// swaggerUIVersion is the vendored swagger-ui-dist release (see swaggerui/README.md).
const swaggerUIVersion = "5.34.1"

// swaggerUIFS holds a pinned swagger-ui-dist build. It is served from dbbat's
// own origin rather than a CDN: the page runs where the web UI keeps its session
// token, so a floating third-party script could read it.
//
//go:embed swaggerui/swagger-ui-bundle.js swaggerui/swagger-ui.css swaggerui/swagger-initializer.js
var swaggerUIFS embed.FS

// swaggerUIAssets maps the asset names served under /api/docs/ to their content type.
var swaggerUIAssets = map[string]string{
	"swagger-ui-bundle.js":   "application/javascript; charset=utf-8",
	"swagger-initializer.js": "application/javascript; charset=utf-8",
	"swagger-ui.css":         "text/css; charset=utf-8",
}

// swaggerUICSP confines the docs page to its own origin. Inline styles stay
// allowed because Swagger UI's React components set style attributes; inline
// scripts do not, which is why the initializer is a separate file.
const swaggerUICSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>DBBat API Documentation</title>
    <link rel="stylesheet" type="text/css" href="/api/docs/swagger-ui.css?v=` + swaggerUIVersion + `">
</head>
<body>
    <div id="swagger-ui"></div>
    <script src="/api/docs/swagger-ui-bundle.js?v=` + swaggerUIVersion + `"></script>
    <script src="/api/docs/swagger-initializer.js?v=` + swaggerUIVersion + `"></script>
</body>
</html>`

// handleSwaggerUI serves the Swagger UI page and its embedded assets. Any
// other path under /api/docs renders the page, as it always has.
func (s *Server) handleSwaggerUI(c *gin.Context) {
	name := strings.TrimPrefix(c.Param("any"), "/")
	if contentType, ok := swaggerUIAssets[name]; ok {
		data, err := swaggerUIFS.ReadFile("swaggerui/" + name)
		if err != nil {
			c.Status(http.StatusInternalServerError)

			return
		}

		c.Header("Cache-Control", "public, max-age=86400")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Data(http.StatusOK, contentType, data)

		return
	}

	c.Header("Content-Security-Policy", swaggerUICSP)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(swaggerUIHTML))
}
