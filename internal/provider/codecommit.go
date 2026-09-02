package provider

import (
	"fmt"

	"git-bridge/internal/config"
)

type CodeCommit struct {
	region      string
	gitUsername string
	gitPassword string
}

func NewCodeCommit(cfg config.ProviderConfig) *CodeCommit {
	return &CodeCommit{
		region:      cfg.Region,
		gitUsername: cfg.Credentials["git_username"],
		gitPassword: cfg.Credentials["git_password"],
	}
}

// Remote returns the credential-free CodeCommit HTTPS address plus the git
// credentials to hand over separately.
//
// The two used to be joined as userinfo, which meant going through
// url.PathEscape; PathEscape is for paths and does not cover every character
// that carries meaning in userinfo. The values no longer pass through the URL at
// all, so no encoding is needed and a password containing `/` or `@` is used
// as-is.
func (c *CodeCommit) Remote(repoPath string) Remote {
	return Remote{
		URL:      fmt.Sprintf("https://git-codecommit.%s.amazonaws.com/v1/repos/%s", c.region, repoPath),
		Username: c.gitUsername,
		Password: c.gitPassword,
	}
}

func (c *CodeCommit) WebURL(repoPath string) string {
	return fmt.Sprintf("https://%s.console.aws.amazon.com/codesuite/codecommit/repositories/%s/browse", c.region, repoPath)
}

func (c *CodeCommit) Type() string { return "codecommit" }
