package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git-bridge/internal/config"
)

func TestNoop_Send(t *testing.T) {
	n := NewNoop()
	// Should not panic
	n.Send(Message{Level: "success", Title: "test", Body: "body"})
	n.Send(Message{Level: "error", Title: "err", Body: "failed"})
}

func TestSlack_Send_Success(t *testing.T) {
	var received map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s := NewSlack(config.SlackConfig{
		WebhookURL: server.URL,
		Channel:    "#test",
	})

	s.Send(Message{
		Level: "success",
		Title: "Mirror Complete",
		Body:  "codecommit/repo → gitlab/repo",
	})

	if text := attachmentText(t, received); len(text) == 0 {
		t.Error("text should not be empty")
	}

	ch, ok := received["channel"].(string)
	if !ok || ch != "#test" {
		t.Errorf("channel = %q, want #test", ch)
	}
}

func TestSlack_Send_Error(t *testing.T) {
	var received map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s := NewSlack(config.SlackConfig{WebhookURL: server.URL})

	s.Send(Message{
		Level: "error",
		Title: "Mirror Failed",
		Body:  "clone failed",
	})

	text := attachmentText(t, received)
	if len(text) == 0 {
		t.Error("text should not be empty")
	}
}

func TestSlack_Send_NoChannel(t *testing.T) {
	var received map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s := NewSlack(config.SlackConfig{WebhookURL: server.URL})
	s.Send(Message{Level: "success", Title: "test", Body: "ok"})

	if _, ok := received["channel"]; ok {
		t.Error("channel should not be set when empty")
	}
}

func TestSlack_Send_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	s := NewSlack(config.SlackConfig{WebhookURL: server.URL})
	// Should not panic on server error
	s.Send(Message{Level: "error", Title: "test", Body: "body"})
}

func TestSlack_Send_InvalidURL(t *testing.T) {
	s := NewSlack(config.SlackConfig{WebhookURL: "http://invalid.invalid.invalid:99999"})
	// Should not panic on connection error
	s.Send(Message{Level: "error", Title: "test", Body: "body"})
}

func TestSlack_Send_Warning(t *testing.T) {
	var received map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s := NewSlack(config.SlackConfig{WebhookURL: server.URL, Channel: "#alerts"})
	s.Send(Message{Level: "warning", Title: "Slow Sync", Body: "took 5m"})

	if text := attachmentText(t, received); len(text) == 0 {
		t.Fatal("text is empty")
	}
}

func TestSlack_Send_UnknownLevel(t *testing.T) {
	var received map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s := NewSlack(config.SlackConfig{WebhookURL: server.URL})
	// A level that is not in the map must fall back to success (✅).
	s.Send(Message{Level: "bogus", Title: "Unknown", Body: "fallback"})

	text := attachmentText(t, received)
	if len(text) == 0 || text[:len("✅")] != "✅" {
		t.Errorf("unknown level should fall back to ✅ prefix, got %q", text)
	}
}

// When Message.WebhookURL is set, Slack.Send must POST to that URL instead of
// the notifier's configured default. Two test servers cover both branches.
func TestSlack_Send_MessageWebhookURLOverride(t *testing.T) {
	defaultHits := 0
	overrideHits := 0

	defaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defaultHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer defaultSrv.Close()
	overrideSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		overrideHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer overrideSrv.Close()

	s := NewSlack(config.SlackConfig{WebhookURL: defaultSrv.URL})

	// 1) Without override → hits the default server
	s.Send(Message{Level: "success", Title: "T", Body: "B"})
	if defaultHits != 1 || overrideHits != 0 {
		t.Errorf("default branch: defaultHits=%d overrideHits=%d", defaultHits, overrideHits)
	}

	// 2) With override → hits the override server, not the default
	s.Send(Message{Level: "success", Title: "T", Body: "B", WebhookURL: overrideSrv.URL})
	if defaultHits != 1 || overrideHits != 1 {
		t.Errorf("override branch: defaultHits=%d overrideHits=%d", defaultHits, overrideHits)
	}
}

func TestNoop_Send_DoesNothing(t *testing.T) {
	// Noop.Send must not panic and must accept any Message. There is no
	// observable side-effect — we exercise the call site for coverage.
	n := NewNoop()
	n.Send(Message{Level: "success", Title: "T", Body: "B"})
	n.Send(Message{Level: "error", Title: "X", Body: "Y", WebhookURL: "ignored"})
}

func TestNewSlack(t *testing.T) {
	s := NewSlack(config.SlackConfig{
		WebhookURL: "https://hooks.slack.com/test",
		Channel:    "#ch",
	})
	if s.webhookURL != "https://hooks.slack.com/test" {
		t.Errorf("unexpected webhookURL: %q", s.webhookURL)
	}
	if s.channel != "#ch" {
		t.Errorf("unexpected channel: %q", s.channel)
	}
	if s.client == nil {
		t.Error("client should not be nil")
	}
}

// attachmentText pulls the rendered body out of the payload.
//
// The message moved into an attachment when the colour bar was added: only
// attachments carry a colour, so the top-level "text" is now deliberately
// empty. Every assertion on the rendered message goes through here.
func attachmentText(t *testing.T, payload map[string]interface{}) string {
	t.Helper()
	return attachmentField(t, payload, "text")
}

func attachmentColor(t *testing.T, payload map[string]interface{}) string {
	t.Helper()
	return attachmentField(t, payload, "color")
}

func attachmentField(t *testing.T, payload map[string]interface{}, key string) string {
	t.Helper()
	atts, ok := payload["attachments"].([]interface{})
	if !ok || len(atts) != 1 {
		t.Fatalf("want exactly 1 attachment, got %#v", payload["attachments"])
	}
	att, ok := atts[0].(map[string]interface{})
	if !ok {
		t.Fatalf("attachment is not an object: %#v", atts[0])
	}
	v, ok := att[key].(string)
	if !ok {
		t.Fatalf("attachment %q field missing in %#v", key, att)
	}
	return v
}

// Duplicating the body at the top level would render it twice — once above the
// coloured attachment and once inside it.
func TestSlack_Send_BodyLivesOnlyInTheAttachment(t *testing.T) {
	var received map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	NewSlack(config.SlackConfig{WebhookURL: server.URL}).Send(Message{
		Level: "success", Title: "Mirror Complete", Body: "codecommit/repo → gitlab/repo",
	})

	if top, present := received["text"]; present && top != "" {
		t.Errorf("top-level text = %q, want it empty so the body is not rendered twice", top)
	}
	if body := attachmentText(t, received); !strings.Contains(body, "codecommit/repo → gitlab/repo") {
		t.Errorf("attachment text = %q, want it to carry the body", body)
	}
}

// The colour is what makes a failure findable while scrolling past routine
// green syncs, so each level must map to a distinct bar.
func TestSlack_Send_LevelPicksTheColour(t *testing.T) {
	for _, tc := range []struct{ level, want string }{
		{"success", "#2eb886"},
		{"error", "#a30200"},
		{"warning", "#daa038"},
		{"bogus", "#2eb886"}, // unknown falls back to success, matching the ✅ prefix
	} {
		t.Run(tc.level, func(t *testing.T) {
			var received map[string]interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&received)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			NewSlack(config.SlackConfig{WebhookURL: server.URL}).Send(Message{
				Level: tc.level, Title: "T", Body: "B",
			})

			if got := attachmentColor(t, received); got != tc.want {
				t.Errorf("colour for %q = %q, want %q", tc.level, got, tc.want)
			}
		})
	}
}

// Without mrkdwn_in the *bold* title renders as literal asterisks inside an
// attachment — attachments are not markdown by default, unlike top-level text.
func TestSlack_Send_AttachmentOptsIntoMarkdown(t *testing.T) {
	var received map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	NewSlack(config.SlackConfig{WebhookURL: server.URL}).Send(Message{
		Level: "success", Title: "Mirror Complete", Body: "b",
	})

	atts := received["attachments"].([]interface{})
	att := atts[0].(map[string]interface{})
	mrkdwn, ok := att["mrkdwn_in"].([]interface{})
	if !ok || len(mrkdwn) == 0 || mrkdwn[0] != "text" {
		t.Errorf("mrkdwn_in = %#v, want [\"text\"] so *bold* renders", att["mrkdwn_in"])
	}
}

// The lock-screen preview must stay short — the body carries SHAs and URLs.
func TestSlack_Send_FallbackIsTitleOnly(t *testing.T) {
	var received map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	NewSlack(config.SlackConfig{WebhookURL: server.URL}).Send(Message{
		Level: "error", Title: "Forced Update: repo", Body: "Deleted tip: deadbeef\nUndo: git fetch ...",
	})

	fallback := attachmentField(t, received, "fallback")
	if !strings.Contains(fallback, "Forced Update: repo") {
		t.Errorf("fallback = %q, want it to name the event", fallback)
	}
	if strings.Contains(fallback, "deadbeef") {
		t.Errorf("fallback = %q, want it free of body detail", fallback)
	}
}

// The colour bar moved the body into an attachment, whose column is narrower
// than a plain message — a raw CodeCommit console URL wrapped onto three lines
// there and pushed the message past Slack's collapse threshold.
func TestLink_WrapsURLInSlackLinkSyntax(t *testing.T) {
	got := Link("https://example.com/a/b/c", "codecommit/my-repo")
	if want := "<https://example.com/a/b/c|codecommit/my-repo>"; got != want {
		t.Errorf("Link() = %q, want %q", got, want)
	}
}

// A provider with no web URL must not render an empty link target.
func TestLink_EmptyURLFallsBackToTheLabel(t *testing.T) {
	if got := Link("", "codecommit/my-repo"); got != "codecommit/my-repo" {
		t.Errorf("Link(\"\") = %q, want the bare label", got)
	}
}
