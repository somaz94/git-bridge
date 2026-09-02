// Package askpass implements the GIT_ASKPASS side channel that hands
// credentials to git.
//
// Credentials used to be embedded in the clone URL
// (`https://oauth2:<token>@gitlab.example.com/team/repo.git`). That URL becomes
// a git command-line argument as-is, so the token leaked in two places.
//
//  1. `ps` / `/proc/<pid>/cmdline` — anyone who can list processes on the same
//     node reads it. This is the exposure path that was actually confirmed.
//  2. The `remote.origin.url` a `--mirror` clone writes into the cache
//     directory config — it stores the URL it was handed, so the token stays on
//     the PVC forever (confirmed by inspection on 2026-08-25).
//
// git's own output is not on that list. git strips userinfo from the URLs it
// prints (transport_anonymize_url), so neither an error message nor FETCH_HEAD
// carries the token — that was checked before leaving it out. What this
// structure prevents is the two above.
//
// Credentials are now out of the URL entirely, and this helper answers only when
// git asks. The values travel by environment variable alone, and an environment
// is `/proc/<pid>/environ`, readable only by the same uid or by root, so its
// exposure surface is narrower than the command line's.
//
// The helper is not a separate script but the git-bridge binary itself. That
// avoids putting another shell script in the container or writing a file at
// runtime, and because the helper and the service are the same build, the two
// cannot drift apart.
package askpass

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// EnvActive set to "1" means this process was re-executed as the askpass
	// helper rather than as the service. git calls GIT_ASKPASS with a single
	// prompt string appended, and telling the two apart by argument count alone
	// would break the moment any CLI flag is added, so the marker is explicit.
	EnvActive = "GIT_BRIDGE_ASKPASS"
	// EnvUsername/EnvPassword hold the values the helper hands back.
	EnvUsername = "GIT_BRIDGE_ASKPASS_USERNAME"
	EnvPassword = "GIT_BRIDGE_ASKPASS_PASSWORD"
)

// usernamePromptPrefix is the prompt prefix git uses when it asks for a user
// name. git emits two of them: "Username for 'https://gitlab.example.com': "
// and "Password for 'https://oauth2@gitlab.example.com': ". The caller pins the
// locale with LC_ALL=C, so a translated prompt never arrives.
const usernamePromptPrefix = "username"

// Serve writes the answer to out and returns true if this process was invoked
// as the askpass helper. Otherwise it does nothing and returns false — the
// caller (main) then brings up the service as usual.
//
// args is the argument list with the program name removed (os.Args[1:]).
func Serve(args []string, out io.Writer) bool {
	if os.Getenv(EnvActive) != "1" {
		return false
	}
	prompt := ""
	if len(args) > 0 {
		prompt = args[0]
	}
	// git does not treat the newline as part of the value and trims it off, so
	// appending one is safe, and it reads better when running the helper by hand.
	//
	// A write failure is ignored. The only place to report one here is stdout,
	// and stdout is exactly the channel git reads the credential from — an error
	// message laid on top of it is what git would take as the value. When
	// nothing gets written at all, git fails authentication with an empty
	// credential, and that is the most accurate signal this situation has.
	_, _ = fmt.Fprintln(out, Answer(prompt))
	return true
}

// Answer picks the value that matches the prompt.
//
// Anything that is not a user name prompt gets the password unconditionally.
// Going with "hand back nothing when in doubt" would have git attempt
// authentication with an empty password, take a 401, and produce a failure that
// does not distinguish a bad credential from a prompt that failed to parse. The
// password is the default because git asks for both unless the URL carries a
// user name, and of the two the password is the one it hurts to get wrong.
func Answer(prompt string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(prompt)), usernamePromptPrefix) {
		return os.Getenv(EnvUsername)
	}
	return os.Getenv(EnvPassword)
}
