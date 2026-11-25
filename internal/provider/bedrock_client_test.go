package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testAWSKey = "AKIDEXAMPLE:wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"

// bedrockMock serves the Bedrock runtime the way the real API does
// (GET /foundation-models, POST /model/{id}/converse and
// /converse-stream with an Amazon event-stream body) and records
// every request, so tests can prove the request shape and the SigV4
// signature.
type bedrockMock struct {
	t *testing.T
	// streamBody, when set, is the event-stream body returned for
	// converse-stream.
	streamBody []byte
	// errStatus/errBody override for the next chat call (error cases).
	errStatus   int
	errBody     string
	errType     string
	lastPath    string
	lastHeaders http.Header
	lastBody    []byte
}

func (m *bedrockMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.lastPath = r.URL.Path
		m.lastHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		m.lastBody = body
		switch {
		case r.URL.Path == "/foundation-models" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"modelSummaries":[{"modelId":"anthropic.claude-3-haiku-20240307-v1:0"},{"modelId":"meta.llama3-70b-instruct-v1:0"}]}`))
		case strings.HasSuffix(r.URL.Path, "/converse-stream"):
			if m.errStatus != 0 {
				if m.errType != "" {
					w.Header().Set("x-amzn-ErrorType", m.errType)
				}
				w.WriteHeader(m.errStatus)
				_, _ = w.Write([]byte(m.errBody))
				return
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(m.streamBody)
		case strings.HasSuffix(r.URL.Path, "/converse"):
			if m.errStatus != 0 {
				if m.errType != "" {
					w.Header().Set("x-amzn-ErrorType", m.errType)
				}
				w.WriteHeader(m.errStatus)
				_, _ = w.Write([]byte(m.errBody))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"hi there"}]}},"stopReason":"end_turn","usage":{"inputTokens":12,"outputTokens":8,"totalTokens":20}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// bedrockEventFrame builds one event-stream frame for tests (same
// framing as wire.buildEventStreamFrame in the wire tests).
func bedrockEventFrame(t *testing.T, eventType string, payload any) []byte {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	var headers bytes.Buffer
	writeHeader := func(name, value string) {
		headers.WriteByte(byte(len(name)))
		headers.WriteString(name)
		headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(value)))
		headers.WriteString(value)
	}
	writeHeader(":event-type", eventType)
	writeHeader(":message-type", "event")
	writeHeader(":content-type", "application/json")
	totalLen := 12 + headers.Len() + len(body) + 4
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, uint32(totalLen))
	_ = binary.Write(&frame, binary.BigEndian, uint32(headers.Len()))
	prelude := frame.Bytes()
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(prelude))
	frame.Write(headers.Bytes())
	frame.Write(body)
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	return frame.Bytes()
}

func newBedrockMock(t *testing.T) (*httptest.Server, *bedrockMock) {
	t.Helper()
	m := &bedrockMock{t: t}
	var stream bytes.Buffer
	stream.Write(bedrockEventFrame(t, "messageStart", map[string]any{"role": "assistant"}))
	stream.Write(bedrockEventFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}}))
	stream.Write(bedrockEventFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "hi "}}))
	stream.Write(bedrockEventFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "there"}}))
	stream.Write(bedrockEventFrame(t, "contentBlockStop", map[string]any{"contentBlockIndex": 0}))
	stream.Write(bedrockEventFrame(t, "metadata", map[string]any{"usage": map[string]any{"inputTokens": 12, "outputTokens": 8, "totalTokens": 20}}))
	stream.Write(bedrockEventFrame(t, "messageStop", map[string]any{"stopReason": "end_turn"}))
	m.streamBody = stream.Bytes()
	return httptest.NewServer(m.handler()), m
}

func mustBedrockClient(t *testing.T, srv *httptest.Server) Client {
	t.Helper()
	c, err := NewClient(srv.URL, testAWSKey, WithWireFormat(WireFormatBedrock), WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient bedrock: %v", err)
	}
	if _, ok := c.(*bedrockClient); !ok {
		t.Fatalf("NewClient bedrock returned %T, want *bedrockClient", c)
	}
	return c
}

// assertSigV4 proves the mock saw a well-formed SigV4 Authorization
// header (and no Bearer <redacted> the Bearer is NOT this wire's auth).
func assertSigV4(t *testing.T, headers http.Header) {
	t.Helper()
	auth := headers.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
		t.Errorf("Authorization = %q, want SigV4 with AKIDEXAMPLE", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Errorf("Authorization signed headers = %q, want host;x-amz-content-sha256;x-amz-date", auth)
	}
	sig := auth[strings.LastIndex(auth, "Signature=")+len("Signature="):]
	if len(sig) != 64 {
		t.Errorf("signature length = %d, want 64 hex chars", len(sig))
	}
	if headers.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date header missing")
	}
	if headers.Get("X-Amz-Content-Sha256") == "" {
		t.Error("X-Amz-Content-Sha256 header missing")
	}
}

func TestBedrockClientChat(t *testing.T) {
	srv, mock := newBedrockMock(t)
	defer srv.Close()
	c := mustBedrockClient(t, srv)

	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:    "anthropic.claude-3-haiku-20240307-v1:0",
		Messages: []Message{{Role: "system", Content: "Be brief."}, {Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi there" || resp.FinishReason != "stop" {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Usage.TotalTokens != 20 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if mock.lastPath != "/model/anthropic.claude-3-haiku-20240307-v1:0/converse" {
		t.Errorf("path = %q", mock.lastPath)
	}
	assertSigV4(t, mock.lastHeaders)
	var decoded struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text,omitempty"`
			} `json:"content"`
		} `json:"messages"`
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(mock.lastBody, &decoded); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if len(decoded.Messages) != 1 || decoded.Messages[0].Role != "user" {
		t.Errorf("messages = %+v", decoded.Messages)
	}
	if len(decoded.System) != 1 || decoded.System[0].Text != "Be brief." {
		t.Errorf("system = %+v", decoded.System)
	}
}

func TestBedrockClientChatStream(t *testing.T) {
	srv, mock := newBedrockMock(t)
	defer srv.Close()
	c := mustBedrockClient(t, srv)

	var deltas []string
	resp, err := c.ChatStream(context.Background(), ChatRequest{
		Model:    "anthropic.claude-3-haiku-20240307-v1:0",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(s string) error {
		deltas = append(deltas, s)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Content != "hi there" {
		t.Errorf("Content = %q, want hi there", resp.Content)
	}
	if strings.Join(deltas, "") != "hi there" {
		t.Errorf("deltas = %q", deltas)
	}
	if !strings.HasSuffix(mock.lastPath, "/converse-stream") {
		t.Errorf("path = %q, want converse-stream", mock.lastPath)
	}
	assertSigV4(t, mock.lastHeaders)
}

func TestBedrockClientListModels(t *testing.T) {
	srv, mock := newBedrockMock(t)
	defer srv.Close()
	c := mustBedrockClient(t, srv)

	ids, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "anthropic.claude-3-haiku-20240307-v1:0" {
		t.Errorf("ids = %v", ids)
	}
	if mock.lastPath != "/foundation-models" {
		t.Errorf("path = %q, want /foundation-models", mock.lastPath)
	}
	assertSigV4(t, mock.lastHeaders)
	if err := c.TestKey(context.Background()); err != nil {
		t.Fatalf("TestKey: %v", err)
	}
}

func TestBedrockClientErrors(t *testing.T) {
	srv, mock := newBedrockMock(t)
	defer srv.Close()
	c := mustBedrockClient(t, srv)

	// ThrottlingException -> RateLimitError (failover).
	mock.errStatus = http.StatusBadRequest
	mock.errType = "ThrottlingException"
	mock.errBody = `{"message":"slow down"}`
	_, err := c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Errorf("throttling error = %T %v, want *RateLimitError", err, err)
	}
	// Plain 429 -> RateLimitError too.
	mock.errType = ""
	mock.errStatus = http.StatusTooManyRequests
	mock.errBody = `{"message":"too many requests"}`
	_, err = c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.As(err, &rlErr) {
		t.Errorf("429 error = %T %v, want *RateLimitError", err, err)
	}
	// 500 -> retryable UpstreamError.
	mock.errStatus = http.StatusInternalServerError
	mock.errBody = `{"message":"boom"}`
	_, err = c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	var upErr *UpstreamError
	if !errors.As(err, &upErr) || !upErr.Retryable {
		t.Errorf("500 error = %T %v, want retryable *UpstreamError", err, err)
	}
	// 400 validation -> non-retryable UpstreamError.
	mock.errStatus = http.StatusBadRequest
	mock.errBody = `{"message":"ValidationException: bad input"}`
	_, err = c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.As(err, &upErr) || upErr.Retryable {
		t.Errorf("400 error = %T %v, want non-retryable *UpstreamError", err, err)
	}
}

func TestBedrockClientBadCredentials(t *testing.T) {
	srv, _ := newBedrockMock(t)
	defer srv.Close()
	// A plain API key is not AWS credential material: the client
	// must refuse loudly with an exchange error, never sign with a
	// garbage key.
	c, err := NewClient(srv.URL, "not-aws-credentials", WithWireFormat(WireFormatBedrock), WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected exchange error, got nil")
	}
	if !strings.Contains(err.Error(), "exchange") {
		t.Errorf("error %q does not mention exchange", err.Error())
	}
}
