package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"git-bridge/internal/consumer"
	"git-bridge/internal/task"
)

// specPaths extracts the set of "METHOD path" pairs from the embedded spec.
func specPaths(t *testing.T) map[string]bool {
	t.Helper()
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatalf("failed to parse openapi.json: %v", err)
	}
	out := map[string]bool{}
	for p, ops := range spec.Paths {
		for method := range ops {
			out[strings.ToUpper(method)+" "+p] = true
		}
	}
	return out
}

// fullyWiredRoutes returns the route table with every optional dependency
// present, which is the configuration the spec documents.
func fullyWiredRoutes() []route {
	webhook := consumer.NewWebhook(task.NewGroup(context.Background()), nil, "", "", nil)
	retry := consumer.NewRetry(task.NewGroup(context.Background()), nil, "token")
	return routes(webhook, retry)
}

// TestOpenAPICoversAllRoutes checks that the route table and the spec match
// exactly, in both directions.
//
// This is what makes a hand-written OpenAPI spec sustainable here: adding an
// endpoint without updating the spec (or the reverse) fails CI, so drift is
// caught by a machine rather than by discipline. It is not hypothetical —
// /retry/mirror was registered and served for a long time while missing from
// the spec entirely, which is exactly what this test now prevents.
func TestOpenAPICoversAllRoutes(t *testing.T) {
	inSpec := specPaths(t)

	inCode := map[string]bool{}
	for _, rt := range fullyWiredRoutes() {
		inCode[rt.Method+" "+rt.Path] = true
	}

	var missingInSpec, missingInCode []string
	for k := range inCode {
		if !inSpec[k] {
			missingInSpec = append(missingInSpec, k)
		}
	}
	for k := range inSpec {
		if !inCode[k] {
			missingInCode = append(missingInCode, k)
		}
	}
	sort.Strings(missingInSpec)
	sort.Strings(missingInCode)

	if len(missingInSpec) > 0 {
		t.Errorf("served but absent from openapi.json (update the spec): %v", missingInSpec)
	}
	if len(missingInCode) > 0 {
		t.Errorf("in openapi.json but not served (remove from the spec): %v", missingInCode)
	}
}

// TestOpenAPISpecIsWellFormed checks the embedded spec has the minimum shape.
// A malformed JSON file still compiles when embedded, so it is caught here.
func TestOpenAPISpecIsWellFormed(t *testing.T) {
	var spec struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(spec.OpenAPI, "3.") {
		t.Errorf("want OpenAPI 3.x, got %q", spec.OpenAPI)
	}
	if spec.Info.Title == "" || spec.Info.Version == "" {
		t.Errorf("info.title and info.version are required, got %+v", spec.Info)
	}
	if len(spec.Paths) == 0 {
		t.Error("paths is empty")
	}
	// servers is optional for this app. Its Swagger UI is only ever served at
	// the public host root, where the default base "/" already resolves to this
	// app, so pinning it buys nothing. The portal-proxied apps are the ones that
	// must pin it to "." — there "/" would leave the app and hit the portal.
	//
	// Should anyone add it back, it still has to be relative for the same
	// reason, so the check below stays.
	for _, sv := range spec.Servers {
		if strings.HasPrefix(sv.URL, "/") || strings.Contains(sv.URL, "://") {
			t.Errorf("servers[].url must be relative, got %q", sv.URL)
		}
	}
}

// TestOpenAPIRefsResolve checks every $ref points at something in this document.
// A typo in a $ref only shows up when someone opens the Swagger UI.
func TestOpenAPIRefsResolve(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(openAPISpec, &doc); err != nil {
		t.Fatalf("failed to parse openapi.json: %v", err)
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				if !strings.HasPrefix(ref, "#/") {
					t.Errorf("external $ref is not allowed in a single-file spec: %s", ref)
				} else {
					cur := any(doc)
					for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
						m, ok := cur.(map[string]any)
						if !ok {
							t.Errorf("unresolvable $ref: %s", ref)
							return
						}
						if cur, ok = m[seg]; !ok {
							t.Errorf("unresolvable $ref: %s", ref)
							return
						}
					}
				}
			}
			for _, vv := range x {
				walk(vv)
			}
		case []any:
			for _, vv := range x {
				walk(vv)
			}
		}
	}
	walk(doc)
}

// TestAPIDocsUsesRelativeSpecURL checks the docs page fetches the spec with a
// relative URL. An absolute "/openapi.json" breaks once this service is reached
// through a proxy that strips a path prefix, which is the planned console setup.
func TestAPIDocsUsesRelativeSpecURL(t *testing.T) {
	html := string(apiDocsHTML)
	if strings.Contains(html, `url: "/openapi.json"`) || strings.Contains(html, `url: '/openapi.json'`) {
		t.Error(`spec URL is absolute — use the relative "openapi.json"`)
	}
	if !strings.Contains(html, `url: "openapi.json"`) {
		t.Error(`could not find the relative url: "openapi.json"`)
	}
}

// TestAPIDocsAssetsArePinned checks the Swagger UI assets are pinned to an exact
// version and carry an SRI hash. This page shares an origin with /retry/mirror,
// which triggers real mirror pushes, so a floating tag on a third-party CDN is a
// supply-chain path into that context.
func TestAPIDocsAssetsArePinned(t *testing.T) {
	html := string(apiDocsHTML)
	if strings.Contains(html, "swagger-ui-dist@5/") {
		t.Error("Swagger UI assets use a floating major tag — pin an exact version")
	}
	if n := strings.Count(html, "integrity=\"sha384-"); n != 2 {
		t.Errorf("want SRI hashes on both the CSS and JS asset, found %d", n)
	}
}

// TestDocsEndpoints checks both documentation endpoints actually respond.
func TestDocsEndpoints(t *testing.T) {
	srv := httptest.NewServer(NewMux(nil, nil))
	defer srv.Close()

	for _, tc := range []struct {
		path        string
		contentType string
	}{
		{"/openapi.json", "application/json"},
		{"/api-docs", "text/html"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			res, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("want 200, got %d", res.StatusCode)
			}
			if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, tc.contentType) {
				t.Errorf("want Content-Type %q, got %q", tc.contentType, ct)
			}
		})
	}
}

// TestNoDirectMuxRegistration checks routes are not registered behind the route
// table's back. A bypassing registration is invisible to the drift test above,
// so it would appear in neither the table nor the spec.
func TestNoDirectMuxRegistration(t *testing.T) {
	src, err := os.ReadFile("health.go")
	if err != nil {
		t.Fatalf("failed to read health.go: %v", err)
	}
	if n := strings.Count(string(src), "mux.HandleFunc("); n != 1 {
		t.Errorf("found %d mux.HandleFunc calls — only register() should have one", n)
	}
}
