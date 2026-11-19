// Boots Swagger UI on the dbbat OpenAPI spec. Kept out of the HTML page so the
// docs Content-Security-Policy can forbid inline scripts.
window.onload = function () {
  window.ui = SwaggerUIBundle({
    url: "/api/openapi.yml",
    dom_id: "#swagger-ui",
    presets: [SwaggerUIBundle.presets.apis],
    layout: "BaseLayout",
    // The default validator badge loads an image from validator.swagger.io.
    validatorUrl: null,
  });
};
