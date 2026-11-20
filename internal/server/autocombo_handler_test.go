package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func strptr(s string) *string { return &s }

func TestAutoComboHandlerReturnsDocumentedShape(t *testing.T) {
	in := AutoComboCandidatesInput{
		Channel: "coding",
		Pool: []AutoComboCandidateInput{
			{Provider: "cc", ConnectionID: "conn-a", Model: "cc/claude-opus", BreakerState: "CLOSED"},
			{Provider: "openai", ConnectionID: "conn-b", Model: "openai/gpt-5", BreakerState: "OPEN"},
			{Provider: "glm", ConnectionID: "conn-c", Model: "glm/glm-free", BreakerState: "CLOSED", ConnectionCooldown: true},
			{Provider: "deepseek", ConnectionID: "conn-d", Model: "deepseek/deepseek-chat", BreakerState: "HALF_OPEN", ModelLocked: true, FreeAccessExclusion: strptr("paid-model")},
		},
		ExcludedConnectionIDs: map[string]bool{"conn-c": true},
	}
	res, err := ListAutoComboCandidates(in)
	if err != nil {
		t.Fatalf("ListAutoComboCandidates: %v", err)
	}
	if res.Channel != "auto/coding" {
		t.Fatalf("channel = %q, want auto/coding", res.Channel)
	}
	if len(res.Candidates) != 4 {
		t.Fatalf("candidates = %d rows, want 4 (blocked rows are listed, never dropped)", len(res.Candidates))
	}
	// Row 0: healthy, not excluded.
	if c := res.Candidates[0]; !c.Reachable || c.Excluded || c.BreakerState != "CLOSED" {
		t.Fatalf("row 0 = %+v, want reachable=true excluded=false breakerState=CLOSED", c)
	}
	// Row 1: breaker OPEN -> unreachable, but still listed.
	if c := res.Candidates[1]; c.Reachable || c.BreakerState != "OPEN" {
		t.Fatalf("row 1 = %+v, want reachable=false breakerState=OPEN", c)
	}
	// Row 2: cooldown + excluded by this API key.
	if c := res.Candidates[2]; c.Reachable || !c.ConnectionCooldown || !c.Excluded {
		t.Fatalf("row 2 = %+v, want reachable=false connectionCooldown=true excluded=true", c)
	}
	// Row 3: HALF_OPEN can execute, but the model lockout blocks it.
	if c := res.Candidates[3]; c.Reachable || !c.ModelLocked || c.BreakerState != "HALF_OPEN" {
		t.Fatalf("row 3 = %+v, want reachable=false modelLocked=true breakerState=HALF_OPEN", c)
	}
	if got := res.Candidates[3].FreeAccessExclusion; got == nil || *got != "paid-model" {
		t.Fatalf("row 3 freeAccessExclusion = %v, want paid-model", got)
	}
	// The JSON shape is the documented one: channel + candidates,
	// camelCase fields, freeAccessExclusion null when unset.
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, want := range []string{`"channel":"auto/coding"`, `"provider":"cc"`, `"connectionId":"conn-a"`, `"modelStr":"cc/claude-opus"`, `"reachable":true`, `"breakerState":"CLOSED"`, `"freeAccessExclusion":null`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("JSON %s lacks %s", body, want)
		}
	}
}

func TestAutoComboHandlerBaseChannel(t *testing.T) {
	res, err := ListAutoComboCandidates(AutoComboCandidatesInput{Channel: "auto", Pool: []AutoComboCandidateInput{
		{Provider: "cc", ConnectionID: "conn-a", Model: "cc/claude-opus"},
	}})
	if err != nil {
		t.Fatalf("ListAutoComboCandidates: %v", err)
	}
	if res.Channel != "auto" {
		t.Fatalf("channel = %q, want auto", res.Channel)
	}
	// Empty breaker state defaults to CLOSED/reachable.
	if !res.Candidates[0].Reachable || res.Candidates[0].BreakerState != "CLOSED" {
		t.Fatalf("row 0 = %+v, want reachable with default CLOSED breaker", res.Candidates[0])
	}
}

func TestAutoComboHandlerAllowlistExpansion(t *testing.T) {
	res, err := ListAutoComboCandidates(AutoComboCandidatesInput{
		Channel: "fast",
		Pool: []AutoComboCandidateInput{
			{Provider: "cc", AllowedConnectionIDs: []string{"conn-a", "conn-b"}, Model: "cc/claude-opus"},
			{Provider: "gone", Model: "gone/model"}, // no connection, no allowlist -> no row
		},
	})
	if err != nil {
		t.Fatalf("ListAutoComboCandidates: %v", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("candidates = %d rows, want 2 (one per allowlisted connection)", len(res.Candidates))
	}
	if res.Candidates[0].ConnectionID != "conn-a" || res.Candidates[1].ConnectionID != "conn-b" {
		t.Fatalf("candidates = %v, want conn-a then conn-b", res.Candidates)
	}
}

func TestAutoComboHandlerEmptyPoolIsEmptyArray(t *testing.T) {
	res, err := ListAutoComboCandidates(AutoComboCandidatesInput{Channel: "cheap"})
	if err != nil {
		t.Fatalf("ListAutoComboCandidates: %v", err)
	}
	if res.Candidates == nil {
		t.Fatal("candidates is nil; want an empty array, not null")
	}
	body, _ := json.Marshal(res)
	if !strings.Contains(string(body), `"candidates":[]`) {
		t.Fatalf("JSON %s lacks \"candidates\":[]", body)
	}
}

func TestAutoComboHandlerUnknownChannelErrors(t *testing.T) {
	_, err := ListAutoComboCandidates(AutoComboCandidatesInput{Channel: "bogus"})
	if err == nil {
		t.Fatal("unknown channel: expected error, got none")
	}
	if !errors.Is(err, ErrUnknownAutoChannel) {
		t.Fatalf("error = %v, want it to wrap ErrUnknownAutoChannel (route layer maps it to 404)", err)
	}
	if _, err := ListAutoComboCandidates(AutoComboCandidatesInput{Channel: "coding:bogus"}); err == nil {
		t.Fatal("unknown category:tier channel: expected error, got none")
	}
}

func TestAutoComboHandlerInvalidChannelErrors(t *testing.T) {
	for _, ch := range []string{"", "has space", "bad/slash", "bad.dot", strings.Repeat("a", 129)} {
		if _, err := ListAutoComboCandidates(AutoComboCandidatesInput{Channel: ch}); err == nil {
			t.Errorf("channel %q: expected error, got none", ch)
		} else if !errors.Is(err, ErrInvalidAutoChannel) {
			t.Errorf("channel %q: error = %v, want it to wrap ErrInvalidAutoChannel (route layer maps it to 400)", ch, err)
		}
	}
}
