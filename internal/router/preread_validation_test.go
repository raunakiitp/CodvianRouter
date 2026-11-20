package router

// 200-OK body validation in the streaming preread path (parity tracker
// Area 2 row "200-OK body validation predicate → failover";
// BURNDOWN Tier 1): a stream whose leading content already fails
// the predicate fails over pre-commit, exactly like a failed
// attempt — nothing reaches the client from the bad target.
import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func prereadValidationReq() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}
}

// TestPrereadErrorPayloadFailsOver is the row's core case: provider
// "a" answers 200 with a JSON error payload streamed as content;
// the preread must reject it before commit and the chain must
// serve provider "b" instead. The client sees only "from-b".
func TestPrereadErrorPayloadFailsOver(t *testing.T) {
	cfg := testConfig()
	eng := NewEngine(map[string]provider.Client{
		"a": streamClient([]string{`{"error": "boom"}`}, nil),
		"b": streamClient([]string{"from-b"}, nil),
	}, nil, cfg)
	var got strings.Builder
	resp, err := eng.ChatStream(context.Background(), prereadValidationReq(), func(delta string) error {
		got.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.String() != "from-b" {
		t.Fatalf("deltas = %q, want %q (bad body must fail over, never reach the client)", got.String(), "from-b")
	}
	if resp.Content != "from-b" {
		t.Fatalf("response content = %q, want %q", resp.Content, "from-b")
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %v, want 2 (bad target tried, then failover)", trace)
	}
	if trace.Attempts[0].Provider != "a" || trace.Attempts[1].Provider != "b" {
		t.Fatalf("trace providers = %v, want [a b]", trace.Attempts)
	}
}

// TestPrereadValidationDisabledAcceptsBadBody is the toggle-check
// for TestPrereadErrorPayloadFailsOver: with the predicate
// Disabled, the SAME bad body is delivered as-is — no failover.
// If the failover in the test above came from anything but the
// predicate, this test would fail over too.
func TestPrereadValidationDisabledAcceptsBadBody(t *testing.T) {
	cfg := testConfig()
	cfg.ResponseValidation = ResponseValidationConfig{Disabled: true}
	eng := NewEngine(map[string]provider.Client{
		"a": streamClient([]string{`{"error": "boom"}`}, nil),
		"b": streamClient([]string{"from-b"}, nil),
	}, nil, cfg)
	var got strings.Builder
	resp, err := eng.ChatStream(context.Background(), prereadValidationReq(), func(delta string) error {
		got.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.String() != `{"error": "boom"}` {
		t.Fatalf("deltas = %q, want the bad body delivered as-is when the predicate is off", got.String())
	}
	if resp.Content != `{"error": "boom"}` {
		t.Fatalf("response content = %q, want the bad body delivered as-is", resp.Content)
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %v, want 1 (no failover with the predicate off)", trace)
	}
}

// TestPrereadForbiddenSubstringFailsOver: an operator-configured
// forbidden substring present in the first delta fails over
// pre-commit, the same as the built-in error-payload check.
func TestPrereadForbiddenSubstringFailsOver(t *testing.T) {
	cfg := testConfig()
	cfg.ResponseValidation = ResponseValidationConfig{ForbiddenSubstrings: []string{"do not repeat"}}
	eng := NewEngine(map[string]provider.Client{
		"a": streamClient([]string{"do not repeat this"}, nil),
		"b": streamClient([]string{"from-b"}, nil),
	}, nil, cfg)
	var got strings.Builder
	if _, err := eng.ChatStream(context.Background(), prereadValidationReq(), func(delta string) error {
		got.WriteString(delta)
		return nil
	}); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.String() != "from-b" {
		t.Fatalf("deltas = %q, want %q (forbidden substring must fail over)", got.String(), "from-b")
	}
}

// TestPrereadValidBodyCommits is the regression guard: ordinary
// content commits on the first target, exactly once.
func TestPrereadValidBodyCommits(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{
		"a": streamClient([]string{"Hello ", "world"}, nil),
	}, nil, testConfig())
	var got strings.Builder
	resp, err := eng.ChatStream(context.Background(), prereadValidationReq(), func(delta string) error {
		got.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.String() != "Hello world" {
		t.Fatalf("deltas = %q, want %q", got.String(), "Hello world")
	}
	if resp.Content != "Hello world" {
		t.Fatalf("response content = %q, want %q", resp.Content, "Hello world")
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %v, want 1", trace)
	}
}

// TestPrereadToolCallExemptsJSONCheck: a tool-call-carrying chunk
// whose text looks like an error payload still commits — the tool
// calls ARE content, so the JSON check is exempt, exactly as in
// the non-streaming predicate.
func TestPrereadToolCallExemptsJSONCheck(t *testing.T) {
	b := prereadChunks([]provider.StreamChunk{
		{Content: `{"error": "boom"}`, ToolCalls: []provider.ChunkToolCall{
			{Index: 0, ID: "call_1", Name: "get_weather", Arguments: `{}`},
		}},
		{FinishReason: "tool_calls"},
	}, nil)
	eng := NewEngine(map[string]provider.Client{"b": b}, nil, testConfig())
	var got strings.Builder
	resp, err := eng.ChatStream(context.Background(), prereadValidationReq(), func(delta string) error {
		got.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.String() != `{"error": "boom"}` {
		t.Fatalf("deltas = %q, want the tool-call text delivered (JSON check exempt)", got.String())
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("resp tool calls = %d, want 1", len(resp.ToolCalls))
	}
}

// TestValidatePrereadPrefix pins the prefix-sound subset: what runs
// pre-commit, and what is deliberately skipped there.
func TestValidatePrereadPrefix(t *testing.T) {
	zero := ResponseValidationConfig{}
	cases := []struct {
		name         string
		text         string
		hasToolCalls bool
		cfg          ResponseValidationConfig
		wantErr      bool
	}{
		{"error payload", `{"error": "boom"}`, false, zero, true},
		{"error object payload", `{"error": {"message": "nope"}}`, false, zero, true},
		{"empty choices", `{"choices": []}`, false, zero, true},
		{"plain object is fine", `{"a": 1}`, false, zero, false},
		{"plain text is fine", "hello world", false, zero, false},
		{"incomplete JSON prefix cannot be judged", `{"error": `, false, zero, false},
		{"tool calls exempt the JSON check", `{"error": "boom"}`, true, zero, false},
		{"disabled turns everything off", `{"error": "boom"}`, false, ResponseValidationConfig{Disabled: true}, false},
		{"forbidden substring", "say nope now", false, ResponseValidationConfig{ForbiddenSubstrings: []string{"nope"}}, true},
		{"forbidden absent passes", "say hello now", false, ResponseValidationConfig{ForbiddenSubstrings: []string{"nope"}}, false},
		// Deliberately skipped pre-commit (not prefix-sound):
		// a prefix can never prove a required substring is
		// missing, or the whole will stay short.
		{"required substring NOT checked on a prefix", "hello", false, ResponseValidationConfig{RequiredSubstrings: []string{"goodbye"}}, false},
		{"min length NOT checked on a prefix", "hi", false, ResponseValidationConfig{MinContentLength: 10}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePrereadPrefix(tc.text, tc.hasToolCalls, tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatalf("validatePrereadPrefix(%q) = nil, want an error", tc.text)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validatePrereadPrefix(%q) = %v, want nil", tc.text, err)
			}
			if tc.wantErr {
				var ue *provider.UpstreamError
				if !errors.As(err, &ue) {
					t.Fatalf("error = %T, want *provider.UpstreamError", err)
				}
				if ue.StatusCode != 0 || !ue.Retryable {
					t.Fatalf("UpstreamError = %+v, want StatusCode 0, Retryable true", ue)
				}
			}
		})
	}
}
