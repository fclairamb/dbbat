package api

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/fclairamb/dbbat/internal/auth/oidc"
	"github.com/fclairamb/dbbat/internal/auth/slack"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/notify"
)

// apiV1Prefix is the gin group every documented endpoint lives under. The
// OpenAPI spec declares it once as the server base path, so its `paths:` keys
// are relative to it.
const apiV1Prefix = "/api/v1"

// openapiMethods are the path-item keys that describe an operation. Anything
// else under a path item (`parameters`, `summary`, `description`, ...) is
// metadata, not a route.
var openapiMethods = map[string]bool{
	"get":     true,
	"put":     true,
	"post":    true,
	"delete":  true,
	"patch":   true,
	"head":    true,
	"options": true,
	"trace":   true,
}

// exemptRoutes are the routes deliberately outside the REST API surface, and
// therefore outside the spec. Keep this list short: an entry is a promise that
// the route is not an API endpoint, not a license to ship an undocumented one.
// The test fails if a route outside /api/v1 is not listed here, so adding an
// endpoint under a new prefix forces an explicit decision.
var exemptRoutes = map[string]string{
	"GET /":                "redirect to the SPA base URL",
	"GET /llms.txt":        "llms.txt convention (plain text doc map), not a REST endpoint",
	"GET /app/*filepath":   "SPA shell + static assets served from the embedded frontend",
	"GET /api/openapi.yml": "serves the spec itself; describing it in the spec is circular",
	"GET /api/docs":        "Swagger UI HTML page",
	"GET /api/docs/*any":   "Swagger UI HTML page (sub-paths)",
}

// openapiDoc is the minimal slice of OpenAPI we need: path -> path item. The
// path item stays untyped because it mixes operations with metadata keys.
type openapiDoc struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

// parseOpenAPISpec unmarshals the embedded spec. Doing so is itself an
// assertion: nothing else in the test suite checks that openapi.yml is valid
// YAML, and a typo would only surface as a broken Swagger UI in production.
func parseOpenAPISpec(t *testing.T) openapiDoc {
	t.Helper()

	var doc openapiDoc
	if err := yaml.Unmarshal(openapiSpec, &doc); err != nil {
		t.Fatalf("embedded openapi.yml is not valid YAML: %v", err)
	}

	if len(doc.Paths) == 0 {
		t.Fatal("embedded openapi.yml declares no paths")
	}

	return doc
}

// specOperations returns the "METHOD /path" keys declared by the spec.
func specOperations(t *testing.T, doc openapiDoc) map[string]bool {
	t.Helper()

	ops := make(map[string]bool)

	for path, item := range doc.Paths {
		for key := range item {
			if !openapiMethods[strings.ToLower(key)] {
				continue
			}

			ops[operationKey(strings.ToUpper(key), path)] = true
		}
	}

	return ops
}

// operationKey builds the comparable "METHOD /path" form used on both sides.
func operationKey(method, path string) string {
	return method + " " + path
}

// ginPathToSpecPath rewrites a gin route into the spec's vocabulary:
// "/api/v1/connections/:uid" -> "/connections/{uid}".
func ginPathToSpecPath(path string) string {
	path = strings.TrimPrefix(path, apiV1Prefix)

	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, ":") {
			segments[i] = "{" + strings.TrimPrefix(seg, ":") + "}"
		}
	}

	return strings.Join(segments, "/")
}

// newParityTestServer builds the fully-featured router. Four route families
// are registered conditionally on configuration — the Slack OAuth login
// routes, the generic OIDC login routes, the inbound Slack interactions
// webhook and the MCP endpoint — and all are documented, so they must be
// enabled here or they would look like spec-only paths.
func newParityTestServer(t *testing.T) *Server {
	t.Helper()

	server, dataStore := setupTestServer(t)

	// setupTestServer builds a bare config, so the MCP endpoint's shipped
	// default (on) has to be restated. TestDefaultConfigEnablesMCP in
	// internal/config is what pins that default itself.
	server.config.MCP.Enabled = true

	server.oauthProviders["slack"] = slack.NewProvider("test-client-id", "test-client-secret", "T0TEST")

	// Constructing the OIDC provider does no network I/O (discovery is lazy),
	// so an offline route-parity run is fine.
	oidcProvider, err := oidc.NewProvider(oidc.Config{
		Issuer:       "https://issuer.example.test",
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
	})
	if err != nil {
		t.Fatalf("failed to build oidc provider: %v", err)
	}

	server.oauthProviders[oidc.ProviderName] = oidcProvider

	notifier, err := notify.NewSlackNotifier(
		config.SlackNotifyConfig{
			BotToken:      "xoxb-test",
			Channel:       "#dbbat",
			SigningSecret: "test-signing-secret",
		},
		"https://dbbat.example.test",
		dataStore,
		server.logger,
	)
	if err != nil {
		t.Fatalf("failed to build interactive slack notifier: %v", err)
	}

	server.notifier = notifier

	return server
}

// TestOpenAPIRouteParity fails when a registered route is missing from
// openapi.yml, or when the spec documents a path that no route serves.
func TestOpenAPIRouteParity(t *testing.T) {
	t.Parallel()

	doc := parseOpenAPISpec(t)
	documented := specOperations(t, doc)

	server := newParityTestServer(t)
	engine := server.setupRouter()

	registered := make(map[string]bool)

	var undocumented, unexempt []string

	usedExemptions := make(map[string]bool)

	for _, route := range engine.Routes() {
		key := operationKey(route.Method, route.Path)

		if !strings.HasPrefix(route.Path, apiV1Prefix+"/") && route.Path != apiV1Prefix {
			if _, ok := exemptRoutes[key]; ok {
				usedExemptions[key] = true
			} else {
				unexempt = append(unexempt, key)
			}

			continue
		}

		specKey := operationKey(route.Method, ginPathToSpecPath(route.Path))
		registered[specKey] = true

		if !documented[specKey] {
			undocumented = append(undocumented, fmt.Sprintf("%s (gin route %s)", specKey, key))
		}
	}

	var orphaned []string

	for key := range documented {
		if !registered[key] {
			orphaned = append(orphaned, key)
		}
	}

	// Keep exemptRoutes honest: an entry that matches nothing is dead weight.
	var staleExemptions []string

	for key := range exemptRoutes {
		if !usedExemptions[key] {
			staleExemptions = append(staleExemptions, key)
		}
	}

	sort.Strings(undocumented)
	sort.Strings(orphaned)
	sort.Strings(unexempt)
	sort.Strings(staleExemptions)

	for _, key := range undocumented {
		t.Errorf("route registered but missing from internal/api/openapi.yml: %s", key)
	}

	for _, key := range orphaned {
		t.Errorf("internal/api/openapi.yml documents a path with no registered route: %s", key)
	}

	for _, key := range unexempt {
		t.Errorf("route outside %s is neither documented nor listed in exemptRoutes: %s", apiV1Prefix, key)
	}

	for _, key := range staleExemptions {
		t.Errorf("exemptRoutes lists a route that is no longer registered: %s", key)
	}
}

// TestOpenAPIOperationIDs asserts every operation carries a unique
// operationId: generated clients name their methods after it, so a missing or
// duplicated id silently breaks client generation.
func TestOpenAPIOperationIDs(t *testing.T) {
	t.Parallel()

	doc := parseOpenAPISpec(t)

	seen := make(map[string]string)

	var missing, duplicated []string

	for path, item := range doc.Paths {
		for key, raw := range item {
			if !openapiMethods[strings.ToLower(key)] {
				continue
			}

			opKey := operationKey(strings.ToUpper(key), path)

			op, ok := raw.(map[string]any)
			if !ok {
				missing = append(missing, opKey)

				continue
			}

			id, _ := op["operationId"].(string)
			if id == "" {
				missing = append(missing, opKey)

				continue
			}

			if previous, clash := seen[id]; clash {
				duplicated = append(duplicated, fmt.Sprintf("%q used by %s and %s", id, previous, opKey))

				continue
			}

			seen[id] = opKey
		}
	}

	sort.Strings(missing)
	sort.Strings(duplicated)

	for _, key := range missing {
		t.Errorf("operation without an operationId: %s", key)
	}

	for _, clash := range duplicated {
		t.Errorf("duplicate operationId: %s", clash)
	}
}

// operationsDeclaring429 is the exhaustive, hand-maintained allowlist of
// operationIds allowed to document a '429' response. 429 is a cross-cutting
// middleware outcome every operation can produce (see internal/api/server.go,
// PreAuthMiddleware/PostAuthMiddleware), so declaring it per-operation is only
// useful where a client must branch on it instead of just retrying: the
// session-validation path, where mistaking a 429 for an invalid session or bad
// credentials logs a user out or shows the wrong error (see
// specs/done/2026/08/2026-08-07-01-429-must-not-log-out.md). Adding a seventh
// entry here is a deliberate act that should be justified in code review, not
// something that drifts back in — do not "fix" a future failure by growing
// this list without re-reading that rationale.
var operationsDeclaring429 = map[string]bool{
	"getCurrentUser":         true,
	"login":                  true,
	"logout":                 true,
	"changePasswordPreLogin": true,
	"oauthExchange":          true,
	"deviceAuthorization":    true,
}

// TestOpenAPI429OnlyOnAllowlist asserts that the set of operations declaring a
// '429' response in openapi.yml matches operationsDeclaring429 exactly. 429 is
// ambient (every endpoint can answer it), so anywhere outside the allowlist it
// is noise at best and misleading at worst — see the allowlist doc comment.
func TestOpenAPI429OnlyOnAllowlist(t *testing.T) {
	t.Parallel()

	doc := parseOpenAPISpec(t)

	declared := make(map[string]bool)

	for path, item := range doc.Paths {
		for key, raw := range item {
			if !openapiMethods[strings.ToLower(key)] {
				continue
			}

			op, ok := raw.(map[string]any)
			if !ok {
				continue
			}

			id, _ := op["operationId"].(string)

			responses, ok := op["responses"].(map[string]any)
			if !ok {
				continue
			}

			if _, has429 := responses["429"]; has429 {
				if id == "" {
					id = operationKey(strings.ToUpper(key), path)
				}

				declared[id] = true
			}
		}
	}

	var unexpected, missing []string

	for id := range declared {
		if !operationsDeclaring429[id] {
			unexpected = append(unexpected, id)
		}
	}

	for id := range operationsDeclaring429 {
		if !declared[id] {
			missing = append(missing, id)
		}
	}

	sort.Strings(unexpected)
	sort.Strings(missing)

	for _, id := range unexpected {
		t.Errorf("operation %q declares a '429' response but is not in the operationsDeclaring429 "+
			"allowlist in this test — 429 is ambient middleware behavior and is only documented "+
			"per-operation where a client must branch on it; either remove the '429:' response "+
			"block from this operation in openapi.yml, or add it to the allowlist with a "+
			"reviewed justification", id)
	}

	for _, id := range missing {
		t.Errorf("operationsDeclaring429 allowlist expects operation %q to declare a '429' response "+
			"in openapi.yml, but it does not", id)
	}
}
