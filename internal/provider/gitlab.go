package provider

import (
	"fmt"
	"net/url"
	"strings"

	"git-bridge/internal/config"
)

// gitlabTokenUser is the user name used when presenting a PAT or group token
// over HTTP Basic. GitLab only looks at the token in the password slot, so the
// name itself is a convention and not a secret.
const gitlabTokenUser = "oauth2"

type GitLab struct {
	baseURL string
	token   string
}

func NewGitLab(cfg config.ProviderConfig) *GitLab {
	return &GitLab{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		token:   cfg.Credentials["token"],
	}
}

func (g *GitLab) Remote(repoPath string) Remote {
	return Remote{
		URL:      g.cloneURL(repoPath),
		Username: gitlabTokenUser,
		Password: g.token,
	}
}

// cloneURL builds the credential-free clone address.
//
// If userinfo slips into baseURL (a misconfigured base_url with a token in it,
// say), it is dropped here — Remote's contract is that the URL this type emits
// carries no credentials.
func (g *GitLab) cloneURL(repoPath string) string {
	u, err := url.Parse(g.baseURL)
	if err != nil {
		return fmt.Sprintf("%s/%s.git", g.baseURL, repoPath)
	}
	u.User = nil
	u.Path = "/" + repoPath + ".git"
	return u.String()
}

func (g *GitLab) WebURL(repoPath string) string {
	return fmt.Sprintf("%s/%s", g.baseURL, repoPath)
}

func (g *GitLab) Type() string { return "gitlab" }
