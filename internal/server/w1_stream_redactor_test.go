// W1 close-out tests (M1.7): the StreamRedactor must be wired into
// both stream paths, so a PEM private-key block split across SSE
// chunks is fully redacted in the streamed output — no body line
// and no footer may leak, in any chunking.
package server_test

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/server"
)

// w1SplitPEMChunks is one PEM block split mid-header, mid-body and
// mid-footer across six chunks, with clean text on both sides.
var w1SplitPEMChunks = []provider.StreamChunk{
	{Content: "here is a key:\n"},
	{Content: "-----BEG"},
	{Content: "IN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAK"},
	{Content: "++testbase64line\nmorebase64data\n"},
	{Content: "-----END RSA PRIVATE KEY-----\n"},
	{Content: " bye"},
	{FinishReason: "stop"},
}

func TestW1ChatStreamRedactsSplitPEM(t *testing.T) {
	eng := &extChunkEngine{
		resp:   provider.ChatResponse{Content: "here is a key:\n[REDACTED:SECRET] bye", FinishReason: "stop"},
		chunks: w1SplitPEMChunks,
	}
	srv := server.New(eng, server.WithStaticKeys("test-key-123"), server.WithLogger(log.New(io.Discard, "", 0)))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer test-key-123")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leak := range []string{"MIIBOgIBAAJBAK", "testbase64line", "morebase64data", "PRIVATE KEY"} {
		if strings.Contains(body, leak) {
			t.Errorf("streamed body leaks PEM material %q:\n%s", leak, body)
		}
	}
	if !strings.Contains(body, "[REDACTED:SECRET]") {
		t.Errorf("streamed body has no [REDACTED:SECRET] placeholder:\n%s", body)
	}
	if !strings.Contains(body, "here is a key:") || !strings.Contains(body, "bye") {
		t.Errorf("streamed body lost the clean text around the block:\n%s", body)
	}
}

func TestW1MessagesStreamRedactsSplitPEM(t *testing.T) {
	eng := &extChunkEngine{
		resp:   provider.ChatResponse{Content: "here is a key:\n[REDACTED:SECRET] bye", Model: "m", FinishReason: "stop"},
		chunks: w1SplitPEMChunks,
	}
	srv := server.New(eng, server.WithStaticKeys("test-key-123"), server.WithLogger(log.New(io.Discard, "", 0)))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer test-key-123")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leak := range []string{"MIIBOgIBAAJBAK", "testbase64line", "morebase64data", "PRIVATE KEY"} {
		if strings.Contains(body, leak) {
			t.Errorf("streamed body leaks PEM material %q:\n%s", leak, body)
		}
	}
	if !strings.Contains(body, "[REDACTED:SECRET]") {
		t.Errorf("streamed body has no [REDACTED:SECRET] placeholder:\n%s", body)
	}
}
