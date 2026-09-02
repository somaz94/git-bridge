package provider

import (
	"fmt"

	"git-bridge/internal/config"
)

// githubTokenUser is the user name used when sending the token in the password
// slot of HTTP Basic. It is the standard spelling for a GitHub App installation
// token and works just as well for a PAT — GitHub authenticates on the token in
// the password slot as long as the name is not empty.
//
// The token used to be embedded in the URL in the user name slot
// (`https://<token>@github.com/...`). That form is unusable now that Remote
// keeps the URL and the credentials apart — the token goes out as the password
// only.
const githubTokenUser = "x-access-token"

type GitHub struct {
	token string
}

func NewGitHub(cfg config.ProviderConfig) *GitHub {
	return &GitHub{
		token: cfg.Credentials["token"],
	}
}

func (g *GitHub) Remote(repoPath string) Remote {
	return Remote{
		URL:      fmt.Sprintf("https://github.com/%s.git", repoPath),
		Username: githubTokenUser,
		Password: g.token,
	}
}

func (g *GitHub) WebURL(repoPath string) string {
	return fmt.Sprintf("https://github.com/%s", repoPath)
}

func (g *GitHub) Type() string { return "github" }
