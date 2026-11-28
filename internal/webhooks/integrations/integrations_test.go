package integrations

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postToMock POSTs body to a mock endpoint and returns the decoded JSON
// the endpoint received — proving the payload shape survives an actual
// HTTP delivery, not just the in-memory struct.
func postToMock(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST to mock endpoint: %v", err)
	}
	resp.Body.Close()
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("mock endpoint received non-JSON: %v (%s)", err, got)
	}
	return decoded
}

func TestSlackPayloadShape(t *testing.T) {
	ts := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	body, err := Transform(KindSlack, "request.completed", map[string]any{"model": "gpt-4o", "latency_ms": 120}, "", ts)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := postToMock(t, body)
	blocks, ok := got["blocks"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("blocks = %v, want exactly 2 (section + context)", got["blocks"])
	}
	section := blocks[0].(map[string]any)
	if section["type"] != "section" {
		t.Errorf("block[0].type = %v, want section", section["type"])
	}
	text := section["text"].(map[string]any)
	if text["type"] != "mrkdwn" {
		t.Errorf("section text type = %v, want mrkdwn", text["type"])
	}
	st := text["text"].(string)
	if !strings.Contains(st, "request.completed") || !strings.Contains(st, "model=gpt-4o") || !strings.Contains(st, "latency_ms=120") {
		t.Errorf("section text = %q, want the event and the summary", st)
	}
	ctx := blocks[1].(map[string]any)
	if ctx["type"] != "context" {
		t.Errorf("block[1].type = %v, want context", ctx["type"])
	}
	elems := ctx["elements"].([]any)
	ctext := elems[0].(map[string]any)["text"].(string)
	if !strings.Contains(ctext, "2026-11-28T10:00:00Z") || !strings.Contains(ctext, "codvian-router") {
		t.Errorf("context text = %q, want the timestamp and source", ctext)
	}
}

func TestDiscordPayloadShapeAndColors(t *testing.T) {
	cases := []struct {
		event string
		want  int
	}{
		{"request.completed", DiscordColorCompleted},
		{"eval.completed", DiscordColorCompleted},
		{"request.failed", DiscordColorFailed},
		{"eval.run.failed", DiscordColorFailed},
		{"quota.exceeded", DiscordColorQuota},
		{"budget.warning", DiscordColorQuota},
		{"test.ping", DiscordColorTest},
		{"provider.degraded", DiscordColorDefault},
		{"eval.regression", DiscordColorDefault},
	}
	for _, tc := range cases {
		if got := DiscordColor(tc.event); got != tc.want {
			t.Errorf("DiscordColor(%q) = %#x, want %#x", tc.event, got, tc.want)
		}
	}
	ts := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	body, err := Transform(KindDiscord, "request.failed", map[string]any{"provider": "openai", "error": "boom"}, "", ts)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := postToMock(t, body)
	embeds, ok := got["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("embeds = %v, want exactly 1", got["embeds"])
	}
	emb := embeds[0].(map[string]any)
	if emb["title"] != "request.failed" {
		t.Errorf("embed.title = %v, want request.failed", emb["title"])
	}
	if int(emb["color"].(float64)) != DiscordColorFailed {
		t.Errorf("embed.color = %v, want %#x (failed red)", emb["color"], DiscordColorFailed)
	}
	fields, ok := emb["fields"].([]any)
	if !ok || len(fields) != 2 {
		t.Fatalf("embed.fields = %v, want one per payload key", emb["fields"])
	}
	if emb["timestamp"] != "2026-11-28T10:00:00Z" {
		t.Errorf("embed.timestamp = %v, want the fixed ts", emb["timestamp"])
	}
}

func TestTelegramPayloadShape(t *testing.T) {
	tok := "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij" // 9 digits : 36 chars
	if err := ValidateTelegramToken(tok); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	rawURL, err := BuildTelegramURL(tok, "-1001234567890")
	if err != nil {
		t.Fatalf("BuildTelegramURL: %v", err)
	}
	gotTok, gotChat, err := ParseTelegramURL(rawURL)
	if err != nil {
		t.Fatalf("ParseTelegramURL: %v", err)
	}
	if gotTok != tok || gotChat != "-1001234567890" {
		t.Fatalf("round trip = (%q,%q), want (%q,-1001234567890)", gotTok, gotChat, tok)
	}

	body, err := Transform(KindTelegram, "budget.warning", map[string]any{"pct": "*90*", "budget_id": "b1"}, gotChat, time.Now())
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	decoded := postToMock(t, body)
	if decoded["chat_id"] != "-1001234567890" {
		t.Errorf("chat_id = %v, want the URL chat_id", decoded["chat_id"])
	}
	if decoded["parse_mode"] != "MarkdownV2" {
		t.Errorf("parse_mode = %v, want MarkdownV2", decoded["parse_mode"])
	}
	text, _ := decoded["text"].(string)
	// The event's dots and the payload's asterisks must be escaped.
	if !strings.Contains(text, `budget\.warning`) {
		t.Errorf("text = %q, want the event dots escaped", text)
	}
	if strings.Contains(text, "*90*") || !strings.Contains(text, `\*90\*`) {
		t.Errorf("text = %q, want the payload asterisks escaped", text)
	}
}

func TestTelegramTokenValidation(t *testing.T) {
	good := "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"
	if err := ValidateTelegramToken(good); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
	bad := []string{
		"",
		"not-a-token",
		"123456789",                             // no colon
		":ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", // no digits
		"123456789:short",                       // secret too short
		"abc:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", // non-numeric id
		"123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabc!", // invalid char
	}
	for _, b := range bad {
		if err := ValidateTelegramToken(b); err == nil {
			t.Errorf("ValidateTelegramToken(%q) = nil, want an error", b)
		}
	}
}

func TestTelegramURLRejectsNoChatID(t *testing.T) {
	tok := "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"
	if _, _, err := ParseTelegramURL("https://api.telegram.org/bot" + tok + "/sendMessage"); err == nil {
		t.Error("ParseTelegramURL without chat_id = nil error, want rejection")
	}
	if _, _, err := ParseTelegramURL("https://example.com/bot" + tok + "/sendMessage?chat_id=1"); err == nil {
		t.Error("ParseTelegramURL with a non-telegram host = nil error, want rejection")
	}
	if _, err := BuildTelegramURL("bad-token", "1"); err == nil {
		t.Error("BuildTelegramURL with a bad token = nil error, want rejection")
	}
}

func TestTransformRejectsUnknownKind(t *testing.T) {
	if _, err := Transform(Kind("irc"), "x", nil, "", time.Now()); err == nil {
		t.Error("Transform(unknown kind) = nil error, want rejection")
	}
	if _, err := Transform(KindTelegram, "x", nil, "", time.Now()); err == nil {
		t.Error("Transform(telegram, no chat_id) = nil error, want rejection")
	}
}
