package provider

import (
	"fmt"

	"git-bridge/internal/config"
)

// Remote is a remote repository address together with the credentials to
// present at that address.
//
// The point is that the URL and the credentials are kept apart. A single clone
// URL used to carry both as `https://oauth2:<token>@host/team/repo.git`, and
// that string became a git command-line argument as-is, leaking the token into
// `ps` output and into the cache directory's remote.origin.url. The URL now
// holds no credentials, and Username/Password reach git only as environment
// variables by way of GIT_ASKPASS (see internal/askpass).
//
// A side effect is that the character restrictions on credentials are gone.
// Putting them in userinfo needed percent encoding, so a password containing `/`
// or `@` broke silently; an environment variable passes them through
// unchanged.
type Remote struct {
	// URL is the HTTPS address with no credentials in it. It is safe to carry in
	// logs and error messages.
	URL string
	// Username/Password are handed over only when git asks. If both are empty,
	// access is unauthenticated.
	Username string
	Password string
}

// HasCredentials reports whether there is a credential to hand to git.
func (r Remote) HasCredentials() bool { return r.Username != "" || r.Password != "" }

// Provider builds remotes for a git provider.
type Provider interface {
	// Remote returns the credential-free HTTPS remote for a repo path,
	// together with the credentials git should be handed out of band.
	Remote(repoPath string) Remote
	// WebURL returns the browser URL for a repo path (no credentials).
	WebURL(repoPath string) string
	// Type returns the provider type name.
	Type() string
}

// New creates a Provider from config.
func New(name string, cfg config.ProviderConfig) (Provider, error) {
	switch cfg.Type {
	case "codecommit":
		return NewCodeCommit(cfg), nil
	case "gitlab":
		return NewGitLab(cfg), nil
	case "github":
		return NewGitHub(cfg), nil
	default:
		return nil, fmt.Errorf("unknown provider type %q for %q", cfg.Type, name)
	}
}
