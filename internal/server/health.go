package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"git-bridge/internal/consumer"
)

const shutdownTimeout = 5 * time.Second

const (
	// publicReadHeaderTimeout bounds a client that opens a connection and then
	// dawdles over the request line and headers. Without it such a connection is
	// held open indefinitely, and this listener is the one reachable from the
	// internet.
	publicReadHeaderTimeout = 10 * time.Second
	// publicReadTimeout bounds the whole request, body included.
	//
	// 🔴 Do not tighten this without doing the arithmetic against
	// webhook.max_body_size_mb. A body at the 10 MB cap has to arrive inside this
	// window, so 60s sets a floor of roughly 175 kB/s on the sender. Cutting a
	// large webhook short is the exact failure this listener was fixed to stop
	// doing — and a timeout drops it just as silently as a truncating reader did,
	// since neither GitLab nor GitHub retries a webhook.
	//
	// The reason to have it at all: the GitHub handler reads the body *before*
	// verifying the HMAC (there is no other order — the signature covers the
	// body), so an unauthenticated caller can make this listener hold the cap in
	// memory. A bound on how long it can do that is what keeps a slow trickle
	// from being free.
	publicReadTimeout = 60 * time.Second
	// publicWriteTimeout bounds a response. Every handler here answers promptly —
	// a webhook detaches its sync and replies "accepted" — so this only ever
	// fires on a stuck client, unlike the console's budget which has to cover a
	// restore doing git work inside the request.
	publicWriteTimeout = 30 * time.Second
)

//go:embed api-docs.html
var apiDocsHTML []byte

//go:embed openapi.json
var openAPISpec []byte

// route is one documented HTTP route.
//
// Keeping the routes in a table rather than calling mux.HandleFunc directly is
// what lets a unit test compare them against openapi.json — the mux itself
// cannot be enumerated, so drift between code and spec would otherwise go
// unnoticed (it already did: /retry/mirror was served for a long time without
// appearing in the spec).
type route struct {
	// Method is the documented HTTP method, used to match against the spec.
	// It is deliberately NOT used for registration — see register().
	Method  string
	Path    string
	Handler http.HandlerFunc
}

// routes returns every route this service documents, given the wired-up
// dependencies. Webhook routes only exist when a webhook consumer is present;
// the retry route is always registered when a retry handler exists, because the
// handler itself enforces the empty-token = 404 invariant and we want that
// response to be uniform.
func routes(webhook *consumer.Webhook, retry *consumer.Retry) []route {
	rs := []route{
		{"GET", "/health", healthHandler},
		{"GET", "/ready", healthHandler},
		{"GET", "/api-docs", apiDocsHandler},
		{"GET", "/openapi.json", openAPIHandler},
	}
	if webhook != nil {
		rs = append(rs,
			route{"POST", "/webhook/gitlab", webhook.GitLabHandler},
			route{"POST", "/webhook/github", webhook.GitHubHandler},
		)
	}
	if retry != nil {
		rs = append(rs, route{"POST", "/retry/mirror", retry.Handler})
	}
	return rs
}

// register adds the route to the mux by path only, without a method pattern.
//
// The handlers do their own method checks and answer with their own bodies
// (webhook and retry both return 405 themselves). Adding a method to the mux
// pattern would make the mux answer first with a different body, so registering
// by path keeps the wire behaviour byte-for-byte identical to before this table
// existed. Method above is documentation, not routing.
func (r route) register(mux *http.ServeMux) {
	mux.HandleFunc(r.Path, r.Handler)
}

// NewMux creates the HTTP handler with health checks, docs, webhook, and retry endpoints.
func NewMux(webhook *consumer.Webhook, retry *consumer.Retry) *http.ServeMux {
	mux := http.NewServeMux()

	for _, rt := range routes(webhook, retry) {
		rt.register(mux)
	}

	if webhook != nil {
		slog.Info("webhook endpoints registered", "gitlab", "/webhook/gitlab", "github", "/webhook/github")
	}
	if retry != nil {
		slog.Info("retry endpoint registered", "path", "/retry/mirror")
	}

	return mux
}

// apiDocsHandler serves the Swagger UI page. The page fetches the spec with a
// relative URL, so it also works behind a proxy that strips a path prefix.
func apiDocsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(apiDocsHTML)
}

// openAPIHandler serves the raw OpenAPI specification.
func openAPIHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(openAPISpec)
}

// newPublicServer builds the internet-facing listener.
//
// It is a separate function so a test can assert the timeouts are actually set:
// they are easy to drop in a refactor and nothing about the service's behaviour
// looks different when they are missing.
func newPublicServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: publicReadHeaderTimeout,
		ReadTimeout:       publicReadTimeout,
		WriteTimeout:      publicWriteTimeout,
	}
}

// RunServer starts the HTTP server with health checks, webhook, and retry endpoints.
func RunServer(ctx context.Context, port int, webhook *consumer.Webhook, retry *consumer.Retry) {
	mux := NewMux(webhook, retry)

	srv := newPublicServer(fmt.Sprintf(":%d", port), mux)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("server started", "port", port)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "git-bridge",
	})
}
