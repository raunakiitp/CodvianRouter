package webhooks

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// chatRecorder captures the raw body + signature header of one delivery.
type chatRecorder struct {
	mu   sync.Mutex
	body []byte
	sig  string
	ch   chan struct{}
}

func (r *chatRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.body = b
	r.sig = req.Header.Get("X-Codvian-Signature-256")
	r.mu.Unlock()
	select {
	case r.ch <- struct{}{}:
	default:
	}
	w.WriteHeader(http.StatusOK)
}

func (r *chatRecorder) wait(t *testing.T) map[string]any {
	t.Helper()
	select {
	case <-r.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no chat delivery arrived within 5s")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sig != "" {
		t.Errorf("chat delivery carried X-Codvian-Signature-256 = %q, want none (chat kinds are not HMAC-signed)", r.sig)
	}
	var env map[string]any
	if err := json.Unmarshal(r.body, &env); err != nil {
		t.Fatalf("chat delivery body is not JSON: %v (%s)", err, r.body)
	}
	return env
}

func chatTestDispatcher(t *testing.T, ep Endpoint, path string) (*Dispatcher, *chatRecorder) {
	t.Helper()
	recv := &chatRecorder{ch: make(chan struct{}, 4)}
	srv := httptest.NewServer(recv)
	t.Cleanup(srv.Close)
	ep.URL = srv.URL + path
	return New([]Endpoint{ep}, srv.Client(), nil), recv
}

func TestDispatcherDeliversSlackBlocksPayload(t *testing.T) {
	d, recv := chatTestDispatcher(t, Endpoint{ID: "slack", Kind: "slack", Events: []string{RequestCompletedEvent}}, "/")
	d.Notify(RequestCompletedEvent, map[string]any{"model": "gpt-4o", "latency_ms": 42})
	env := recv.wait(t)
	blocks, ok := env["blocks"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("slack body blocks = %v, want 2 (section + context)", env["blocks"])
	}
	if blocks[0].(map[string]any)["type"] != "section" {
		t.Errorf("block[0].type = %v, want section", blocks[0].(map[string]any)["type"])
	}
}

func TestDispatcherDeliversDiscordEmbedWithColor(t *testing.T) {
	d, recv := chatTestDispatcher(t, Endpoint{ID: "discord", Kind: "discord", Events: []string{RequestFailedEvent}}, "/")
	d.Notify(RequestFailedEvent, map[string]any{"provider": "openai"})
	env := recv.wait(t)
	embeds, ok := env["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("discord body embeds = %v, want 1", env["embeds"])
	}
	emb := embeds[0].(map[string]any)
	if emb["title"] != RequestFailedEvent {
		t.Errorf("embed.title = %v, want %s", emb["title"], RequestFailedEvent)
	}
	if int(emb["color"].(float64)) != 0xe74c3c {
		t.Errorf("embed.color = %v, want failed-red 0xe74c3c", emb["color"])
	}
}

func TestDispatcherDeliversTelegramSendMessage(t *testing.T) {
	const tok = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"
	d, recv := chatTestDispatcher(t, Endpoint{ID: "tg", Kind: "telegram", Events: []string{QuotaExceededEvent}},
		"/bot"+tok+"/sendMessage?chat_id=-1001234")
	d.Notify(QuotaExceededEvent, map[string]any{"reason": "blackout"})
	env := recv.wait(t)
	if env["chat_id"] != "-1001234" {
		t.Errorf("chat_id = %v, want -1001234 (read from the URL)", env["chat_id"])
	}
	if env["parse_mode"] != "MarkdownV2" {
		t.Errorf("parse_mode = %v, want MarkdownV2", env["parse_mode"])
	}
	if _, ok := env["text"].(string); !ok {
		t.Errorf("text = %v, want a string", env["text"])
	}
}

// The generic (non-chat) path must still be HMAC-signed.
func TestDispatcherGenericPathStillSigned(t *testing.T) {
	recv := &chatRecorder{ch: make(chan struct{}, 1)}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	d := New([]Endpoint{{ID: "generic", URL: srv.URL, Events: []string{QuotaExceededEvent}, Secret: "s3cr3t"}}, srv.Client(), nil)
	d.Notify(QuotaExceededEvent, map[string]any{"reason": "budget"})
	select {
	case <-recv.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("generic delivery did not arrive")
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if recv.sig == "" {
		t.Error("generic delivery carried no signature, want HMAC")
	}
	var env map[string]any
	if err := json.Unmarshal(recv.body, &env); err != nil {
		t.Fatalf("generic body not JSON: %v", err)
	}
	if env["event"] != QuotaExceededEvent {
		t.Errorf("generic envelope event = %v, want %s", env["event"], QuotaExceededEvent)
	}
}
