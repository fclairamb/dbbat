# Vendored Swagger UI

Served by `GET /api/docs` (`internal/api/swagger_ui.go`) from the binary itself,
so the docs page loads no script from a third-party origin.

- Package: `swagger-ui-dist@5.34.1` (npm), files copied verbatim from the tarball:
  `swagger-ui-bundle.js`, `swagger-ui.css`, `LICENSE`, `NOTICE`,
  `swagger-ui-bundle.js.LICENSE.txt`.
- Tarball integrity: `sha512-VSr8fXfI93obYaQicLJbLUKGRxDojj/SfRJDDLCioGTUBj4BK/Nj4eE9jCeCAnJxVLi7iS+08PbiZLyrQKBkJQ==`
- `swagger-initializer.js` is ours.

To upgrade: `npm pack swagger-ui-dist@<version>` in an empty directory, check its
`sha512` against `npm view swagger-ui-dist@<version> dist.integrity`, copy the
files above, and update the version here and in `swaggerUIVersion`.
