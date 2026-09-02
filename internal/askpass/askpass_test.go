package askpass

import (
	"bytes"
	"testing"
)

func TestAnswer(t *testing.T) {
	t.Setenv(EnvUsername, "oauth2")
	t.Setenv(EnvPassword, "glpat-secret")

	tests := []struct {
		name   string
		prompt string
		want   string
	}{
		// The two prompts git actually emits.
		{"username prompt", "Username for 'https://gitlab.example.com': ", "oauth2"},
		{"password prompt", "Password for 'https://oauth2@gitlab.example.com': ", "glpat-secret"},
		// Case and surrounding whitespace can shift between git versions, so absorb them.
		{"lowercase", "username for 'x': ", "oauth2"},
		{"leading space", "  Username for 'x': ", "oauth2"},
		// An unrecognized prompt is answered with the password — an empty value makes
		// an auth failure indistinguishable from a prompt that failed to parse.
		{"unknown prompt", "Enter passphrase: ", "glpat-secret"},
		{"empty prompt", "", "glpat-secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Answer(tt.prompt); got != tt.want {
				t.Errorf("Answer(%q) = %q, want %q", tt.prompt, got, tt.want)
			}
		})
	}
}

func TestServe_InactiveIsANoOp(t *testing.T) {
	t.Setenv(EnvActive, "")
	var out bytes.Buffer
	if Serve([]string{"Username for 'x': "}, &out) {
		t.Fatal("Serve claimed the process without the marker set")
	}
	if out.Len() != 0 {
		t.Errorf("Serve wrote %q while inactive", out.String())
	}
}

func TestServe_WritesTheCredentialAndClaimsTheProcess(t *testing.T) {
	t.Setenv(EnvActive, "1")
	t.Setenv(EnvUsername, "oauth2")
	t.Setenv(EnvPassword, "glpat-secret")

	var out bytes.Buffer
	if !Serve([]string{"Password for 'https://oauth2@gitlab.example.com': "}, &out) {
		t.Fatal("Serve did not claim the process with the marker set")
	}
	// git reads only the first line. It trims the newline after the value, so one may be there.
	if got := out.String(); got != "glpat-secret\n" {
		t.Errorf("output = %q, want %q", got, "glpat-secret\n")
	}
}

// Being called with no arguments must not kill it. git does not do that, but a
// panic here would send a stack trace into git's stdin instead of a credential
// and turn it into an authentication failure with no discoverable cause.
func TestServe_NoArgs(t *testing.T) {
	t.Setenv(EnvActive, "1")
	t.Setenv(EnvPassword, "pw")

	var out bytes.Buffer
	if !Serve(nil, &out) {
		t.Fatal("Serve did not claim the process")
	}
	if got := out.String(); got != "pw\n" {
		t.Errorf("output = %q, want %q", got, "pw\n")
	}
}
