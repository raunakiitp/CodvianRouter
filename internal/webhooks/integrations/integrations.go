// Package integrations builds provider-specific chat payloads for the
// three chat platforms Codvian Router targets — Slack, Discord and
// Telegram. Each transformer turns the gateway's native
// {event, data} shape into the platform's own request body:
//
//   - Slack    — Block Kit (a `section` block carrying the event +
//     summary, and a `context` block carrying the timestamp/source);
//   - Discord  — a single embed with a per-event colour and one field
//     per payload key;
//   - Telegram — the Bot API `sendMessage` body (chat_id + text +
//     parse_mode=MarkdownV2), with every MarkdownV2 metacharacter
//     escaped so a payload can never break or inject formatting.
//
// These three platforms do not verify an HMAC signature, so chat
// deliveries are NOT signed: the generic webhook path keeps its
// X-Codvian-Signature-256 header, chat kinds do not. The delivery
// machinery (retries, backoff, SSRF-safe client) is shared with the
// generic path — only the body differs.
//
// The Telegram bot token and the destination chat_id both live in the
// endpoint URL (the Bot API URL form,
// https://api.telegram.org/bot<token>/sendMessage?chat_id=<id>): the
// token is validated before delivery and the chat_id is read back out
// of the URL. A sealed token may instead be supplied through endpoint
// metadata and decrypted by the caller before delivery.
package integrations

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kind is a chat integration kind. The zero value ("") is not a chat
// kind: it is the gateway's generic signed-JSON webhook.
type Kind string

const (
	KindSlack    Kind = "slack"
	KindDiscord  Kind = "discord"
	KindTelegram Kind = "telegram"
)

// ParseKind maps a config string to a chat Kind. It returns ok=false
// for the empty string and any unrecognised value (both mean "generic
// webhook", which the caller handles — an unknown NON-empty value is
// rejected at config validation, not here).
func ParseKind(s string) (Kind, bool) {
	switch Kind(strings.ToLower(strings.TrimSpace(s))) {
	case KindSlack:
		return KindSlack, true
	case KindDiscord:
		return KindDiscord, true
	case KindTelegram:
		return KindTelegram, true
	}
	return "", false
}

// Valid reports whether k is one of the three chat kinds.
func (k Kind) Valid() bool {
	_, ok := ParseKind(string(k))
	return ok
}

// Discord embed colours (decimal RGB).
const (
	// DiscordColorCompleted — green, for a *.completed event.
	DiscordColorCompleted = 0x2ecc71
	// DiscordColorFailed — red, for a *.failed event.
	DiscordColorFailed = 0xe74c3c
	// DiscordColorQuota — yellow, for a quota/budget event.
	DiscordColorQuota = 0xf1c40f
	// DiscordColorTest — purple, for test.ping.
	DiscordColorTest = 0x9b59b6
	// DiscordColorDefault — indigo, for everything else.
	DiscordColorDefault = 0x4b0082
)

// DiscordColor returns the embed colour for an event: completed green,
// failed red, quota yellow, test.ping purple, otherwise indigo.
func DiscordColor(event string) int {
	switch {
	case event == "test.ping":
		return DiscordColorTest
	case event == "quota.exceeded" || event == "budget.warning":
		return DiscordColorQuota
	case strings.HasSuffix(event, ".completed"):
		return DiscordColorCompleted
	case strings.HasSuffix(event, ".failed"):
		return DiscordColorFailed
	default:
		return DiscordColorDefault
	}
}

// Summary renders a payload into a single deterministic line
// ("key=value key=value", keys sorted) so every platform shows the
// same content. It never invents data: an empty payload yields "".
func Summary(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, payload[k]))
	}
	return strings.Join(parts, " ")
}

// Transform builds the platform-specific request body for event +
// payload at time ts. chatID is required for Telegram (read from the
// endpoint URL by the caller); it is ignored for Slack and Discord
// (whose destination is entirely in their webhook URL).
func Transform(k Kind, event string, payload map[string]any, chatID string, ts time.Time) ([]byte, error) {
	switch k {
	case KindSlack:
		return json.Marshal(slackBody(event, payload, ts))
	case KindDiscord:
		return json.Marshal(discordBody(event, payload, ts))
	case KindTelegram:
		if strings.TrimSpace(chatID) == "" {
			return nil, fmt.Errorf("integrations: telegram requires a chat_id in the endpoint URL")
		}
		return json.Marshal(telegramBody(event, payload, chatID))
	default:
		return nil, fmt.Errorf("integrations: %q is not a chat kind", k)
	}
}

// ---- Slack (Block Kit) ----

type slackBodyT struct {
	Blocks []slackBlock `json:"blocks"`
}

type slackBlock struct {
	Type     string      `json:"type"`
	Text     *slackText  `json:"text,omitempty"`
	Elements []slackText `json:"elements,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func slackBody(event string, payload map[string]any, ts time.Time) slackBodyT {
	text := "*" + event + "*"
	if s := Summary(payload); s != "" {
		text += "\n" + s
	}
	return slackBodyT{Blocks: []slackBlock{
		{Type: "section", Text: &slackText{Type: "mrkdwn", Text: text}},
		{Type: "context", Elements: []slackText{
			{Type: "mrkdwn", Text: ts.UTC().Format(time.RFC3339) + " • codvian-router"},
		}},
	}}
}

// ---- Discord (embeds) ----

type discordBodyT struct {
	Embeds []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Timestamp   string         `json:"timestamp,omitempty"`
	Fields      []discordField `json:"fields,omitempty"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

func discordBody(event string, payload map[string]any, ts time.Time) discordBodyT {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fields := make([]discordField, 0, len(keys))
	for _, k := range keys {
		fields = append(fields, discordField{Name: k, Value: fmt.Sprintf("%v", payload[k]), Inline: true})
	}
	return discordBodyT{Embeds: []discordEmbed{{
		Title:       event,
		Description: Summary(payload),
		Color:       DiscordColor(event),
		Timestamp:   ts.UTC().Format(time.RFC3339),
		Fields:      fields,
	}}}
}

// ---- Telegram (Bot API sendMessage) ----

type telegramBodyT struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

func telegramBody(event string, payload map[string]any, chatID string) telegramBodyT {
	text := "*" + EscapeTelegramMarkdown(event) + "*"
	if s := Summary(payload); s != "" {
		text += "\n" + EscapeTelegramMarkdown(s)
	}
	return telegramBodyT{ChatID: chatID, Text: text, ParseMode: "MarkdownV2"}
}

// telegramTokenRe is the Bot API token shape: a numeric bot id, a
// colon, then a 35+ char [A-Za-z0-9_-] secret.
var telegramTokenRe = regexp.MustCompile(`^\d+:[A-Za-z0-9_-]{35,}$`)

// ValidateTelegramToken checks a Bot API token against the documented
// shape. It never calls the network.
func ValidateTelegramToken(token string) error {
	if !telegramTokenRe.MatchString(token) {
		return fmt.Errorf("integrations: telegram bot token is malformed (want <digits>:<35+ of [A-Za-z0-9_-]>)")
	}
	return nil
}

// telegramMarkdownV2Special is the set of characters the Bot API
// requires to be backslash-escaped in MarkdownV2 text.
const telegramMarkdownV2Special = `_*[]()~` + "`" + `>#+-=|{}.!\`

// EscapeTelegramMarkdown escapes every MarkdownV2 metacharacter and
// the backslash itself, so arbitrary payload text can never break or
// inject formatting. A backslash is escaped first so the escapes added
// below are not themselves escaped.
func EscapeTelegramMarkdown(s string) string {
	var b strings.Builder
	b.Grow(len(s) * 2)
	for _, r := range s {
		if strings.ContainsRune(telegramMarkdownV2Special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ParseTelegramURL extracts the bot token and chat_id from a Bot API
// endpoint URL of the form
// https://api.telegram.org/bot<token>/sendMessage?chat_id=<id>.
// It validates the token shape and rejects a URL with no token, no
// chat_id, or a non-telegram host.
func ParseTelegramURL(raw string) (token, chatID string, err error) {
	u, perr := url.Parse(strings.TrimSpace(raw))
	if perr != nil || u == nil {
		return "", "", fmt.Errorf("integrations: telegram url %q is not parseable", raw)
	}
	if !strings.EqualFold(u.Hostname(), "api.telegram.org") {
		return "", "", fmt.Errorf("integrations: telegram url host %q is not api.telegram.org", u.Hostname())
	}
	// Path: /bot<token>/sendMessage
	p := strings.Trim(u.Path, "/")
	parts := strings.Split(p, "/")
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "bot") {
		return "", "", fmt.Errorf("integrations: telegram url path %q is not /bot<token>/<method>", u.Path)
	}
	token = strings.TrimPrefix(parts[0], "bot")
	if err := ValidateTelegramToken(token); err != nil {
		return "", "", err
	}
	chatID = u.Query().Get("chat_id")
	if strings.TrimSpace(chatID) == "" {
		return "", "", fmt.Errorf("integrations: telegram url %q carries no chat_id query parameter", raw)
	}
	return token, chatID, nil
}

// ChatIDFromURL extracts the chat_id query parameter from a stored
// Telegram endpoint URL WITHOUT checking the host. It is the
// delivery-time read: the endpoint URL was already validated when it
// was registered or loaded, and delivery must POST to exactly that
// stored URL (a host re-check here would also make the delivery path
// untestable against a local mock endpoint).
func ChatIDFromURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return "", fmt.Errorf("integrations: telegram url %q is not parseable", raw)
	}
	cid := u.Query().Get("chat_id")
	if strings.TrimSpace(cid) == "" {
		return "", fmt.Errorf("integrations: telegram url %q carries no chat_id query parameter", raw)
	}
	return cid, nil
}

// TelegramChatID returns the chat_id query parameter of a Bot API
// endpoint URL, validating that the host is api.telegram.org. It is
// used when the bot token comes from metadata rather than the URL: the
// chat_id always lives in the URL (the delivery destination), while
// the token may be sealed in endpoint metadata.
func TelegramChatID(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return "", fmt.Errorf("integrations: telegram url %q is not parseable", raw)
	}
	if !strings.EqualFold(u.Hostname(), "api.telegram.org") {
		return "", fmt.Errorf("integrations: telegram url host %q is not api.telegram.org", u.Hostname())
	}
	cid := u.Query().Get("chat_id")
	if strings.TrimSpace(cid) == "" {
		return "", fmt.Errorf("integrations: telegram url %q carries no chat_id query parameter", raw)
	}
	return cid, nil
}

// BuildTelegramURL is the inverse of ParseTelegramURL: it builds the
// Bot API endpoint URL for a token + chat_id, validating the token.
// The token is embedded in the URL path (never logged, never returned
// through the API).
func BuildTelegramURL(token, chatID string) (string, error) {
	if err := ValidateTelegramToken(token); err != nil {
		return "", err
	}
	if strings.TrimSpace(chatID) == "" {
		return "", fmt.Errorf("integrations: telegram chat_id is required")
	}
	return "https://api.telegram.org/bot" + token + "/sendMessage?chat_id=" + url.QueryEscape(chatID), nil
}
