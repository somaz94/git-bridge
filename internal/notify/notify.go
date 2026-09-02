package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"git-bridge/internal/config"
)

const httpTimeout = 10 * time.Second

// levelPrefix maps each notification level to its prefix emoji. A level that is
// not in the map is treated as success (✅).
var levelPrefix = map[string]string{
	"success": "✅",
	"error":   "❌",
	"warning": "⚠️",
}

// levelColor maps each level to its attachment color bar. A level that is not in
// the map is treated as success.
//
// The emoji alone does not make the level stand out in a channel being scrolled.
// The color bar is seen first, on the left edge, before the body is read, so a
// failure mixed in among normal syncs can be found by skimming. The values are
// the same colors as Slack's good/warning/danger.
var levelColor = map[string]string{
	"success": "#2eb886",
	"error":   "#a30200",
	"warning": "#daa038",
}

// slackPayload is the JSON body sent to the Slack webhook.
// channel is dropped by omitempty when empty (the notifier's default is used).
//
// A color bar can only be expressed on an attachment, so the body goes inside one
// (Block Kit has no notion of color). Leaving the top-level text empty is
// deliberate — filling it renders the same content a second time above the
// attachment. The notification preview and clients with no color support use the
// attachment's fallback.
type slackPayload struct {
	Text        string            `json:"text,omitempty"`
	Channel     string            `json:"channel,omitempty"`
	Attachments []slackAttachment `json:"attachments,omitempty"`
}

// slackAttachment is the message body that carries the color bar.
//
// Without MrkdwnIn, the *bold* inside an attachment renders as literal asterisks —
// unlike the top-level text, an attachment is not markdown by default.
type slackAttachment struct {
	Color    string   `json:"color"`
	Fallback string   `json:"fallback"`
	Text     string   `json:"text"`
	MrkdwnIn []string `json:"mrkdwn_in"`
}

// Message represents a notification message.
//
// WebhookURL is an optional per-message override — when set, Slack.Send routes
// the message to that URL instead of the notifier's configured default. Used
// for per-repo Slack channel routing (e.g. git-bridge-test → TEST channel,
// other repos → prod channel).
type Message struct {
	Level      string // success, error, warning
	Title      string
	Body       string
	WebhookURL string // optional override; empty means the notifier's default is used
}

// Link wraps url in a Slack link that displays as label.
//
// The reason a raw URL must not go in as-is is width. Adding the color bar moved
// the body inside an attachment, and an attachment's body column is narrower than
// a plain message. A long address like a CodeCommit console URL breaks into three
// lines there, and the message grows long enough that Slack folds it behind a
// "show more". Folding it into a label brings it back to one line.
//
// SHAs and git commands stay plain text — those are copied, not clicked.
func Link(url, label string) string {
	if url == "" {
		return label
	}
	return fmt.Sprintf("<%s|%s>", url, label)
}

// Notifier sends notifications.
type Notifier interface {
	Send(msg Message)
}

// Slack sends notifications to Slack via webhook.
type Slack struct {
	webhookURL string
	channel    string
	client     *http.Client
}

func NewSlack(cfg config.SlackConfig) *Slack {
	return &Slack{
		webhookURL: cfg.WebhookURL,
		channel:    cfg.Channel,
		client:     &http.Client{Timeout: httpTimeout},
	}
}

func (s *Slack) Send(msg Message) {
	prefix := levelPrefix[msg.Level]
	if prefix == "" {
		prefix = "✅" // an unknown level is treated as success
	}
	color := levelColor[msg.Level]
	if color == "" {
		color = levelColor["success"] // the same fallback the emoji uses
	}

	text := fmt.Sprintf("%s *%s*\n%s", prefix, msg.Title, msg.Body)
	payload := slackPayload{
		Channel: s.channel,
		Attachments: []slackAttachment{{
			Color: color,
			// fallback is used for the notification preview, so it carries
			// the title only. Putting the whole body in floods a lock-screen
			// notification with SHAs and URLs.
			Fallback: fmt.Sprintf("%s %s", prefix, msg.Title),
			Text:     text,
			MrkdwnIn: []string{"text"},
		}},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("slack notification marshal failed", "error", err)
		return
	}
	url := s.webhookURL
	if msg.WebhookURL != "" {
		url = msg.WebhookURL
	}
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Error("slack notification failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		slog.Error("slack notification failed", "status", resp.StatusCode)
	}
}

// Noop is a no-op notifier (when Slack is not configured).
type Noop struct{}

func NewNoop() *Noop           { return &Noop{} }
func (n *Noop) Send(_ Message) {}
