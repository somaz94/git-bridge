package provider

import (
	"strings"
	"testing"

	"git-bridge/internal/config"
)

func TestCodeCommit_Remote(t *testing.T) {
	p := NewCodeCommit(config.ProviderConfig{
		Region: "eu-central-1",
		Credentials: map[string]string{
			"git_username": "user-at-123",
			"git_password": "pass123",
		},
	})

	rem := p.Remote("my-repo")

	if !strings.Contains(rem.URL, "git-codecommit.eu-central-1.amazonaws.com") {
		t.Errorf("URL missing codecommit host: %s", rem.URL)
	}
	if !strings.Contains(rem.URL, "/v1/repos/my-repo") {
		t.Errorf("URL missing repo path: %s", rem.URL)
	}
	if !strings.HasPrefix(rem.URL, "https://") {
		t.Errorf("URL should start with https://: %s", rem.URL)
	}
	if rem.Username != "user-at-123" || rem.Password != "pass123" {
		t.Errorf("credentials = %q/%q, want user-at-123/pass123", rem.Username, rem.Password)
	}
	if p.Type() != "codecommit" {
		t.Errorf("type = %q, want codecommit", p.Type())
	}
}

// Whatever characters the credentials contain, the URL side is left alone. Back
// when they went into userinfo, percent encoding was needed and a password
// containing `/` or `@` broke silently; the values no longer pass through the
// URL, so there is no encoding at all.
func TestCodeCommit_Remote_CredentialsAreNotInTheURL(t *testing.T) {
	p := NewCodeCommit(config.ProviderConfig{
		Region: "eu-central-1",
		Credentials: map[string]string{
			"git_username": "user@123",
			"git_password": "pass/with=chars",
		},
	})

	rem := p.Remote("repo")

	if strings.Contains(rem.URL, "user@123") || strings.Contains(rem.URL, "pass") {
		t.Errorf("credentials leaked into URL: %s", rem.URL)
	}
	if rem.URL != "https://git-codecommit.eu-central-1.amazonaws.com/v1/repos/repo" {
		t.Errorf("unexpected URL: %s", rem.URL)
	}
	if rem.Password != "pass/with=chars" {
		t.Errorf("password was mangled: %q", rem.Password)
	}
}

func TestGitLab_Remote(t *testing.T) {
	p := NewGitLab(config.ProviderConfig{
		BaseURL: "http://gitlab.example.com",
		Credentials: map[string]string{
			"token": "glpat-test123",
		},
	})

	rem := p.Remote("server/my-repo")

	if rem.URL != "http://gitlab.example.com/server/my-repo.git" {
		t.Errorf("unexpected URL: %s", rem.URL)
	}
	if rem.Username != "oauth2" || rem.Password != "glpat-test123" {
		t.Errorf("credentials = %q/%q, want oauth2/glpat-test123", rem.Username, rem.Password)
	}
	if p.Type() != "gitlab" {
		t.Errorf("type = %q, want gitlab", p.Type())
	}
}

func TestGitLab_Remote_HTTPS(t *testing.T) {
	p := NewGitLab(config.ProviderConfig{
		BaseURL: "https://gitlab.com",
		Credentials: map[string]string{
			"token": "glpat-xyz",
		},
	})

	rem := p.Remote("org/repo")
	if !strings.HasPrefix(rem.URL, "https://") {
		t.Errorf("URL should start with https://: %s", rem.URL)
	}
}

func TestGitLab_Remote_TrailingSlash(t *testing.T) {
	p := NewGitLab(config.ProviderConfig{
		BaseURL: "http://gitlab.example.com/",
		Credentials: map[string]string{
			"token": "tok",
		},
	})

	rem := p.Remote("team/repo")
	if strings.Contains(rem.URL, "//team") {
		t.Errorf("URL has double slash: %s", rem.URL)
	}
}

// Even if credentials slip into base_url, they do not survive into the URL
// Remote emits. Remote's contract is "no credentials in the URL", so a single
// configuration mistake must not be able to break it.
func TestGitLab_Remote_DropsUserinfoFromBaseURL(t *testing.T) {
	p := NewGitLab(config.ProviderConfig{
		BaseURL:     "https://oauth2:leaked@gitlab.example.com",
		Credentials: map[string]string{"token": "tok"},
	})

	rem := p.Remote("team/repo")
	if strings.Contains(rem.URL, "leaked") || strings.Contains(rem.URL, "@") {
		t.Errorf("userinfo survived into the clone URL: %s", rem.URL)
	}
}

func TestGitHub_Remote(t *testing.T) {
	p := NewGitHub(config.ProviderConfig{
		Credentials: map[string]string{
			"token": "ghp_test123",
		},
	})

	rem := p.Remote("org/my-repo")

	if rem.URL != "https://github.com/org/my-repo.git" {
		t.Errorf("unexpected URL: %s", rem.URL)
	}
	if strings.Contains(rem.URL, "ghp_test123") {
		t.Errorf("token leaked into URL: %s", rem.URL)
	}
	// The token goes out in the password slot, not the user name — the
	// `https://<token>@github.com/...` form from the days of embedding it in the
	// URL is unusable once the credentials are kept separate.
	if rem.Username != "x-access-token" || rem.Password != "ghp_test123" {
		t.Errorf("credentials = %q/%q, want x-access-token/ghp_test123", rem.Username, rem.Password)
	}
	if p.Type() != "github" {
		t.Errorf("type = %q, want github", p.Type())
	}
}

func TestRemote_HasCredentials(t *testing.T) {
	tests := []struct {
		name string
		rem  Remote
		want bool
	}{
		{"both set", Remote{Username: "u", Password: "p"}, true},
		{"password only", Remote{Password: "p"}, true},
		{"username only", Remote{Username: "u"}, true},
		{"neither", Remote{URL: "https://example.com/repo.git"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rem.HasCredentials(); got != tt.want {
				t.Errorf("HasCredentials() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNew_ValidProviders(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.ProviderConfig
		wantType string
	}{
		{
			name:     "codecommit",
			cfg:      config.ProviderConfig{Type: "codecommit", Region: "us-east-1", Credentials: map[string]string{}},
			wantType: "codecommit",
		},
		{
			name:     "gitlab",
			cfg:      config.ProviderConfig{Type: "gitlab", BaseURL: "http://gl.test", Credentials: map[string]string{}},
			wantType: "gitlab",
		},
		{
			name:     "github",
			cfg:      config.ProviderConfig{Type: "github", Credentials: map[string]string{}},
			wantType: "github",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(tt.name, tt.cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Type() != tt.wantType {
				t.Errorf("type = %q, want %q", p.Type(), tt.wantType)
			}
		})
	}
}

// --- WebURL tests ---

func TestCodeCommit_WebURL(t *testing.T) {
	p := NewCodeCommit(config.ProviderConfig{
		Region:      "eu-central-1",
		Credentials: map[string]string{"git_username": "u", "git_password": "p"},
	})

	url := p.WebURL("my-repo")
	if !strings.Contains(url, "eu-central-1.console.aws.amazon.com") {
		t.Errorf("URL missing region console host: %s", url)
	}
	if !strings.Contains(url, "/repositories/my-repo/browse") {
		t.Errorf("URL missing repo browse path: %s", url)
	}
}

func TestGitLab_WebURL(t *testing.T) {
	p := NewGitLab(config.ProviderConfig{
		BaseURL:     "https://gitlab.example.com",
		Credentials: map[string]string{"token": "t"},
	})

	url := p.WebURL("team/my-repo")
	if url != "https://gitlab.example.com/team/my-repo" {
		t.Errorf("unexpected URL: %s", url)
	}
}

func TestGitHub_WebURL(t *testing.T) {
	p := NewGitHub(config.ProviderConfig{
		Credentials: map[string]string{"token": "t"},
	})

	url := p.WebURL("org/my-repo")
	if url != "https://github.com/org/my-repo" {
		t.Errorf("unexpected URL: %s", url)
	}
}

func TestNew_UnknownProvider(t *testing.T) {
	_, err := New("test", config.ProviderConfig{Type: "bitbucket"})
	if err == nil {
		t.Fatal("expected error for unknown provider type")
	}
}

func TestGitLab_Remote_InvalidURL(t *testing.T) {
	p := &GitLab{
		baseURL: "://invalid",
		token:   "tok",
	}
	rem := p.Remote("repo")
	if !strings.Contains(rem.URL, "://invalid/repo.git") {
		t.Errorf("expected fallback URL, got %s", rem.URL)
	}
	// The token stays out of the URL on the fallback path too.
	if strings.Contains(rem.URL, "tok") {
		t.Errorf("token leaked into fallback URL: %s", rem.URL)
	}
}
